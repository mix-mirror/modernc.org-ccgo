# Handoff: keep locals whose address does not escape out of the TLS frame

Status: **PROPOSAL, thinking phase.** Nothing changes in ccgo or libc until the
new modernc.org/sqlite (3.54.1) is tagged (user, 2026-10-19). Asking for a
second opinion now.

Measured 2026-10-19 on ccgo v4.37.0 output (modernc.org/libquickjs master
865a39e, QuickJS 2026-06-04), Go 1.27.2, linux/amd64 (AMD Ryzen 9 3900X).
Coordinated between the libquickjs and quickjs sessions. Related:
`../../docs/handoffs/unsafeptr-handoff.md` (resolved in 6cf0225, released in v4.38.0) and `../../docs/handoffs/setjmp-recover-handoff.md`.

## TL;DR

- A C function gets a TLS frame (`bp := tls.Alloc(N)` + `defer tls.Free(N)`)
  as soon as one of its locals is *pinned*, that is, its address is needed.
  In libquickjs, 988 of 1894 functions have one. An experiment that dropped
  every `defer tls.Free` gave -18.6% on BenchmarkMicro. The user rejected
  that: existing embedders rely on the defers to clean up when a panic goes
  through transpiled code (see "Why not drop the defers" below).
- Instead, emit **fewer frames**. Most pinned locals need an address only to
  be dereferenced right away, mostly to read a union member of a value
  (`JS_VALUE_GET_PTR(v)` in the inlined `JS_FreeValue`/`JS_DupValue`, the
  musl `isnan` union, by-value `JSValue` parameters). Go can do that on a Go
  local with `unsafe.Pointer(&x)`, and escape analysis keeps the local on the
  goroutine stack.
- What a classifier over the generated Go found (method below), weighted by
  the 1.38e9 frame entries of one BenchmarkMicro run:

  | frame category                                   | functions | frame entries |
  |--------------------------------------------------|----------:|--------------:|
  | step 1: all slots only dereferenced locally      | 414 (42%) |         51.6% |
  | step 2: slot addresses also go to safe callees   | 306 (31%) |   14.0% (19.4%) |
  | slots safe, plus va_list areas                   |  20 (2%)  |          2.3% |
  | va_list areas only                               |  36 (4%)  |          0.0% |
  | blocked: some address really escapes             | 206 (21%) |   31.0% (25.6%) |
  | bp + non-constant offset (not analyzed)          |   6 (1%)  |          1.1% |

  The numbers in parentheses assume libc `memset`/`memcpy`/`memmove`/`memcmp`
  can take Go pointers.
- **A prototype of step 1 alone gives -11.6%** on BenchmarkMicro (median of
  4 alternating runs: 20,234 -> 17,890; per-test geomean -13.3%; 60 of 66
  tests faster), **with every defer kept**. The full suite including Test262
  passes. No frame local escapes to the heap (`-gcflags=-m`). The stack
  limits stay the same (see "Hazards", item 6).
- Recommendation: do step 1 in ccgo first. It is local to one function and
  has the largest share. Step 2 (pointer-typed parameters, below) adds about
  14-19% of the frame entries and needs whole-program information at link
  time.

## Why not drop the defers

Findings that led the user to keep the defers (libc v1.79.0, probe on
linux/amd64 and windows/amd64 under wine):

- With frees before each return instead of defers, a panic that goes through
  transpiled frames leaves those frames allocated. If the panic is recovered
  outside transpiled code and the TLS is then closed:
  - On linux, `TLS.Close` frees everything.
  - On every other target, `Close` is only `t.Free(4)`. It frees no stack
    segment and never looks at `allocas`/`allocaStack`, so both leak.
  - The user decided that this `Close` leak gets fixed in libc after sqlite
    3.54.1, whatever happens to the defers.
- If the embedder keeps using the TLS without closing it:
  - The frames leak, and `StackSlots()` stays raised on every target.
  - Outside linux, if an older transpiled frame recovers and then frees its
    own frame, `Free(n)` steps below the segment start and the next `Alloc`
    returns memory outside any segment.
- The gain comes almost all from the escaping-local `defer tls.Free(N)`.
  The alloca defers are in only 4 functions, so dropping just those would buy
  little.

Removing frames avoids all of this: a Go local needs no cleanup on any
path, panics included.

## Where the frames come from (examples from ccgo_linux_amd64.go)

- `XJS_SetPropertyInt64`: the whole frame is the temporary of the inlined
  `JS_FreeValue`, `*(*TJSValue)(unsafe.Pointer(bp)) = v1`, read back as
  `*(*uintptr)(unsafe.Pointer(bp))` to get `u.ptr`.
