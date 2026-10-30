[![LiberaPay](https://liberapay.com/assets/widgets/donate.svg)](https://liberapay.com/jnml/donate)
[![receives](https://img.shields.io/liberapay/receives/jnml.svg?logo=liberapay)](https://liberapay.com/jnml/donate)
[![patrons](https://img.shields.io/liberapay/patrons/jnml.svg?logo=liberapay)](https://liberapay.com/jnml/donate)

# ccgo/v4

Command ccgo/v4 is a C compiler producing Go code.

## Interacting with Transpiled Code: The Memory ABI

`ccgo` transpiles C pointers into Go `uintptr`s. Because C and Go have fundamentally different memory management models (manual vs. Garbage Collected), there is a strict Application Binary Interface (ABI) rule you must follow when passing data between Go and transpiled C code.

**⚠️ Rule: Never pass a Go pointer to a transpiled C function.**

You must never convert a Go pointer (e.g., pointing to a Go slice, string, or struct) into a `uintptr` and pass it to a `ccgo`-generated function. 

Go's Garbage Collector is unaware of C-style pointer math. If the GC runs while the transpiled code is executing, it may move the Go memory, leaving the `uintptr` pointing to invalid memory. Furthermore, passing Go pointers to transpiled C code will frequently cause `go test -race` to panic: `-race` (and `-msan`, `-asan`) enables checkptr, which rejects a pointer computed from a `uintptr` when it points into Go-managed memory (`checkptr: pointer arithmetic result points to invalid allocation`). That makes `go test -race` the way to find violations of this rule.

### The Correct Approach: `libc.Xmalloc`

To pass memory to or from a transpiled function, you must allocate it on the "C Heap" managed by `modernc.org/libc`. This memory is invisible to the Go GC and remains completely stable.

**The Workflow:**

1. **Allocate:** Use `libc.Xmalloc(tls, size)` to allocate memory.
2. **Copy In:** Use `unsafe.Slice` to create a Go view of the C memory and `copy()` your Go data into it.
3. **Execute:** Pass the `uintptr` returned by `Xmalloc` to the transpiled function.
4. **Copy Out:** Use `unsafe.Slice` to copy the results back into your Go variables.
5. **Free:** Always clean up the memory using `libc.Xfree(tls, ptr)`.

**Example:**

```
tls := libc.NewTLS()
defer tls.Close()

size := 64
// 1. Allocate on C Heap
cPtr := libc.Xmalloc(tls, size)
defer libc.Xfree(tls, cPtr) // 5. Free

// 2. Copy In
cSlice := unsafe.Slice((*byte)(unsafe.Pointer(cPtr)), size)
copy(cSlice, myGoData)

// 3. Execute
transpiled_c_function(tls, cPtr)

// 4. Copy Out
copy(myGoData, cSlice)
```

### `go vet` findings in transpiled code

Transpiled code converts `uintptr` C pointers to `unsafe.Pointer`, which the `unsafeptr` analyzer of `go vet` reports as "possible misuse of unsafe.Pointer". In `ccgo` output these are false positives: C pointers point into memory the Go GC does not manage (the libc heap, the TLS stack, pointer-free Go globals), so nothing can move or free it behind a `uintptr`. That holds only as long as the rule above is followed.

- `go test` never runs `unsafeptr`, its vet subset does not include it (see `go help test`).
- `go vet ./...` vets only the packages of the current module, never its dependencies. Only a module that contains `ccgo` output itself sees the findings. Use `go vet -unsafeptr=false ./...` there and expect `unreachable code` findings as well.
- gopls shows the same diagnostics in an editor. They are cosmetic.

## `--compact-data`

Replaces large const scalar array initializers with a compact `[N]uint64`
literal cast through `unsafe.Pointer`. Optional threshold argument, default
256 elements.
