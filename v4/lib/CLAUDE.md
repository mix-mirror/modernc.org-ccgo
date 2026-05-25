# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository layout

The current working directory (`v4/lib`) is the implementation package; the command lives one level up at `../` (`modernc.org/ccgo/v4`) and is a thin wrapper that selects between `v3/lib` and this `v4/lib`. Older `v1`, `v2`, `v3` directories at the repo root are kept only for backward compatibility — work happens in `v4/lib`. `go.mod` is at `../go.mod` (one directory up).

## Build / test commands

All `make` targets must be run from `v4/lib`.

- `make editor` — regenerate `stringer.go`, `gofmt`, `go test -o /dev/null -c` (compile-check tests), `go install` of the command with `-tags=ccgo.assert,ccgo.dmesg`, then `staticcheck`. Run this after edits.
- `make test` / `make shorttest` — both run `go test -v -timeout 36h -short -failfast -tags=ccgo.assert -trc -shelltimeout 1m`, then `git status testdata && git diff testdata/`. The diff catches drift in the per-target `testdata/test_exec_<goos>_<goarch>.golden` lists.
- `make build_all_targets` — cross `go build` + `go test -c` for the full GOOS/GOARCH matrix listed in the Makefile. Use to confirm a change does not break platforms you can't run locally.
- `make work` — regenerate `../go.work` pulling in adjacent worktrees of `modernc.org/cc/v4` and `modernc.org/gc/v2`. Use when developing cross-repo against unreleased upstream changes.
- `make clean` — remove `log-*`, `cpu.test`, `mem.test`, `*.out`, and `go clean`.

### Build tags

- `ccgo.assert` — enables `assert == true` (used by `if assert { ... }` cheap-runtime asserts). The test target sets it. `noassert.go` flips it off when the tag is absent.
- `ccgo.dmesg` — `dmesg()` writes timestamped lines to `<os.TempDir()>/ccgo.log` (init-opened, append, sync). `nodmesg.go` is the no-op stub.

### Running a single test / corpus subset

- `go test -run TestExec/<subpath>` to focus a `TestExec` subtest (subtests are named after the corpus directory, e.g. `tcc-0.9.27/tests/tests2`).
- `-re <regex>` — filters individual corpus C files by `filepath.Base` match. Defined in `TestMain`, applies to `TestExec` and `TestCSmith`.
- `-trc` (already set by `make`) — prints each tested path. `-trctodo` prints origin of every `TODO`-tagged error returned via `errorf`.
- `-trcc` / `-trccc` / `-trcf` / `-trco` — trace transpile errors, C compile errors, file content, output, respectively (see flag block at top of `all_test.go`).
- `-keep` — keep the per-test temp directory after the run. `-xwork=<dirs>` makes `TestExec`/`TestCSmith` set up a `go.work` pointing at the listed packages instead of `go get`ing the pinned libc version from `../go.mod`.
- `-panic` — `panic` instead of report on a miscompilation, useful for stack traces during debugging.

### Test corpus and golden files

`TestMain` mounts `modernc.org/ccorpus2` (via `ccorpus2.FS`) overlaid with `testdata/overlay/`; corpus paths look like `assets/<corpus>/.../file.c`. After a successful `TestExec`, paths land in `testdata/test_exec_<goos>_<goarch>.golden`. Diffs in those files are part of the test signal — review them before committing.

### Known-failure tables

`known_failures_<goos>_<goarch>_test.go` defines the platform-specific `testExecKnownFails` map (corpus path → struct{}). A path here is *skipped on failure*, not skipped outright: if the C reference itself fails the same way, the test passes. When fixing or breaking a corpus case, edit the file matching the build's actual `runtime.GOOS_runtime.GOARCH`. Each entry is annotated with the failure category (Won't fix / TODO / EXEC FAIL / BUILD FAIL / VET FAIL / COMPILE FAIL).

### Long-running tests

- `TestCSmith` is skipped under `-short`. Default duration is `-csmith=1h` with per-binary timeout `-csmithc=1m`. It uses an external `csmith` binary; skipped on windows/arm64.
- `TestSQLite` builds and runs SQLite's amalgamation through ccgo. Skipped on Windows (windows-specific link gaps documented inline).

## Architecture

### The Task pipeline

`Task` (`ccgo.go:47`) is the compiler driver. `NewTask(goos, goarch, args, stdout, stderr, fs)` then `task.Main()`. Main routes:

1. `task.Exec()` — invoked when the env var `CCGO_EXEC_ENV` is set; ccgo is acting as a shim for `cc`/`ar`/etc. inside a `-exec` subprocess.
2. `task.main()` — normal flag parsing (`modernc.org/opt`-based `set`) in `ccgo.go:215`. **Add new CLI flags here** alongside the existing `set.Arg`/`set.Opt` registrations.
3. After flag parsing, the task either preprocesses (`-E`), compiles (`-c` → `task.compile`), or compiles+links (`task.link`). `-exec` reruns the task via `task.exec` (in `exec.go`), interposing on a host build system.

### Per-file compilation

