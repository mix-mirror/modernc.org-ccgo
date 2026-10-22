# Handoff: keep locals whose address does not escape out of the TLS frame

Status: **steps 1 and 2 RESOLVED**, see "Resolution, step 1" (2026-10-21)
and "Resolution, step 2" (2026-10-22) at the end. The smaller items are open.
The gate, modernc.org/sqlite with SQLite 3.54.1, was released as v1.61.0 on
2026-10-21.

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

## Resolution, step 1 (2026-10-21)

Implemented in ccgo: `postfixExpressionSelect` and
`postfixExpressionSelectUnionField` in expr.go, with `isAutomaticLvalue`,
`isAddressable` and the `addressableParams` bookkeeping of `inlineInfo`.
Test: `TestUnionMemberFrame` in frame_test.go. Steps 2 and the smaller items
stay open.

### What pinned the locals

Not a missing escape analysis. ccgo already keeps a struct local as a Go
variable and reads its members as Go fields. A union member it has to read
through a pointer, since Go has no unions, and two of the three codegen paths
for that read took the base variable's address as a `uintptr`
(`c.expr(..., exprUintptr)`). Taking the address that way pins the variable
into the TLS frame, whatever happens to the address afterwards:

- a member whose size equals the union's: `v.u.ptr`, `v.u.float64`, `v.u.u64`
  of a JSValue, musl's `isnan` union `__u.__i`;
- a union member reached through a struct member chain
  (`postfixExpressionSelectUnionField`).

The third path, a member smaller than the union such as `v.u.int32`, already
used `unsafe.Add(unsafe.Pointer(&v.Fu), off)` on the Go variable without
pinning it. The two other paths use that form now whenever the base is an
automatic variable or a member chain of one (`isAutomaticLvalue`). The
address exists only inside the expression that dereferences it, which is what
the Go unsafe rules allow, nothing is converted to `uintptr`, and the variable
stays on the goroutine stack, so no cleanup is needed on any path, panics
included. Every remaining frame keeps its defer.

Covered: locals, by-value parameters, parameters of inlined functions
(`JS_FreeValue`, `JS_DupValue`, ...). An inlined parameter that is read once
and neither written nor address-taken is replaced by its argument expression
as written; when that argument is not addressable, for example a call result,
the parameter keeps the old spill into the caller's frame
(`addressableParams`). Volatile and atomic accesses keep the old path.

### Numbers (libquickjs, linux/amd64)

Measured by the libquickjs session on 2026-10-21 (libquickjs ba53053, Go
1.27.3), ccgo cfa1c20 against cfa1c20 plus this change:

- Functions with a TLS frame: 988 -> 576 of 1893 in libquickjs, 866 -> 488 of
  1700 in internal/run-test262.
- The classifier of this document: local-only 414 functions (1122 slots) ->
  2 (7 slots). The two left are the call-result spill described above
  (`JS_GetActiveFunction(ctx).u` in js_object_constructor and
  js_method_set_home_object). Interprocedural 306 -> 304, blocked 206 -> 205.
- Frame entries in one BenchmarkMicro run: 1.383e9 -> 0.670e9 (-51.6%),
  the whole local-only share. What is left: blocked 64%, interprocedural
  (step 2) 29%, VaList-only 4.8%.
- `go build -gcflags=-m`: 0 "moved to heap" before and after. Inlinable
  functions 331 -> 364, none lost; the 412 inlining candidates that
  disappear are the `.deferwrap1` closures of the removed frames.
- `_JS_CallInternal` code 75121 -> 68391 bytes.
- BenchmarkMicro, five alternating rounds per build, `-test.cpu 4`, quiet
  machine: total 17,759.9 -> 14,762.8 ns (median, -16.9%, the ranges do not
  overlap), geomean over the 66 tests 0.811 (-18.9%), every test faster by
  median. `_JS_CallInternal` sits at the same 64-byte offset in both
  binaries, so alignment does not skew the comparison.
- The full libquickjs suite with Test262 passes (58/83558 errors, none new),
  `-race` without Test262 passes, and internal/run-test262 was built from
  the new output as well.

