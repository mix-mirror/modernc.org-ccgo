# Phase 2: `--compact-data` Flag — Design Document

## Goal

Emit large const scalar/POD arrays as binary files embedded via `//go:embed`,
instead of inline Go literals.  This reduces generated Go source size by ~90%
for data-heavy grammars (e.g., tree-sitter's LR parse tables) and
dramatically speeds up Go compilation by shifting work from the compiler
(`go build`) to binary I/O.

## Motivation

| Array (tree-sitter-python) | Elements | Type | Lines (before) | Lines (Phase 2) |
|---|---|---|---|---|
| `_ts_small_parse_table` | 118,009 | `uint16` | 117,908 | ~1 |
| `_ts_parse_actions` | 5,042 | union | 73,934 | ~6,000 |
| `_ts_parse_table` | — | — | 18,313 | ~1 |
| `_ts_lex_modes` | — | — | 12,202 | ~1 |

Phase 1 (compact union byte literals, MR !26) handles the union case.
Phase 2 targets the scalar arrays that account for the remaining bulk.

## Design

### CLI flag

```
--compact-data[=<threshold>]
```

- Boolean option (default: off).
- Optional threshold: minimum **element count** to embed (default: 256).
- Only applies when generating a standalone output file (`-o file.go`).

### When to apply

All of the following must be true:

1. **`--compact-data` flag is set.**
2. **Array element type is scalar** — integers (`char`, `short`, `int`,
   `long`, `long long`, their `unsigned` variants), `_Bool`, or floating
   types (`float`, `double`).  Pointers, arrays, structs, unions, and
   complex types are **not** compacted (they stay inline).
3. **Element count >= threshold** (default 256).  Small arrays aren't
   worth the embedding overhead.
4. **All elements are compile-time constants** — no relocations, no
   deferred `init()` patches.  This is guaranteed by the existing ccgo
   initializer pipeline: if any element requires a pointer or address
   constant, the whole array stays inline.
5. **Proven const / never-mutated** — the array is declared `const` in
   C or ccgo can determine that no code writes to it.  (See
   [Mutability analysis](#mutability-analysis) below.)

### Output

Instead of:

```go
var table = [118009]uint16{
    0: 1234,
    1: 5678,
    // ... 117,907 more lines ...
}
```

Emit:

```go
//go:embed table.bin
var table_bin [236018]byte // = 118009 * 2 bytes

var table = (*[118009]uint16)(unsafe.Pointer(&table_bin))
```

### Alignment (`the hard problem`)

`//go:embed` creates a `[]byte` or `[N]byte` with 1-byte alignment.
Casting `*[N]uint16` atop a 1-aligned base is undefined behaviour on
strict-alignment architectures (arm, arm64, etc.) and `go vet`-hostile.

**Solution**: generate an `init()` function that copies the embedded data
into an aligned global:

```go
//go:embed table.bin
var table_bin [236018]byte

var table [118009]uint16

func init() {
    copy((*[236018]byte)(unsafe.Pointer(&table))[:], table_bin[:])
    // Rationale: Go guarantees that the address of a global is
    // sufficiently aligned for its type.  The copy goes byte-by-byte
    // through the runtime, so alignment of the source doesn't matter.
}
```

**Trade-off**: this doubles memory (the embed copy + the aligned global)
and adds startup cost.  For the typical use case (programs that are >90%
data), the memory doubling is a concern.

Future work could investigate zero-copy alignment techniques (e.g.,
using Go's internal `runtime.mallocgc` alignment guarantees or linker
support), but no such API currently exists in Go 1.25.

### Mutability analysis

We must not apply `--compact-data` to arrays that are later mutated,
because embedding in `//go:embed` puts data in read-only memory.

**Conservative rule**: only compact arrays declared `const` in C.  This
matches tree-sitter's usage (all parse tables are `const`) and is
straightforward to check: the AST node carries a `Const()` method or
equivalent.

**Future enhancement**: tracing writes to determine that a non-`const`
array is never written (dead dataflow analysis).  Not needed for the
initial implementation.

### Endianness

Binary files are target-endian.  For a given C source, ccgo targeting
linux/amd64 (little-endian) and linux/s390x (big-endian) will produce
different `.bin` files.  This is already the case for ccgo output (which
is per-GOOS/GOARCH), so it's not an additional burden.

Implementation: use `t.cfg.ByteOrder` (cc's ABI byte order) when encoding
multi-byte integers.

### File naming

Binary files are placed alongside the output `.go` file, named
`<basename>_<arrayname>.bin`.  For example, `-o output.go` with array
`_ts_small_parse_table` → `output_ts_small_parse_table.bin`.

Multiple arrays in the same output file each get their own `.bin` file.

### Compile-time embedding

The `//go:embed` directive requires `table.bin` to exist at compile
time.  This means the `.bin` files must be generated **before**
`go build`.  Two options:

1. **ccgo generates both `.go` and `.bin` files simultaneously** —
   simplest.  The user runs `ccgo -o output.go --compact-data input.c`,
   and ccgo writes `output.go` + `output_*.bin`.  Then `go build`
   picks up the `.bin` files.

2. **ccgo generates a Go source that imports the embed** — as above.

No additional tooling required.

## Implementation plan

### Step 1: Flag and plumbing (future MR)

- Add `compactDataThreshold uint32` field to `Task` struct (0 = off, >0 = threshold)
- Add `--compact-data` as `OptionalArg` in arg parsing (using `set.OptionalArg`)
- Wire the flag through to `ctx` so `initializerArray` can see it

### Step 2: Binary writer utilities

- Create a writer that encodes scalars to `io.Writer` using target byte order
- Handle types: `bool`, `int8/uint8`, `int16/uint16`, `int32/uint32`,
  `int64/uint64`, `float32`, `float64`

### Step 3: `initializerArray` compact path

- In `initializerArray`, before emitting the literal, check eligibility
  (const, scalar, threshold)
- If eligible, open `.bin` file, iterate elements writing raw bytes
- Emit `//go:embed` + global declaration + `init()` copy or pointer
- Fall back to inline literal if any element is non-const or needs relocation

### Step 4: Test with real grammars

- Run on tree-sitter-python, measure source size and compile time
- Verify correctness (output matches before/after)

## Open questions

1. **Threshold default**: 256 is a guess.  Should it be higher (1024)?
   Lower?  Measure the overhead of `init()` copy vs. inline literal.
2. **Mutability escape hatch**: should there be `--compact-data-force` or
   `//ccgo:compact-data` pragma for users who know their arrays are
   never mutated despite not being `const` in C?
