# Handoff: replace the `__ccgo_up` vet bypass with `unsafe.Add(nil, x)` (#45)

Status: **RESOLVED and released** in `v4.38.0` (commit `6cf0225`). See "Resolution" at
the end for what was changed and verified.

Measured 2026-10-19 on ccgo v4.37.0 output (modernc.org/libquickjs
master 776071a, QuickJS 2026-06-04), Go 1.27.2. Coordinated between the
libquickjs and quickjs sessions; trigger was the liustack/quickjs fork, commit
[ec65a63](https://github.com/liustack/quickjs/commit/ec65a639c03d560048b4424085dd5230aa53411a),
which strips `__ccgo_up` from its vendored libquickjs by post-processing.

## TL;DR

- Since v4.34.1 (da48850, extended in ec33443) ccgo writes some dereferences of
  a C pointer `x` (a `uintptr`) as `**(**T)(__ccgo_up(x))`, only to keep
  `go vet -unsafeptr` quiet. This is the benchmark the #45 thread asked for
  ("I want to see some benchmarks, like from modernc.org/quickjs if there was
  any slowdown/cost introduced by this partial fix. If something goes wrong we
  would need to revert and rethink."). Something went wrong:
  - every use inlines into its **own address-taken stack slot**: a store of
    `x` and a load back, never merged, never forwarded on arm64;
  - `_JS_CallInternal`'s Go frame is **2568 bytes instead of 936** on
    linux/amd64 and **9832 instead of 968** on linux/arm64. On arm64, 1209
    functions get bigger frames and the code of the package grows 10%;
  - the QuickJS microbenchmark total is **28% higher** on darwin/arm64 (M1).
    Removing the bypass cuts it by 22%, the per-test geomean by 28%, and
    every one of the 66 tests gets faster. On linux/amd64 the total is
    7-9% higher, and the interpreter-loop tests are 18-38% slower;
  - each libquickjs TLS stack slot costs about 2-3x more goroutine stack on
    amd64 and 7-8x more on arm64: 10000 slots of JS recursion take 134.5 MB
    of goroutine stack on darwin/arm64, against 17.0 MB without the bypass.
- `*(*T)(unsafe.Add(nil, x))` is **vet-clean and free**: `unsafeptr` only
  inspects `unsafe.Pointer(<uintptr>)` conversions, and the compiler folds
  `nil + x` away. On libquickjs it gives the same vet count as `__ccgo_up` and
  1.4-1.8% *smaller* code than even the pre-v4.34.1 plain conversion.
- **Recommendation:** emit `unsafe.Add(nil, x)` wherever `__ccgo_up` is used
  now, then drop `__ccgo_up`. A "use the bypass only where it does not hurt"
  split is not needed: there is a form that does not hurt anywhere.
  Optionally extend `unsafe.Add(nil, x)` to the remaining flagged conversions
  (21,724 in libquickjs) to make the output `unsafeptr`-clean at no cost.
- `-race` changes back to the pre-v4.34.1 behaviour (see below). That is a
  plus: `__ccgo_up` hides from checkptr exactly the bug #45 was about.

## The three forms

```go
*(*T)(unsafe.Pointer(x))       // A: ccgo before v4.34.1
**(**T)(__ccgo_up(x))          // B: since v4.34.1; __ccgo_up(n) returns unsafe.Pointer(&n)
*(*T)(unsafe.Add(nil, x))      // C: proposed
```

Micro test (one loop, three dereferences, a call between them), Go 1.27.2:

| form | `go vet` | linux/amd64 frame / code | linux/arm64 frame / code |
|---|---|---|---|
| A plain conversion | 3x "possible misuse of unsafe.Pointer" | 0x30 / 205 B | 0x38 / 224 B |
| B `__ccgo_up` | clean | 0x30 / 202 B | **0x48 / 240 B** |
| C `unsafe.Add(nil, x)` | clean | 0x30 / 197 B | 0x38 / 208 B |
| D `unsafe.Pointer(uintptr(unsafe.Pointer(nil)) + x)` | clean | 0x30 / 197 B | 0x38 / 208 B |

arm64 code for B. Each `__ccgo_up` is its own slot (`n-40`, `n-48`, `n-56`):

```
MOVD R1, forms.n-40(SP)     // store x
MOVD forms.n-40(SP), R1     // load it back
MOVD (R1), R1               // the real load
```

C folds into a single `MOVD (R0)(R1), R1`. D behaves the same as C, but C
reads better. On amd64 the slots partly merge or forward, so B costs less there
than on arm64, but not nothing (see the frame table below).

## libquickjs numbers

All four 64-bit generated files were rewritten mechanically (13,242 sites in
the linux files, 13,224 in the darwin ones). The tool is at
`/tmp/claude-1000/-home-jnml-src-modernc-org-libquickjs/ef1bb532-f105-4fb3-a81f-9417a4698302/scratchpad/tools/unup`
(`-add` for form C; it may be gone after a reboot, but it is trivial to
redo). Variants: **orig** = as generated (B), **noup** = A, **add** = C.

### Frames and code (`go build -gcflags=modernc.org/libquickjs=-S`, `STEXT ... locals=`)

| | linux/amd64 orig | noup | add | linux/arm64 orig | noup | add |
|---|---|---|---|---|---|---|
| `_JS_CallInternal` frame | 2568 | 936 | 928 | 9832 | 968 | 1112 |
| `_resolve_labels` frame | 1496 | 928 | 928 | 3592 | 936 | 936 |
| `_next_token` frame | 576 | 152 | 152 | 2152 | 152 | 152 |
| sum of all frames | 259,408 | 247,496 | 243,440 | 384,648 | 273,944 | 270,408 |
| functions with a bigger frame than add | 484 | 305 | - | 1209 | 195 | - |
| functions with a smaller frame than add | 6 | 10 | - | 1 | 5 | - |
| code bytes | 1,627,962 | 1,597,219 | 1,568,444 | 1,553,056 | 1,426,544 | 1,406,656 |

### Speed (BenchmarkMicro = QuickJS `microbench.js`; `total` row of its table, lower is better)

Interleaved runs of prebuilt test binaries. darwin/arm64 on the M1 builder
machine while a builder test was running there; linux/amd64 on a 3900X pinned
to one core, with the builder VMs running.

linux/amd64 (Ryzen 9 3900X), 4 interleaved rounds, noisy box:

| | total median (min-max) | vs orig | per-test geomean vs orig |
|---|---|---|---|
| orig (B) | 19,915 (19,496-20,666) | - | - |
| noup (A) | 18,235 (17,885-18,967) | -8.4% | -10.7% |
| add (C) | 18,592 (18,571-20,116) | -6.6% | -7.8% |

On amd64, noup and add are within noise of each other: one add run was an
outlier, and the best runs are 17,885 and 18,571. A rerun of the two on a
quiet machine was started but stopped. On arm64, add was ahead in every
round.

Selected tests, median ns: `empty_loop` 62.4 / 50.2 / 50.2, `int_arith`
74.1 / 55.2 / 54.5, `float_arith` 106.4 / 67.1 / 66.2 (orig / noup / add).

darwin/arm64 (Apple M1), 4 interleaved rounds:

| | total median (min-max) | vs orig | per-test geomean vs orig |
|---|---|---|---|
| orig (B) | 16,861 (16,565-16,886) | - | - |
| noup (A) | 13,241 (13,217-13,286) | -21.5% | -27.2% |
| add (C) | 13,132 (13,130-13,151) | **-22.1%** | **-28.1%** |

Selected tests, median ns: `empty_loop` 41.8 / 34.1 / 33.1, `float_arith`
79.4 / 42.8 / 40.8, `prop_read` 51.0 / 32.0 / 31.3, `array_read`
38.4 / 23.3 / 22.7, `func_call` 108.5 / 84.0 / 84.1 (orig / noup / add).
The smallest gain is `prop_clone`, -11%.

The per-test view on amd64: the tight interpreter-loop tests
(`empty_loop` -18%, `int_arith` -28%, `float_arith` about -40%) gain
consistently. The `map_*` tests are bimodal in every variant (about 820 or
1150-1250 ns), which is noise.

### Goroutine stack per TLS stack slot (`StackInuse` at the recursion limit)

| `JS_SetMaxStackSize` | linux/amd64 orig | add | darwin/arm64 orig | add |
|---|---|---|---|---|
| 1000 slots | 4.5 MB | 1.4 MB | 17.0 MB | 2.4 MB |
| 10000 slots | 33.9 MB | 17.1 MB | **134.5 MB** | **17.0 MB** |

(Stacks grow by doubling, so these are coarse.) This matters for
libquickjs's default `MaxStackSlots` (see "libquickjs follow-ups" below).

### vet

| | `go vet -unsafeptr` findings |
|---|---|
| orig (B at the two v4.34.1/v4.34.2 sites) | 21,724 |
| noup (A everywhere) | 34,966 |
| add (C at the same sites as B) | 21,724 |

So today the bypass removes 38% of the findings and libquickjs is still far
from vet-clean. A full `go vet` also reports 525 `unreachable code` findings
(ccgo's labels), so `unsafeptr` is not the only analyzer that fires.

## `-race` / checkptr

With `-race` (also `-msan`, `-asan`), cmd/compile instruments every
`uintptr` to `unsafe.Pointer` conversion (`walk/convert.go` `walkConv` ->
`walkCheckPtrArithmetic`) and every `unsafe.Add` (`walk/expr.go:135`, walked
as `unsafe.Pointer(uintptr(p)+uintptr(n))`) with
`runtime.checkptrArithmetic(p, originals)`. It throws when `p` is in
(0, 4096), or when `p` is inside a Go heap object, the goroutine stack, or
the `data`/`bss` sections and no original points into the same object. Here
the only original is nil.

- ccgo memory passes. libc heap and the TLS stack are C heap: the non-musl
  `TLS.Alloc` pages come from `Xmalloc` (`libc/etc.go`), and musl's
  `allocSlow` uses `mustMalloc`. alloca is `Xmalloc`. C globals are Go
  variables without pointers, so they live in `noptrdata`/`noptrbss`, which
  `checkptrBase` does not look at. `__ccgo_ts` is in rodata.
- Verified on libquickjs: noup passes `Test`, `Test2` and a full
  `BenchmarkMicro` pass under `-race`. add passes `Test` and `Test2` under
  `-race`. One `-race` BenchmarkMicro took 125 s (orig) vs 134 s (noup): one
  runtime call per site, `-race` builds only.
- Verified with a micro test: with a pointer into a noptrbss array, all four
  forms pass. With a pointer into a Go heap slice (the #45 reporter's
  original mistake), **A, C and D throw**
  `checkptr: pointer arithmetic result points to invalid allocation`, and
  **B passes silently**. `__ccgo_up` converts the `*uintptr` `&n` (rule 1),
  so checkptr never sees the real pointer. It therefore hides the one runtime
  check that catches Go memory passed into transpiled code, against the memory
  ABI the README now documents.

Caveat for the docs: `unsafe.Add(nil, x)` is **not** a documented valid
pattern either. Rule 3 wants the base to point into an allocation. It is
"not flagged by vet today, same or better code than the plain conversion".
A future vet or checkptr tightening could flag it. `__ccgo_up` has the same
status: it satisfies rule 1 only by reinterpreting a `uintptr`'s storage.

## Order of evaluation

`__ccgo_up(x)` is a call, so Go evaluates the reads in `x` in call order with
the other calls of the statement. A conversion and `unsafe.Add` are not calls
(the spec orders only calls, method calls, receives and `&&`/`||`), so a read
in `x` is unordered with respect to a call to its right. Example:
`s->buf[i] = f(s)`, where `f` reallocs `buf`. C leaves this unsequenced too,
so correct C code does not depend on it, and ccgo emitted A for years before
v4.34.1. The liustack fork audited libquickjs: 23 statements per file where
the order changes, none dependent (`internal/fixlib/upsites.go` there).

## What users need to be told (README)

- **`go test` never runs `unsafeptr`.** Its vet subset (Go 1.27, `go help test`)
  is atomic, bools, buildtag, directive, errorsas, ifaceassert, nilfunc,
  printf, stdversion, stringintconv and tests. `go test` of libquickjs passes
  with the default vet. `-vet=off` only saves time and memory: a full vet of
  libquickjs takes about 6 s and 650 MB RSS on linux/amd64, which can hurt a
  small 32-bit builder.
- **`go vet ./...` never vets dependencies**, only the packages of the
  current module. Only a module that *contains* ccgo output sees the findings:
  the generated package's own repo, or a fork that vendors it as a subpackage
  (liustack/quickjs's `lib/`). Use `go vet -unsafeptr=false ./...` there, and
  expect `unreachable` findings too.
- Why it is a false positive: ccgo pointers are `uintptr`s into memory the Go
  GC does not manage (libc heap, the TLS stack, pointer-free Go globals), so
  nothing can move or free it behind the `uintptr`. Keep the existing rule:
  never pass Go memory into transpiled code. `go test -race` catches a
  violation, but only where ccgo does not use `__ccgo_up` (another reason to
  drop it).
- gopls shows the same diagnostics in an editor. That is cosmetic.

## Plan

1. Add a helper next to `unsafePointer` in `etc.go`, e.g.
   `func unsafeFromUintptr(arg interface{}) string { return unsafe("Add", fmt.Sprintf("nil, %s", arg)) }`
   (keep the `tag(importQualifier)`/`tag(preserve)` handling of `unsafe()`).
2. Replace the seven `ccgoUP` emit sites, turning
   `**(**%s)(%s%s(%s))` into `*(*%s)(<unsafeFromUintptr>)` with the same
   operands: `expr.go:1764, 2032, 2034, 4448, 4599, 4609` and `decl.go:1434`.
3. Stop emitting the `__ccgo_up` definition (`link.go:1719`) and drop the
   `ccgoUP` constant. Hand-written code that references `__ccgo_up` (none known)
   would break, so grep the fleet first.
4. Optional, in batches as #45 planned: route the remaining
   `unsafe.Pointer(<uintptr expr>)` emitters through the same helper until
   `-xvet` passes. Each batch is free at run time.
5. Verify: `go test -xvet` and csmith on the builders. On libquickjs, frame
   sizes (`-gcflags=-S | grep STEXT`), `BenchmarkMicro` on linux/amd64 and
   darwin/arm64, and `go test -race -run 'Test$|Test2$'`. A `-race` run of
   modernc.org/sqlite would be a good extra check of the checkptr side.

## Related ccgo/libc costs found in the same fork (separate from #45)

The fork post-processes these too (`internal/fixlib/alloca.go`, `frames.go`,
`cells.go`). They belong in ccgo or libc, not in a downstream rewriter:

1. **alloca bookkeeping on darwin/freebsd/windows allocates per call.**
   `stmt.go:294` emits `defer tls.FreeAlloca()()` for every non-linux
   target. libc `pthread.go:80` returns a new closure and sets `t.allocas =
   nil`, so the next alloca regrows the slice: about 1-2 Go allocations per
   call of `JS_CallInternal`, `js_call_c_function` and the other callers.
   The linux targets use `AllocaEntry`/`AllocaExit` (`libc_musl.go:548`),
   which allocates nothing. Fix: port `AllocaEntry`/`AllocaExit` to the
   non-musl libc and emit them for all targets.
2. **TLS frames used only for type punning.** Example: generated `XFreeValue`
   (`JS_FreeValue`) does `bp := tls.Alloc(16); defer tls.Free(16)` only to
   copy a by-value `JSValue` into the frame and read its union member back
   as a pointer. The C code never takes the address, but union member access
   on a value does. A local whose address is used only inside the expression,
   and never escapes as a C pointer value, can stay a Go local, read through
   `(*T)(unsafe.Pointer(&v))`; escape analysis keeps it on the stack. That saves
   `tls.Alloc`, `tls.Free` and a `defer` in many hot small functions, and the
   `JS_DupValue`/`JS_FreeValue` cells inside `JS_CallInternal`. Caveat: it
   changes how many TLS stack slots a call chain uses, and libquickjs's stack
   check counts slots (`MaxStackSlots`), so its tuning needs a recheck.
3. The fork's other two rewrites are not ccgo material. It pins 15
   interpreter-state variables to their stack slots on amd64 (a Go
   register-allocator workaround), and `objlitDefine` is a hand-written
   QuickJS fast path.

## libquickjs follow-ups (tracked on the libquickjs side, listed here for context)

- Fixed in libquickjs 31ad80f: `JS_NewRuntime2` set `stack_size = 1 MB`
  (meant as bytes), but `_js_check_stack_overflow` reads it as a slot count.
  So the default limit was about 1M slots, and infinite recursion killed the
  process with Go's `fatal error: stack overflow` instead of throwing.
  `quickjs.diff` now leaves it 0, so `MaxStackSlots` applies.
- libquickjs c79c3f9 raised `MaxStackSlots` from 1000 to 5000. A call through
  built-ins (getter, `forEach`, `apply`, proxy, generator) costs 3-9 slots,
  so 5000 is about native C QuickJS's depth. With today's frames that is
  64 MiB of goroutine stack at the limit on darwin/arm64. Go's fatal ceiling
  (a stack may not grow past 1 GB, so 512 MiB in practice) is reached at about
  40000 slots on arm64. With form C, 10000 slots would take about 17 MiB on
  arm64, so landing this handoff makes about 10000 (V8's ballpark) affordable.

## Resolution (2026-10-19)

Plan steps 1-3 are done, step 4 is not.

- `etc.go`: `unsafeFromUintptr(arg)` renders `unsafe.Add(nil, arg)` through
  `unsafe()`, so the `importQualifier`/`preserve` tags are kept.
- The seven emit sites (`expr.go` cast/deref, two in `postfixExpressionIndex`,
  the compound assignment, two pinned-local reads; `decl.go` pinned-local
  initializer) now emit `*(*T)(unsafe.Add(nil, x))` with the same operands.
- `link.go`: the `__ccgo_up` definition is no longer written unconditionally.
  The linker writes it only when an object file references it, the same way
  `needFpFunc` handles `__ccgo_fp`. New output never contains it, but a
  `.o.go` from v4.34.1-v4.37.0 (same `objectFileSemver` v2) still links
  instead of failing with `undefined: __ccgo_up`. `ccgoUP` stays for that
  check only. No hand-written code under `~/src/modernc.org` references
  `__ccgo_up` (2,027 files do, all ccgo output or ccgo itself).
- `../README.md`: the memory ABI section now says that `-race` panics come
  from checkptr (not ThreadSanitizer), and a new subsection covers `go vet`
  findings in transpiled code (the points of "What users need to be told").

Verified on linux/amd64:

- `make test` passes (0 fails in every corpus). The golden drift it printed
  is the known host-gcc-14 noise (three `mir/c-tests/lacc` files) and was
  reverted.
- A small C test that hits every changed site matches gcc, also under
  `-race`, and is clean under `go vet -unsafeptr`. An object file produced
  by the v4.37.0 ccgo links with the new linker and runs.
- libquickjs regenerated with this ccgo (scratch clone, `go.work`, linux/amd64)
  is identical to the mechanical `unup -add` rewrite of the committed file,
  except for one line: a redundant `int64(...)` around an `int64_t` pinned
  local in `quickjs-libc.c`'s `js_printf_internal` is now dropped. The
  linker's conversion pruning (`link.go`, `*gc.Conversion`) can type the
  operand through `unsafe.Add` but could not through `__ccgo_up`.
  `_JS_CallInternal` frame 928 (was 2568), `_resolve_labels` 928,
  `_next_token` 152. `go vet -unsafeptr` count is 21,732 with both the old
  and the new output at libquickjs c79c3f9. All tests except Test262 pass,
  also with `-race`, and a `BenchmarkMicro` pass completes.

Not done here:

- Step 4 (route the remaining `unsafe.Pointer(<uintptr>)` emitters through
  `unsafeFromUintptr` until `-xvet` passes).
- `-xvet`/csmith on the builders and the darwin/arm64 benchmark. They were not
  rerun: the regenerated output is the measured form C.
- The "Related ccgo/libc costs" items 1 and 2 remain open.
- libquickjs (and the rest of the fleet) picks this up on the next ccgo
  release and regeneration.