- `_JS_ToUint8ClampFree`: the by-value parameter `val` is copied to `bp+8` to
  read its union, and musl's `isnan` union `__u` sits at `bp+0`.
- `_js_string_pad`:
  - `str` and `v` are JSValues pinned only because their union is read;
  - `b_s` is the `StringBuffer b_s, *b = &b_s` pattern (step 2);
  - `n` is an out-parameter of `JS_ToInt32Sat` (step 2).
- `_js_array_includes`: the out-parameters `&len` (`js_get_length64`),
  `&n` (`JS_ToInt64Clamp`), and `&arrp`/`&count32` (`js_get_fast_array`),
  all step 2.
- Blocked, for good reasons:
  - `JS_CallInternal` and `js_call_c_function`: the stack frame `sf_s` is
    linked into `rt->current_stack_frame`.
  - `JS_ToPrimitiveFree`: `&arg` becomes the argv of an indirect call.
  - `__JS_AtomToValue`: `char buf[]` goes to `snprintf`/`memcpy`.
  - `u32toa`-style callees return a pointer derived from their argument.

In ccgo the decision is `declInfo.pinned()` (decl.go:29), set by `pin()`
(expr.go:245) whenever an expression needs `exprUintptr` mode, by the inline
parameter path (expr.go:4536), and by `registerLocal` (decl.go:283), which
pins *every* local that is not a parameter in a function with a switch inside a
loop. Why that last one is needed is a question for you. The step 1 prototype
removed such frames whenever all their slots were dereferenced locally, and
Test262 passed. ccgo.go:11 already has the TODO: "acosh u does not need to be
pinned, need better escape analysis above 'address taken'".

## Step 1: locals whose address is only dereferenced locally

Rule: a pinned local can be a Go local if its address is used only inside the
expression that dereferences it, never converted to `uintptr`, never stored,
passed, returned or compared outside such a dereference.

What the prototype emitted (a post-processing of the generated Go; 414 of 414
functions rewritten, plus 381 in `internal/run-test262`):

```go
var __frame [N/8]uint64 // instead of bp := tls.Alloc(N); defer tls.Free(N)
... *(*T)(unsafe.Add(unsafe.Pointer(&__frame), K)) ... // instead of *(*T)(unsafe.Pointer(bp + K))
```

In ccgo proper, one Go local per declarator is better (`var x TJSValue`,
`*(*uintptr)(unsafe.Pointer(&x))`). It keeps Go's register allocation and
liveness per variable. Here a union member is read in one expression, which is
exactly where Go's unsafe rules allow it.

Results:
- BenchmarkMicro (test binaries run alternately, `-test.cpu 4`):
  - totals: baseline 20,140-20,262, prototype 17,853-18,104;
  - best: array_slice -36%, string_build3 -29%, int_arith -28%;
  - slower: array_push +12%, array_prop_create +7%, math_min +7%,
    float_arith +3%. Not investigated yet. Frame zeroing or bigger Go frames
    are suspects.
- The test binary is 320 KB smaller.

## Step 2: pointer-typed parameters

The user's original idea: for a static callee, turn a pointer to a caller's
local into an extra return value, or an extra argument plus an extra return
value, rewriting the dereferences in the callee. A variant that is easier and
covers more is to keep reference semantics and only change the type:

- The caller's local stays a Go local, and its address travels as
  `unsafe.Pointer` (or `*T`).
- The callee's parameter becomes `unsafe.Pointer`:
  - `*(*T)(unsafe.Pointer(p))` -> `*(*T)(p)`;
  - `p + K` -> `unsafe.Add(p, K)`;
  - `p != 0` -> `p != nil`.
- Callers that pass a C pointer convert it with `unsafe.Pointer(x)`, as ccgo
  does everywhere today.
- Go's escape analysis then decides stack or heap on its own, and when the
  callee only dereferences, the local stays on the stack.

Compared with copy-in/copy-out, this:

- has no aliasing problem: if `*p` is also reachable another way, both still
  refer to the same memory;
- has no partial-write problem on a longjmp or panic out of the callee;
- handles optional out-parameters (`if (pres) *pres = ...`): callers may pass
  NULL;
- handles structs, field addresses and pointers passed further down: the
  parameter of the next callee becomes `unsafe.Pointer` too, computed as a
  fixpoint over the call graph;
- handles alias variables (`StringBuffer *b = &b_s`): the local pointer variable
  becomes `unsafe.Pointer`;
- leaves the callee's body almost unchanged.

A value return for pure scalar out-parameters could come later as a
refinement, since it lets Go keep the value in a register. The frame and
the defer are the main cost, though, not the memory access.

Eligibility, as the classifier computes it (a greatest fixpoint):