### Answers to the questions above

- `registerLocal` pinning every local of a function with a switch in a loop:
  only with `--hot-switch <regexp>` (e469782, October 2025, no rationale
  recorded), which nothing in the fleet passes. It never produced a libquickjs
  frame.
- The `ccgo.go:11` TODO about escape analysis above "address taken" is about
  `&x` handed to a callee, that is step 2, not this.
- Step 2, pointer-typed parameters becoming `unsafe.Pointer`: feasible, but it
  belongs in the compiler, per translation unit and for static functions,
  rather than in the linker. The fixpoint over the call graph needs the C
  parameter types and the call sites, which the compiler has and the linker,
  which sees Go text, does not. Exported functions keep their `uintptr` ABI
  through a wrapper or a clone. Not started.
- The smaller items (VaList-only frames in a closure, libc `mem*` on Go
  pointers): not started.

### Hazards, checked against this change

1. No `uintptr` is formed: the address is `unsafe.Pointer(&v)` inside one
   expression.
2. Not applicable, no variable changes type.
3. Offsets are constant member offsets inside the variable.
4. Alignment: a Go local of a ccgo struct type keeps the C layout, and a
   member read at its C offset is at least as aligned as Go requires for the
   member type on every target.
5. Zeroing: unchanged, these variables were Go locals before whenever no
   member read pinned them.
6. Stack limit: no crash anywhere, TestStackOverflow passes, and
   nesting up to 10^6 levels ends in a JS exception in all nine parser cases.
   The parser depths at the default limit are unchanged within +3. JS
   recursion through accessors, bound functions, toString, generators,
   proxies and replace callbacks loses one frame per level, so the same
   MaxStackSlots=5000 allows deeper recursion: getter 1665 -> 2498 levels
   with 4.5 -> 8.7 MB of goroutine stack, toString 1248 -> 1665 (2.4 -> 4.5
   MB), proxy 999 -> 1249. Plain recursion, forEach, apply and JSON are
   unchanged. The worst case at limit 10000 is 17.1 MB, far below Go's 1 GB
   (250 MB on 32-bit). An observable change: libquickjs may retune
   MaxStackSlots.
7. setjmp: Go locals survive panic/recover; the setjmp lowering is unchanged.

### Noted, not changed

- `(c ? a : b).u.x` and `(a = b).u.x`, a union member of a conditional or an
  assignment expression, fail with a TODO (`"ccv1" from=V struct 16
  exprUintptr -> to=pointer to V`) on master too. Unrelated to frames.

## Resolution, step 2 (2026-10-22)

Implemented in ccgo: goptr.go (the analysis, the predicates and the emission
helpers), the mode `exprUnsafePointer` in expr.go with branches at the
dereference, comparison, assignment, increment and call sites, the signature,
local declaration and wrapper emission in decl.go. Flag: `--goptr=on|off|all`,
default on; `off` restores the previous output, `all` converts every safe
variable whether or not a local's address reaches it, which is the stress
mode the test suite also runs in (`go test -goptr=all`). Test: `TestGoPtr` in
goptr_test.go. The smaller items stay open.

### What is converted

A pointer-typed parameter of a function defined in the translation unit, or a
pointer-typed local, becomes a Go `unsafe.Pointer` when every use of its value
is one of:

- a dereference, as `*p`, `p->m`, `p[i]`, and any member, element or address
  taken of the object so reached; the object may be read, written, compound
  assigned, incremented, passed or returned by value;
- a comparison with another pointer or with NULL, or a use as a condition;
- `p + i`, `p - i`, a cast to another pointer type, `p++`, `p += i`, where the
  result is used the same way;
- an assignment to another such variable, or an argument of a direct call
  whose parameter is such a variable: a greatest fixpoint over the call graph
  of the translation unit, so mutually recursive and chained out-parameters
  work.

Everything else is a sink: a store into memory, a return, a conversion to an
integer, an argument of an indirect call, of a libc or any other external
function, of an inlined function or of a variadic position, a va_list, a
comparison with or an assignment of a non-zero integer constant (a C sentinel
in a Go pointer slot is an invalid pointer to the runtime), a bitfield or a
volatile object reached through the pointer, a conditional or a statement
expression yielding the pointer, and anything the classifier does not list.

