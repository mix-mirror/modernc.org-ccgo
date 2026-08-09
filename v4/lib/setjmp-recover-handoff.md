# Handoff: the `setjmp` lowering swallows every panic (silent `exit(0)` on a fault)

Status: **RESOLVED and released** in `v4.35.0` (commits `850923f`, `4a53405`).
See "Resolution" at the end for what was changed, what was verified, and answers
to the five open questions. Reported against ccgo
`v4.34.7-0.20260805164225-484bae2893ff`. Filed by the `modernc.org/xetex` side.

Re-checked from the reporting side on 2026-08-09 against `v4.35.0`: the original
three-case reproducer (normal completion / real `longjmp` / nil store inside the
try) now gives exit 0 / exit 0 / exit 2 with
`panic: … [recovered, repanicked]`, where pre-fix it gave 0 / 0 / **0**. Nothing
outstanding from this repo's point of view.

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

## Resolution

The report is accurate and the defect is confirmed at all six emission sites, not
only the one the reporter's program reached. A reproducer driving each site
through three outcomes (normal completion, real `longjmp`, nil store) shows `gcc`
dying with SIGSEGV at every site while pre-fix ccgo continues and exits 0. The
two closure forms are worse than described: the enclosing function returns its
*correct* value, so the fault leaves no trace at all.

### What changed

`stmt.go` only, 6 emission sites now routed through two new helpers,
`setJmpRecover` and `setJmpRecoverDefault`, so the shape cannot drift apart
again. The four doc comments were updated to the shape actually emitted.

The proposed patch could not be used verbatim. `switch x := recover().(type)` is
not expressible here: the object file keeps identifiers tagged, so `type` is at
that point still the ordinary identifier `pptype` and only the tag substitution
done when linking turns it into the keyword. That leaves the guard form an
assignment where an expression is required — it does not parse, and ccgo fails
the object file at its `gofmt` step before linking ever runs. Binding the value
before the switch is valid Go both before and after the substitution:

```go
x := recover()
switch x.(type) {
case libc.LongjmpRetval:
	stmt1
default:
	tls.PopJumpBuffer(jb)
	if x != nil {
		panic(x)
	}
}
```

The behaviour is the one proposed. Non-longjmp panics propagate; the nil-store
reproducer now exits 2 with `panic: runtime error: invalid memory address or nil
pointer dereference [recovered, repanicked]` at every site, and the normal and
`longjmp` paths are unchanged.

### A sharper motivating case than the nil store

libc's own fallbacks `Xsetjmp` and `Xlongjmp` are `panic(todo(""))`. Any `setjmp`
in a shape the lowering does not recognise — `int rc = setjmp(jb);` is enough —
compiles to `libc.Xsetjmp`. Nested inside a recognised region, that panic used to
vanish: the program skipped the code and exited 0. It now reports
`panic: libc_musl.go:1055:Xsetjmp TODO [recovered, repanicked]`. So the clause was
not only swallowing user faults, it was swallowing ccgo's and libc's own
not-implemented diagnostics.

### The five open questions

1. **The corpus.** `make shorttest` passes, 884s on linux/amd64
   (`TestExec` + `TestSQLite`; `TestCSmith` is `-short`-skipped). The golden
   drifts by 4 lines, all of it host-cc noise unrelated to this change: three
   `mir/c-tests/lacc` entries the local gcc now rejects outright (implicit int,
   implicit function declaration) and one `gcc.c-torture` entry it now accepts.
   None of the four mentions `setjmp`. That drift was reverted, not committed.
   `assets/github.com/vnmakarov/mir/c-tests/new/setjmp.c` stays in the golden, so
   the setjmp corpus coverage is unchanged. No known-failure entry moved.
2. **Whether `default:` should also pop.** Yes, keep the pop, and for a stronger
   reason than "it matches the frame leaving": `PopJumpBuffer` panics unless the
   buffer it is given is on top of the stack, so leaving a stale entry behind
   would break the *enclosing* setjmp regions the panic unwinds into. Nothing can
   rely on the buffer surviving, because a Go panic that escapes the region can
   never be resumed back into it.
3. **The `stmtHasJump` fallback forms.** Covered — the reproducer reaches all six
   sites and the generated Go was checked to confirm each took the intended path.
   Both fallback forms behaved exactly like the closure forms, before and after.
4. **Cross-target.** `make build_all_targets` passes, all 21 GOOS/GOARCH pairs.
   Nothing in the change is target-dependent.
5. **Re-panic versus something narrower.** Re-panic is right. Converting to a
   C-visible abort would match the native segfault's exit status but destroy the
   diagnostic, which is the entire point; it would also be actively wrong for the
   `panic(todo(...))` diagnostics above, which are not faults. Re-panic preserves
   the original value and stack, and it restores the option of a consumer
   recovering the fault deliberately — impossible today.

### Adjacent defects found while verifying — none of them is this bug

All pre-date the recover fix and were confirmed against builds from before it.

**A. Two `setjmp` regions lexically nested in one function shared a jump buffer.
FIXED**, separately from the recover change. Both regions got the same autovar
(`var v1 uintptr`): the pool in `fnCtx.newAutovarTyp` recycles per statement via
`rewindAutovars`, but a jump buffer is read by the recovering defer long after the
statement that pushed it, so a nested region asking for one in between got the
same variable. The outer `PopJumpBuffer` was then handed the *inner* buffer's
address and panicked `unsupported setjmp/longjmp usage` — even on the path where
nothing jumps at all.

