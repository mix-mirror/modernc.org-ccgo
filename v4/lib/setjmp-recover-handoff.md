# Handoff: the `setjmp` lowering swallows every panic (silent `exit(0)` on a fault)

Status: **OPEN**. Reported against ccgo `v4.34.7-0.20260805164225-484bae2893ff`.
Filed by the `modernc.org/xetex` side; **no ccgo change has been made** — this is
a request for independent scrutiny, because the fix touches codegen that every
ccgo consumer inherits.

## TL;DR

All four `setjmp` lowerings in `stmt.go` emit a deferred `recover()` in a type
switch whose `default:` clause **returns normally**. `recover()` stops *any*
panic, not just `libc.LongjmpRetval`, so a Go panic raised anywhere inside a
`setjmp`-guarded region is silently absorbed and the enclosing function returns
its zero values. A C null-pointer store that segfaults natively instead prints
nothing and lets the program continue with a wrong result and **exit status 0**.

The one-line shape of the fix is to re-panic non-longjmp values in `default:`.
It is verified working below, but the blast radius is every generated package,
so please review rather than take my word for it.

## Why this matters (motivation)

Found while debugging `modernc.org/xetex` (web2c XeTeX → wasm → C → Go via
`wa2go`, which uses ccgo for the C→Go half). The engine died mid-`\dump` and the
first instrumented build — one that `panic`ed deliberately to report a wasm trap
code — **exited 0 with a half-written output file** instead of printing anything.
That turned a one-hour bug into a session: the diagnostic could not escape.

But the wasm route is incidental. The reproducer below is plain C compiled by
ccgo alone, and the consequence is general: **inside a `setjmp` region, ccgo
downgrades every Go-visible fault into silence.** `modernc.org/sqlite` and
anything else using `setjmp` for error recovery inherit it. Silent wrong answers
are worse than crashes, which is why this is worth your time even though nothing
in the corpus currently fails because of it.

## The defect

`stmt.go` has four entry points — `setJmpNeq0` (499), `setJmpEqM1` (575),
`notSetJmp` (651), `setJmpEq0` (747). `notSetJmp` and `setJmpEq0` each emit two
variants (a `stmtHasJump` fallback and a closure form), so there are **six
emission sites**, all the same shape:

```go
tls.PushJumpBuffer(jb)
defer func() {
	switch recover().(type) {
	case libc.LongjmpRetval:
		stmt1
	default:
		tls.PopJumpBuffer(jb)   // <- panic already recovered; falls off the end
	}
}()
stmt2
```

Two facts combine:

1. `recover()` in a deferred function stops the panicking sequence
   unconditionally. There is no "only recover this type" form — the type switch
   selects a *branch*, it does not decline the recovery.
2. `default:` is reached both when there was no panic (`recover()` → `nil`) and
   when there was a non-longjmp panic. Both then return normally.

So the deferred function cannot tell "clean exit" from "the try region blew up",
and treats both as clean.

Note the doc comments above all four functions already show the intended shape
with the value bound — `switch x := recover().(type)` (e.g. `stmt.go:490`) — but
the emitted code drops the `x :=`, so `default:` has nothing to re-panic even if
it wanted to. That reads like the binding was lost rather than deliberately
removed; worth checking the history.

## Reproducer (pure ccgo — no wasm, no wa2go)

```c
#include <setjmp.h>
#include <stdio.h>
static jmp_buf jb;

static int faulted(void) {
	if (setjmp(jb) != 0) { printf("  faulted: WRONG, caught a fault as longjmp\n"); return -1; }
	printf("  faulted: about to fault\n");
	{ volatile int *p = (int *)0; *p = 42; }
	printf("  faulted: WRONG, survived the fault\n");
	return 0;
}

int main(void) { printf("faulted -> %d\n", faulted()); printf("main: returned normally\n"); return 0; }
```

`gcc` (correct — the process dies):

```
  faulted: about to fault
Segmentation fault
exit=139
```

`ccgo -o main.go main.c && go build && ./repro` (wrong — the fault vanishes):

```
  faulted: about to fault
faulted -> 0
main: returned normally
exit=0
```

The nil store raises a recoverable `runtime.Error`; `default:` eats it; `faulted`
returns its zero value; `main` carries on and reports success.

## Proposed fix, and what it was verified against

Bind the recovered value and re-panic it after popping:

```go
defer func() {
	switch x := recover().(type) {
	case libc.LongjmpRetval:
		stmt1
	default:
		tls.PopJumpBuffer(jb)
		if x != nil {
			panic(x)
		}
	}
}()
```

`x` is unused in the `LongjmpRetval` clause, which is fine — Go only rejects a
type-switch variable unused in *every* clause.

I applied exactly this by hand to ccgo's **generated output** (not to ccgo) for a
three-case program and rebuilt:

