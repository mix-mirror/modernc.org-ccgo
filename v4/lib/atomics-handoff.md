# Handoff: complete the C11 `<stdatomic.h>` lowering (`__atomic_load` / `__atomic_exchange`)

Status: **RESOLVED** on branch `wa2go`. Opened against ccgo `v4.34.4-20-g87f6a6a`.

## Resolution

The blocker was **not** in `stdatomicLoad` / `stdatomicExchange` — those handlers
already lowered correctly to the width-specific libc helpers
(`libc.X__atomic_loadUint32`, `libc.X__atomic_exchangeInt64`, …) via the
out-parameter ABI. The real gap was one level up: the **GNU statement-expression**
that the glibc macros wrap the builtins in could not be surfaced as a value.

`atomic_load_explicit` / `atomic_exchange` expand to
`({ …; __atomic_load(p, &tmp, mo); tmp; })`. The yielded `tmp` has type
`__typeof__((void)0, *p)`, which cc/v4 keeps volatile/atomic-qualified, so
`isVolatileOrAtomicExpr(n)` is true for the whole `*cc.PrimaryExpression`
statement-expression. The `PrimaryExpressionStmt` handler in `expr.go` already
surfaces the value correctly (through the plain local autovar `v`), but it never
set `volatileOrAtomicHandled`, so the guard in `(*ctx).expr` (`expr.go:117-119`)
fired the TODO. `atomic_store_explicit` was unaffected because its
statement-expression is void-typed (nothing to surface).

Fix: in the `PrimaryExpressionStmt` handler, mark the result handled when the
node carries a volatile/atomic type — a statement-expression performs no volatile
access *at its own level*; any access inside is emitted (and flagged) by the
inner expression handlers, and the result flows out through `v`. One guarded
`b.volatileOrAtomicHandled = true`. This fixes the whole class of value-yielding
volatile/atomic-typed statement-expressions, not just these two builtins.

Verified (translate → `go build` → run): the two reproducers below; a
store-5/fetch_add-3/load→8 / exchange→old-8-leaves-1 program; all of
1/2/4/8-byte signed+unsigned widths; the clang `__c11_atomic_*` spelling (which
took a different, already-working direct-call path); and the wasm2c-style
`(_Atomic volatile T*)(mem+addr)` cast-into-buffer pattern. No golden drift
attributable to the change.

---

Original report (for context):

## TL;DR

ccgo translates *most* of the GCC `__atomic_*` builtins that glibc's
`<stdatomic.h>` expands to, but **`atomic_load_explicit` and `atomic_exchange`
hit a `TODO`** — they expand to the out-parameter builtins `__atomic_load` /
`__atomic_exchange` wrapped in a GNU statement-expression that yields the result
through a temporary, and ccgo can't render that as a value (`toMode exprDefault`
on the enclosing `*cc.PrimaryExpression`). Completing these two unblocks C11
atomic loads/exchanges.

## Why this matters (motivation)