The fix keeps the recycling optimisation (`5975017 performance++: reuse some
autovars`) and exempts only values whose live range outlives their statement, via
`fnCtx.newLiveAutovarType`, which owns a variable per *site* rather than per
position in a handout order. Nested regions now match gcc on all four paths
(no jump, inner longjmp, fault, outer longjmp after the inner region exits).

Scope of the old bug, for the record: it needed the two regions to be lexically
nested **in the same function**. Sequential regions in one function were fine
(their live ranges do not overlap) and so was nesting across a call, the common
case — verified byte-identical generated code before and after the fix.

**B. The defer-hosted forms cannot resume the code after the `if`.** gcc returns
11/22/45/67 for the four such sites in the reproducer, ccgo returns 0. This is
*not* an oversight that switching them to the closure form would repair — see C.
It is inherent: the catch runs inside the recovering defer, and a Go panic cannot
be resumed. It does not bite the canonical idiom
`if (setjmp(jb)) { cleanup(); return -1; }`, because a `return` in the catch is
routed to the result variable by the `fnCtx.inDefer` mechanism. It bites only when
the catch falls through to code after the `if`.

**C. The closure form pops the jump buffer too early — a regression from
`ef21892`.** In C, `setjmp` arms the buffer until the enclosing function returns,
so a `longjmp` from code *after* the `if` still lands at the `if`. The closure
form pops on normal completion of the try, so that `longjmp` finds an empty stack:

```c
static int eq0(void) {
	if (setjmp(jb) == 0) { printf("  try\n"); }
	else { printf("  catch\n"); return -1; }
	printf("  after if\n");
	g();				/* longjmps to jb */
	return 0;
}
```

gcc, and ccgo built from `ef21892~1`, both print `try / after if / catch` and
return -1. Broken ccgo printed `try / after if` and then panicked
`libc_musl.go:466:PopJumpBuffer TODO unsupported setjmp/longjmp usage`.

What it came down to: the two lowerings modelled *different* things and neither was
right on both axes. The defer form gets the buffer's C lifetime right and cannot
resume; the closure form can resume and truncated the lifetime to the try. Since
`if (setjmp(jb) == 0) A; else B;` and `if (setjmp(jb) != 0) B; else A;` are the
same program, ccgo compiled logically identical C two different ways — the second
spelling handled the case above correctly, the first did not.

**FIXED** by giving the closure form both handlers. The closure keeps reporting a
longjmp caught during the try, so `ef21892`'s value-producing catch still works,
and a function level defer now owns the pop and hosts a second copy of the catch
for a longjmp arriving once the closure can no longer see it. Since `Longjmp` pops
on its way out, the buffer is pushed again before the catch runs at the level of
the if: in C it stays armed.

Leaving the buffer armed was at first restricted to functions containing a single
`setjmp`, because C keeps every buffer of a live frame valid while libc kept armed
buffers on a stack whose top `Longjmp` required as its target, and the two agree
only while at most one buffer per function is armed. That restriction is gone, see
D. What remains of it is a region inside a loop, where the push and the function
level defer would happen per iteration — worth 100MB per million iterations in a
measurement — and a catch defining a label, which cannot be emitted twice.

**D. `longjmp` past an intervening active region was unsupported. FIXED**, in libc
and ccgo together, which also removed the restriction above.

The mismatch was that C treats armed jump buffers as a set, any of which a longjmp
may target, while libc treated them as a stack whose top `TLS.Longjmp` required.
Jumping from an inner region straight to an outer one panicked `unsupported
setjmp/longjmp usage` where gcc runs the outer catch.

libc now lets `Longjmp` disarm its target wherever it sits, and `LongjmpRetval`
carries that target rather than only the value setjmp must appear to return. The
generated code needs the target because a panic unwinds through every region
between the longjmp and its destination: each compares the buffer named in the
value with the one it armed, and unless they are equal disarms its own and
re-panics, leaving the value for the region it was meant for. Of two regions
sharing a buffer the innermost is disarmed, which is the one C resumes at.

The corpus shows it: `assets/github.com/vnmakarov/mir/c-benchmarks/except.c`, a
nested exception benchmark that longjmps to whichever of two buffers matches the
exception, was listed in `known_failures_linux_amd64_test.go` under "Won't fix:
setjmp/longjmp". It passes now and has moved into the golden, its known-failure
entry dropped. The other 22 platform tables still list it; the fix is not target
dependent, but only linux/amd64 was verified here.

Both sides are required. New generated code against an old libc does not build,
`LongjmpRetval` having become a struct. Old generated code against a new libc
builds but is degraded: it does not compare, so a longjmp aimed past it is
recovered rather than passed on. `objectFileSemver` moves to `v2` so that object
files predating the check cannot be linked into a program that relies on it, and
libc's own package documentation already states the rule for the source level —
recompile with a matching ccgo rather than upgrading libc alone.

### Divergences from the C reference that remain

Measured by building each reproducer with gcc and with ccgo and comparing output
and exit status. Everything not listed matches gcc exactly, including nested and
sequential regions, nesting across a call, the value-producing catch, a longjmp
aimed past an active inner region, and a longjmp from the tail of a function
holding two regions.

| case | gcc | ccgo |
|---|---|---|
| fault inside a guarded region | dies on SIGSEGV, exit 139 | panics, exit 2 — the point of the fix, loud either way |
| longjmp caught by a defer hosted catch that falls through past the `if` | runs the code after the `if` | returns from the function, defect B |
| `setjmp` in a shape the lowering does not match | works | panics `Xsetjmp TODO`, and no Go implementation of `setjmp` is possible |

### Not done

No regression test was added to the repo. The corpus is the external
`modernc.org/ccorpus2` module and `testdata/overlay/` holds only `.arg` files, so
a C regression case for this belongs upstream in ccorpus2 rather than here.