A safe variable is converted only when the address of an automatic variable
can reach it, directly or through other converted variables, so that code
without such calls is generated exactly as before. The address itself is
produced as `unsafe.Pointer(&v)`, `unsafe.Add(unsafe.Pointer(&v), off)` for a
member or a constant array index, without pinning v. A variable pinned by
another use is addressed through its frame slot and still passed as an
`unsafe.Pointer`.

A function with a converted parameter is emitted with the new signature. An
exported function, or a static one referenced other than by a direct call,
keeps a wrapper with the original name and the uintptr signature that converts
the arguments and calls the body, which is emitted as `name__ccgo_gp`; the ABI
seen from other translation units, through function pointers and from Go stays
the same. This is what makes the QuickJS chains work: `JS_ToInt64Clamp`,
`JS_ToInt32Sat`, ... are exported but their out-parameters flow into them from
static callers with `&local` arguments.

### Recursion stays bounded

The libquickjs session found that the conversion removed the only frame from
11 functions that each close a recursive cycle of the parser, the module
linker, JSON and the string ropes. libquickjs bounds recursion by comparing
`libc.TLS.StackSlots` with a limit, which only works while every cycle holds
a frame; without one, `eval("!".repeat(1e7) + "1")` ended in Go's fatal
goroutine stack overflow instead of a SyntaxError.

`goPtrFrames` keeps that bound: over the direct calls within the translation
unit, with ccgo-inlined functions folded into their callers, every recursive
cycle whose remaining frames the conversion took away gets a one-slot frame
(`tls.Alloc(16); defer tls.Free(16)`) in one of the functions that lost its
frame, the first in translation unit order. A function lost its frame when
its first pass leaves it without one while the analysis saw the address of
one of its locals flow into a converted variable. Such a function emits a
marker where the frame would go; once the whole unit is emitted and every
frame is known, the cycles are computed and each marker becomes the frame or
nothing. A cycle that never had a frame stays as it was. On libquickjs this
restores a frame in 10 functions, the 11 above minus `js_parse_var`, whose
cycle `js_parse_function_decl2` covers, and the cycle check of the
libquickjs session then lists exactly the 16 frameless cycles the base has.
Its tests on that output: `"!".repeat(1e7) + "1"` ends in `SyntaxError:
stack overflow` with 1.7 MB of goroutine stack, `JSON.stringify` of nested
arrays stops with `InternalError: stack overflow` at every depth in about
160 ms, and every nested parse form (`!`, `typeof`, `void`, `a=`, `x=>`,
`?:`, spread, `new`, calls, members, `if`, `while`, templates) throws.

Compiling quickjs.c takes 4.6 s instead of 4.2 s with the analysis, about
9% more, and 8% more memory.

### Soundness

- A converted variable is never read as a uintptr: the identifier handler
  returns the new mode, and converting it to any value mode is an internal
  error reported at compile time, not a silent conversion. The analysis
  therefore only has to agree with the emitter on which uses are safe; the
  `--goptr=all` run of the whole corpus is what checks that agreement.
- A pointer value derived from a converted variable stays an
  `unsafe.Pointer` in every temporary too: the temporaries for `(*p)++`,
  `*p += x` and similar are typed `unsafe.Pointer`.
- Go's escape analysis sees the same facts: a converted parameter is only
  dereferenced, compared, or passed to another such parameter, so it does
  not leak and the caller's local stays on the stack (libquickjs: 0 "moved to
  heap", see below).

### Numbers (libquickjs, linux/amd64)

Regenerated from libquickjs ba53053 in a scratch clone with ccgo d5de9df
plus this change:

- Functions with a TLS frame: 989 without either step, 576 after step 1
  (measured by the libquickjs session on 2026-10-21), 346 after step 2, of
  1893 in libquickjs; internal/run-test262: 867 -> 488 -> 277.