This is the single hard gate for **WebAssembly threads** support in
`modernc.org/wa2go` (wasm → C via wasm2c → Go via ccgo). wasm2c's
`--enable-threads` output lowers Wasm atomic ops to exactly this C11 surface
(`atomic_load_explicit` / `atomic_store_explicit` / `atomic_fetch_*` /
`atomic_exchange` over `_Atomic volatile` casts into linear memory). Everything
else the threads path needs already exists in the modernc stack — pthreads as
goroutines each with their own `*libc.TLS`, a shared off-heap linear-memory
buffer that (thanks to wa2go's non-moving mmap-reserve memory) does not move on
`memory.grow`. So a threaded Wasm module currently fails to translate *only*
because of these two atomic builtins. The fix is also useful on its own: any
C using C11 atomic loads/exchanges benefits.

## The gap, precisely

`<stdatomic.h>` (glibc) expands the generic-form atomics into the out-parameter
GCC builtins inside a statement-expression. ccgo dispatches them in
`expr.go` (the builtin `switch`):

```
expr.go:2219  case "__atomic_load", "__atomic_store":   -> c.stdatomicLoad(...)      // expr.go:2813
expr.go:2228  case "__atomic_exchange":                 -> c.stdatomicExchange(...)  // expr.go:2916
```

- `__atomic_store` (void) works — there is no result to surface.
- `__atomic_load` and `__atomic_exchange` **TODO** — they must surface the
  loaded/exchanged value out of the `&tmp` out-parameter, and the handler does
  not produce a usable value in the statement-expression / `exprDefault` context.

The working sibling handlers that *do* surface a value in the same context are
the implementation references to mirror:

```
expr.go:2780  stdatomicFetchAdd          (returns the value directly — works)
expr.go:2993  stdatomicCompareExchange   (surfaces bool + writes *expected — works)
```

(The `__c11_atomic_*` variants — `c11AtomicLoad`/`c11AtomicExchange`, expr.go
~2224–2232 — are a separate spelling clang emits; this report is about the glibc
`__atomic_*` spelling, but check whether the same fix covers both.)

## Reproducer

```c
// load.c — atomic_load_explicit
#include <stdatomic.h>
#include <stdint.h>
static _Atomic volatile uint32_t cell;
int main(void){ return (int)atomic_load_explicit(&cell, memory_order_seq_cst); }
```

```
$ go mod init repro && go get modernc.org/libc@latest
$ ccgo -o load.go load.c
TODO load.c:4:29: "({ __auto_type __atomic_load_ptr = (&cell);
  __typeof__ ((void)0, *__atomic_load_ptr) __atomic_load_tmp;
  __atomic_load (__atomic_load_ptr, &__atomic_load_tmp, ( memory_order_seq_cst));
  __atomic_load_tmp; })" ... *cc.PrimaryExpression uint32_t, toMode exprDefault
```

`atomic_exchange(&cell, 1)` fails the same way via `__atomic_exchange`:

```
"({ __auto_type __atomic_exchange_ptr = (&cell);
  __typeof__(...) __atomic_exchange_val = (1);
  __typeof__(...) __atomic_exchange_tmp;
  __atomic_exchange (__atomic_exchange_ptr, &__atomic_exchange_val,
                     &__atomic_exchange_tmp, (5));
  __atomic_exchange_tmp; })" ... toMode exprDefault
```

## Verified per-op status (current)

Each op made reachable from `main` (so it is not dead-code-eliminated — note that
a `-o` build of unreferenced functions "passes" only because their bodies are
never translated):

| C11 op | builtin | result |
|---|---|---|
| `atomic_load_explicit`   | `__atomic_load`     | **TODO** |
| `atomic_exchange`        | `__atomic_exchange` | **TODO** |
| `atomic_store_explicit`  | `__atomic_store`    | ok (void) |
| `atomic_fetch_add/sub/and/or/xor` | `__atomic_fetch_*` | ok |
| `atomic_compare_exchange_strong`  | `__atomic_compare_exchange` | ok |

## What "done" looks like

1. The two reproducers above translate, build, and **run correctly** — e.g. a
   program that `atomic_store`s 5, `atomic_fetch_add`s 3, then
   `atomic_load`s prints `8`; an `atomic_exchange(&cell, 1)` after storing 5
   returns 5 and leaves 1.
2. Lower to Go `sync/atomic` (or the libc atomic helpers used by the working
   handlers) at the right width (1/2/4/8 bytes, signed/unsigned): `__atomic_load`
   → `atomic.Load{Uint32,Uint64,…}`, `__atomic_exchange` → `atomic.Swap…`.
   Memory orders can start as sequentially-consistent (Go atomics are seq-cst);
   the order argument (the trailing `int`) can be ignored initially.
3. Spot-check the `_Atomic volatile` cast and the `(void)0, *ptr` `__typeof__`
   type expression are handled (they appear in the macro expansion).

## Downstream validation

After the builtins land, the end-to-end check is a wa2go threaded module:
`wat2wasm --enable-threads` + `wasm2c --enable-threads` produce the C11 surface
above over a `wasm_rt_shared_memory_t`; wa2go then needs a `wasm_rt_*_memory_shared`
mem variant and a `wasi:thread-spawn` host, but **the atomic builtins are the
prerequisite** — until they translate, nothing downstream can be exercised. See
`modernc.org/wa2go` ROADMAP → "Further out" → threads.