`task.compile` (`ccgo.go:799`) dispatches each input `.c` to a fresh `newCtx(task, eh).compile(ifn, ofn)` (`compile.go`). The `ctx` (`compile.go:233`) is the per-translation-unit state — every method in `decl.go`, `stmt.go`, `expr.go`, `init.go`, `type.go` hangs off `*ctx`. It owns the C AST (`cc.AST`), the import map, the type/name namespaces (`anonTypes`, `enumerators`, `taggedStructs`, …), inline-function state (`inlineFuncs`, `inlineLabelSuffix`), and the multi-pass control (`pass` field: 0 out of fn, 1 fn 1st pass, 2 fn 2nd pass — a function is walked twice so addresses-taken decisions are stable before output).

File-by-file responsibility:

- `compile.go` — `ctx`, the writer interface (`buf`), object-file metadata (`jsonMeta` carrying weak/canonical aliases and visibility), top-level orchestration of one translation unit.
- `decl.go` — declarations, function definitions, parameter handling, `declInfo`/`declInfos` tracking `addressTaken`/`pinned` (any `auto` whose address is taken gets pinned via `bp` stack frame), `fnCtx` per-function state.
- `stmt.go` — C statements → Go statements; `c.statement(w, n)` dispatches on `cc.StatementCase`.
- `expr.go` — the bulk of the translator (~5200 lines). Every expression method takes `(w writer, n cc.ExpressionNode, to cc.Type, toMode mode)`. The `mode` enum (`expr.go:19-31`) drives codegen: `exprBool`, `exprCall`, `exprDefault` (rvalue), `exprIndex` (array indexable), `exprLvalue`, `exprSelect` (struct), `exprUintptr` (raw C pointer), `exprVoid`. Atomic builtins, `__sync_*`, vector ops, and many GCC builtins are handled here.
- `init.go` — C initializer flattening and per-type emission (scalar / array / struct / union / bitfields).
- `type.go` — C type → Go type rendering; `typ`, `typedef`, `helper`, `verifyTyp` variants choose whether to use named typedefs/tags or expand.
- `link.go` — second phase. Parses each `.o.go` with `modernc.org/gc/v2`, collects extern vars/types/consts (`object.collect*`), resolves cross-file references, dead-code-eliminates via `internal/secret_sauce`, and writes one merged Go file. ~2200 lines; significantly larger than the C front-end glue.
- `exec.go` — `-exec` mode: install ccgo as `$PATH/cc`, `ar`, etc., then `exec` the wrapped build system so its `cc`/`ar` invocations route back through ccgo.
- `etc.go` — `errorf`/`todo`/`trc`/`origin` helpers, `nameSet`/`nameSpace` symbol bookkeeping, reserved identifier table, parallel test plumbing.

### Identifier tagging

C identifiers carry a 2-letter Go-side prefix encoding their kind. The table is `tags` in `ccgo.go:79-99` (also reflected in `stringer.go` for the `name` type). Examples: `X` external, `tn` typename, `ts` tagged struct, `te` tagged enum, `mv` macro value, `fd` field, `si` static internal, `aa` auto. **Never change a `name`→prefix mapping** without bumping `objectFileSemver` in `ccgo.go:43` — it is the on-disk ABI of `.o.go` files. The semver is validated at `init()`.

The `helper` and `preserve` (`pp`) prefix marks identifiers reserved for the runtime (e.g. `iqlibc.ppTLS` → `libc.TLS` after qualifier substitution). The `__ccgo_fp` / `__ccgo_fp_` / `__ccgo_ts` / `__ccgo_up` magic names in `expr.go:34-48` carry function-pointer, ABI0-vs-internal, thread-storage and `unsafe.Pointer` semantics.

### Transpiled-code ABI (consumers of generated output)

Generated Go uses C-heap memory via `modernc.org/libc` (selectable with `--libc`, default `modernc.org/libc` aka v1; alternative `modernc.org/libc/v2`). **Never pass a Go pointer into a transpiled function** — cast Go data into memory from `libc.Xmalloc` first. See `README.md` for the canonical workflow. This rule also explains why `go test -race` is hostile to generated code and why `-xvet` is currently default-off (see issue #45).

### Internal helpers

- `internal/secret_sauce` — Go-AST dead-variable elimination using `modernc.org/gc/v3`, called from the linker.
- `internal/autogen` — empty placeholder in the checked-out tree.

## Debugging conventions

- `trc(format, args...)` (`etc.go:360`) — stderr printf with caller `file:line:func`, intended for ad-hoc debugging; commits should not leave new `trc` calls behind. Sprinkled throughout as `// trc(...)` placeholders.
- `todo(...)` (`etc.go:346`) — marks not-yet-implemented branches; pair with `c.err(errorf("internal error %T %v", n, n.Case))` for unreachable-case complaints. With `-trctodo`, any `errorf` whose message starts with `TODO` prints its origin.
- `errorf(...)` (`etc.go:403`) — error constructor. With `-extended-errors` (or `extendedErrors = true`, which `TestMain` sets), errors carry a 6-deep call-origin chain; this is what `TestExec` prints when reporting a failure.
- `dmesg(...)` — `<os.TempDir()>/ccgo.log` when built with `-tags=ccgo.dmesg`.

## Stringer-generated code

`stringer.go` is regenerated by `make editor` via `stringer -output stringer.go -type mode,name`. Do not hand-edit it.

## Tooling pinned in `../go.mod`

The C front-end is `modernc.org/cc/v4`. The Go parser used by the linker is `modernc.org/gc/v2`; `modernc.org/gc/v3` is used by `internal/secret_sauce` only. Most platform-specific arithmetic uses `modernc.org/mathutil`. Test corpus is `modernc.org/ccorpus2`.