- 240 functions take at least one `unsafe.Pointer` parameter, 44 of them
  through a wrapper: the exported `JS_ToInt32Sat`, `JS_ToFloat64`,
  `JS_ToCStringLen2`, `JS_NewCFunctionData`, `js_realloc2`, ... and the
  address-taken statics in the C function tables such as `js_array_push`,
  `js_array_includes`, `js_regexp_exec`, which are also called directly with
  `&local` arguments.
- `go build -gcflags=-m`: 0 "moved to heap".
- `go test -race -skip Test262 -run Test`: passes (76.9 s).

Measured by the libquickjs session on 2026-10-22 on the same output, before
the recursion fix, which only adds the one-slot frames:

- The full suite with Test262 passes (179 s); Test262 stays at 58/83558
  errors, none new. `-race` without Test262 passes, no checkptr report.
- BenchmarkMicro, five alternating rounds, `-test.cpu 4`, medians: total
  14,849.5 -> 14,590.8 ns (-1.7%, geomean -1.8%, 31 tests faster, 35
  slower). `_JS_CallInternal` moved from a 32 mod 64 to a 0 mod 64 offset;
  with a padding function restoring the base alignment the total is 14,877.0
  (+0.2%, geomean +1.3%). The alignment alone is worth 2% of the total and
  up to 13% on single tests, so the time effect of step 2 on libquickjs is
  within the alignment noise.
- Why so little: after step 1 the remaining frames cost about 8% of the run
  (CPU profiles of 42 s). Step 2 removes 20% of the frame entries but mostly
  cheap ones: `TLS.Alloc` 1.92 -> 1.72 s, `TLS.Free` 0.55 -> 0.42 s, the
  defer machinery 1.47 -> 1.18 s, about 1.4% in all. The biggest remaining
  frames belong to `js_array_cmp_generic` (18% of the Alloc entries) and
  `JS_ToPrimitiveFree` (16%), which pass a local `argv` to `JS_Call`, a real
  sink, then `JS_CallInternal`, `js_call_c_function` and
  `JS_ToNumberHintFree`, where the address of a local reaches a converted
  parameter through a ccgo-inlined header function (`JS_ToUint32` in
  quickjs.h calls `JS_ToInt32`) and the local stays pinned: closing that gap
  is worth about 1% more at most.

The libquickjs session's read, shared here: step 2 is correct and passes
everything, but its speedup on libquickjs is too small to measure. Other
code with more pointer out-parameters to locals may gain more; whether the
complexity is worth keeping is the user's call.

### Hazards, checked against this change

1. No `uintptr` outlives an expression: converted variables and the
   temporaries derived from them are `unsafe.Pointer` typed.
2. Sentinels: a constant other than NULL assigned to, passed as or compared
   with a converted variable makes it a sink. A sentinel loaded from memory
   into a converted variable is not detectable; the variable is then
   dereferenced by the same function, so the C program would be in trouble
   too.
3. Pointers outside the object: `&v[i]` with a non-constant index and
   arithmetic on `&v` or on a decaying array with a non-constant offset keep
   the old path, which pins v. Arithmetic on a converted parameter, which points to the caller's
   object, is the C program's responsibility as before.
4. Alignment: unchanged, the memory is the same.
5. Zeroing: unchanged, these variables were Go locals whenever nothing pinned
   them.
6. The stack limit: see "Recursion stays bounded" above. Where a cycle keeps
   a frame in another function than before, the depth at the limit changes
   but stays bounded; the libquickjs session re-measures.
7. setjmp: a converted parameter or local is a Go local like any other; the
   guarded region runs in a closure, which Go handles.

### Noted, not changed

- libc `memset`/`memcpy`/`memmove`/`memcmp` on Go pointers (the second
  smaller item) would make `fill(S *s)`-style callees convertible; today any
  libc call on a derived pointer is a sink.
- A `setjmp` region in ccgo returns the function's zero result after the
  longjmp instead of continuing after the `if`: `withjmp` in the scratch test
  returned 0 where gcc returns 10, with `--goptr=off` as well. Pre-existing,
  see `../../docs/handoffs/setjmp-recover-handoff.md`.