| case | native C | ccgo as-is | ccgo + fix |
|---|---|---|---|
| normal completion, no jump | exit 0, try ran | exit 0 ✅ | exit 0 ✅ |
| real `longjmp`, caught | exit 0, caught | exit 0 ✅ | exit 0 ✅ |
| nil store inside the try | **exit 139 (SIGSEGV)** | exit 0 ❌ silent | exit 2, panic reported ✅ |

Fixed output for the third case:

```
  faulted: about to fault
panic: runtime error: invalid memory address or nil pointer dereference [recovered, repanicked]
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4e1315]
```

Not byte-identical to C (Go exits 2 with a traceback, C dies on a signal), but
loud and diagnosable instead of silent, which is the point.

A second, quieter benefit: it fixes jump-buffer hygiene. Today a non-longjmp
panic never unwinds, so nesting is moot; with the fix each level pops its own
buffer on the way out, so `PopJumpBuffer`'s top-of-stack invariant still holds
when an outer `setjmp` region sees the propagating panic.

## Due diligence already done

**Is any panic legitimately being absorbed here?** I inventoried
`modernc.org/libc@v1.74.4`. The only panic value used as control flow is
`LongjmpRetval` (`pthread.go:115`, `libc_musl.go:477`, both from `TLS.Longjmp`).
Every other panic is a diagnostic: 921 `panic(todo(...))` sites, plus a handful
of `panic(<string>)` (unsupported inline asm) and a `panic(m)` for a bad `fopen`
mode. None of those should be swallowed. So the `default:` clause has nothing to
protect.

**`Longjmp` pops before it panics** (`tls.PopJumpBuffer(jb); panic(LongjmpRetval(val))`),
which is why the `case` branch correctly does *not* pop. The fix leaves that
alone.

**Corpus coverage exists** for the paths the fix must not regress:
`assets/github.com/vnmakarov/mir/c-tests/new/setjmp.c` is in
`testdata/test_exec_linux_amd64.golden`; `setjmp2.c` is "Won't fix" and two
`built-in-setjmp.c` are BUILD FAIL in `known_failures_linux_amd64_test.go`. I did
**not** run `make test` — see below.

## What I did not verify — please close these

1. **The corpus.** I did not run `make test` / `make shorttest`, so I do not know
   whether re-panicking changes any golden or known-failure entry. This is the
   main thing to check. My expectation is "no drift", because a corpus test that
   panicked under `setjmp` would already be failing loudly for another reason —
   but that is reasoning, not evidence.
2. **Whether `default:` should also pop on a non-longjmp panic.** I kept the pop
   (it matches the frame leaving), but if any consumer relies on the buffer
   surviving an abnormal unwind, that is a behaviour change.
3. **The `stmtHasJump` fallback forms** in `notSetJmp`/`setJmpEq0`. I only
   exercised the shape my reproducer generated. The other emission sites look
   identical but I did not build a case that reaches each one.
4. **Cross-target.** Verified on linux/amd64 only; nothing here looks
   target-dependent, but `make build_all_targets` is the check.
5. **Whether re-panic is the right choice versus something narrower**, e.g.
   converting the panic into a C-visible abort so behaviour matches the native
   segfault more closely. Re-panic is the smallest change; you may prefer
   otherwise.

## Adjacent findings, explicitly *not* ccgo bugs

Recorded so they are not re-diagnosed, and because they interact with the above.

- **`isSetJmp` matches only the literal identifier `setjmp`** (`stmt.go`), not
  `sigsetjmp`/`_setjmp`. Combined with libc having `Xsetjmp`/`Xlongjmp` but no
  `Xsiglongjmp`, a C program using `sigsetjmp` gets an unresolved `siglongjmp`.
  `wa2go` papers over this with a hand-written `_siglongjmp` stub that panics
  "unexpected call" — **and that panic is exactly what the bug above would
  swallow**. Whether `sigsetjmp` should be recognised is a separate question; I
  raise it only because the two defects mask each other.
- **Every wasm trap in `wa2go` output is a bare `abort()`.** That one is wa2go's:
  nothing arms `g_wasm_rt_jmp_buf`, so WABT's `WASM_RT_LONGJMP` takes its
  `if (!initialized) abort()` branch — exit 134, no trap code, no message. Fix
  belongs in wa2go via `-DWASM_RT_TRAP_HANDLER=`. Mentioned only because these
  two silences together are what made the original bug expensive.

## Context

The xetex-side write-up, including how the trap was eventually cornered, is in
`modernc.org/xetex/HANDOFF.md` under "Blocker A". The related wa2go fix that
unblocked xetex (`-DWASM_RT_MAX_CALL_STACK_DEPTH=100000`) is `wa2go 1bdb337`; it
is independent of anything here.