- An address value is safe when every use is one of:
  - a dereference;
  - a comparison;
  - `+`/`-` whose result is again safe;
  - an assignment to a local or parameter variable that is safe;
  - an argument to a direct call whose parameter is safe.
- Unsafe ("sinks"):
  - a store to memory;
  - `return`;
  - a conversion to an integer;
  - an indirect call;
  - a libc call (unless libc gets `unsafe.Pointer` variants of `mem*`);
  - a `VaList` value.
- `static` is not required. In libquickjs, step 2 changes 310 parameters of
  258 functions. 36 of those are exported and 16 have their address taken. For
  those, ccgo keeps a wrapper with the original `uintptr` signature that
  converts and calls the new form, or clones the function for the direct
  calls.
- It needs every call site and every callee body, so it belongs at link time.
  Is that feasible in ccgo's linker?

## Smaller items

- Frames that exist only for `libc.VaList` (mostly error paths: varargs
  `JS_ThrowTypeError` calls) could move into a closure at the call site:
  `func() { bp := tls.Alloc(n); defer tls.Free(n); f(..., libc.VaList(bp, ...)) }()`.
  That's 56 functions, but only 2.3% of frame entries in this benchmark.
- libc `memset`/`memcpy`/`memmove`/`memcmp` taking Go pointers (Go code, or
  `unsafe.Pointer` variants) would add another 5.4% of frame entries.

## Hazards and constraints

1. **A Go pointer must never become a `uintptr` that outlives the
   expression.** A stack copy would not update it. The analysis has to be
   sound, and anything it doesn't recognize is a sink.
2. **Small integers in pointer-typed variables.** Go throws "invalid pointer
   found on stack" when a pointer-typed stack slot holds 1..4095. A parameter
   that becomes `unsafe.Pointer` must not receive C sentinels such as
   `(void *)1`, so callers that pass a non-zero integer constant disqualify the
   parameter. NULL is fine.
3. **Pointers outside the object.** C allows one-past-the-end and other
   arithmetic beyond the object (`end = buf + n`), and Go's unsafe rules
   don't. On the stack it's benign, since a stack copy adjusts every pointer
   into the old stack. It could matter if Go ever moved such a local to the
   heap. To start with, step 2 could exclude arrays whose derived pointers use
   non-constant arithmetic (161 named `bp` slots in libquickjs are arrays).
4. **Alignment.** Go locals must keep the C alignment, for example `uint64`
   elements or an aligned struct, and 16 where C needs it.
5. **Zeroing.** Go zeroes every local on each call, while `tls.Alloc` didn't.
   Large `char buf[...]` locals could stay in `bp`, or the threshold could be
   measured.
6. **The JS stack limit.** libquickjs (and `internal/run-test262`) compare
   `tls.StackSlots()` with the runtime's limit in `_js_check_stack_overflow`.
   A frame that leaves the TLS stack no longer counts.
   - Step 1 keeps a frame in every recursion cycle that was tested. The
     maximum depth stayed the same within +-4 for nested parentheses,
     blocks, functions, `JSON.parse`, non-capturing regexp groups,
     `JSON.stringify` and JS recursion, and no input up to 10^6 levels
     reached Go's fatal stack overflow.
   - Step 2 must re-check this. A recursion cycle without any frame would be
     stopped only by Go's 1 GB stack limit, which is fatal.
   - The goroutine stack per level grows by the frames moved onto it. That's
     not measured yet.
7. **setjmp.** Locals of a function that calls setjmp must keep their values
   across the longjmp, which ccgo lowers to panic/recover in that function.
   Go locals do. If the lowering uses closures, captured locals escape to the
   heap, which is correct but slower. Please check against
   `../../docs/handoffs/setjmp-recover-handoff.md`.

## Reproducing

The tools are in the libquickjs session's scratchpad and may be gone. The
method is short:

- `bpclass`: go/ast over `ccgo_linux_amd64.go`.
  - Slots come from the `/* name at bp+K */` comments, the parameter copies
    and the `libc.VaList(bp+K, ...)` calls.
  - Each `bp`/`bp + K` occurrence is classified by its parent chain, using the
    rules above.
  - Uses of every local and parameter identifier are classified the same way,
    giving edges.
  - A greatest fixpoint follows.
- Weighting: a counter after each `bp := tls.Alloc` in a scratch copy, then
  `-bench Micro -benchtime=1x`. 299 functions entered frames 1.38e9 times;
  `_set_value` alone accounts for 232M of them (step 1: an inlined
  `JS_FreeValue` temporary).
- `bplocal`: the step 1 rewrite above, applied to the functions `bpclass`
  lists as local-only.
