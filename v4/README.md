[![LiberaPay](https://liberapay.com/assets/widgets/donate.svg)](https://liberapay.com/jnml/donate)
[![receives](https://img.shields.io/liberapay/receives/jnml.svg?logo=liberapay)](https://liberapay.com/jnml/donate)
[![patrons](https://img.shields.io/liberapay/patrons/jnml.svg?logo=liberapay)](https://liberapay.com/jnml/donate)

# ccgo/v4

Command ccgo/v4 is a C compiler producing Go code.

## Interacting with Transpiled Code: The Memory ABI

`ccgo` transpiles C pointers into Go `uintptr`s. Because C and Go have fundamentally different memory management models (manual vs. Garbage Collected), there is a strict Application Binary Interface (ABI) rule you must follow when passing data between Go and transpiled C code.

**⚠️ Rule: Never pass a Go pointer to a transpiled C function.**

You must never convert a Go pointer (e.g., pointing to a Go slice, string, or struct) into a `uintptr` and pass it to a `ccgo`-generated function. 

Go's Garbage Collector is unaware of C-style pointer math. If the GC runs while the transpiled code is executing, it may move the Go memory, leaving the `uintptr` pointing to invalid memory. Furthermore, passing Go pointers to transpiled C code will frequently cause `go test -race` to panic, as the race detector's ThreadSanitizer does not recognize the memory access patterns of the transpiled code on Go-managed memory.

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
