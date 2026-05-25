# Code review findings

Here's what I found across the codebase. I read core files top-to-bottom and sampled the big ones (`expr.go`, `link.go`); the testdata and `known_failures_*` files weren't reviewed. Findings ordered roughly by impact.

## Real bugs

**1. Unbalanced parens in atomic bitfield read — `expr.go:3165`**
```go
b.w("((%s(%s((%s)&%#0x)>>%d)", ...) // 5 opens, 3 closes — net +2 opens
break
```
The non-atomic branch directly below balances with a trailing `b.w(")))")`, but the atomic-`break` path has no closer. Generated Go for `atomic` + bitfield reads will be a syntax error. Unlikely to be exercised in the existing corpus (atomic bitfields are rare in C), but it is wrong.

**2. `debugLinkerSave` is a no-op — `link.go:1155-1163`**
```go
if l.task.debugLinkerSave {
    os.WriteFile(ofn, out.Bytes(), 0666)   // "pre type checking" save
}
if err := os.WriteFile(ofn, out.Bytes(), 0666); ... // same file, same bytes
```
Both writes go to `ofn` with identical `out.Bytes()`, so the debug flag has no observable effect. To actually save a pre-gofmt/pre-postProcess artifact, the first write needs a distinct path (e.g. `ofn + ".debug"`).

**3. Wrong arg count in two atomic-builtin error messages**
- `expr.go:2887` — `c11AtomicLoad` checks `len(args) != 2` but says *"expected 1"*.
- `expr.go:2964` — `c11AtomicExchange` checks `len(args) != 3` but says *"expected 4"*.

Cosmetic but actively misleading when triaging a failure.

**4. Bad format string — `expr.go:4436`**
```go
c.err(errorf("TODO internal error", n))
```
No verb for `n`, so the produced error is `"TODO internal error%!(EXTRA *cc.ExpressionList=...)"`. Either drop the arg or add `%v: %T` / `%v`.

**5. Dead code after panic — `expr.go:4604-4605`**
```go
panic(todo(""))
c.err(errorf("TODO %v", mode))
```
The `c.err` line is unreachable.

## Quality / latent issues

**6. `paths` for `-include` search uses filenames as directories — `ccgo.go:779-794`**
```go
paths := append([]string{"."}, t.include...)
for _, v := range t.include {
    for _, w := range paths { ... filepath.Join(w, v) ... }
}
```
`t.include` is the list of `-include` *files*, not include *paths*. Beyond the first iteration with `w == "."`, every other `w` is a previously-listed `-include` filename, so `filepath.Join("prev.h", "next.h")` will always Stat-fail. Almost certainly a typo — likely intended `t.iquote` / `t.I` / `t.isystem`. Works in practice only because the cwd lookup at `w == "."` covers the common case.

**7. `cc` builder appends `.go` blindly — `exec.go:469`**
```go
set.Arg("o", true, func(arg, val string) error { args.add(arg, val+".go"); return nil })
```
`mv`/`rm`/`libtool` route the value through `t.goFile()`, which preserves `.go` filenames and converts `.o`/`.lo`/`.a` correctly. `cc -o`'s shim just concatenates `.go`, so `-o foo.go` becomes `-o foo.go.go`. Probably benign because autoconf-driven invocations always pass `-o foo.o`, but inconsistent with sibling shims.

**8. `compile.go:413-417` — inner `err` shadows outer named return**
```go
defer func() {
    if err := w.Flush(); err != nil {  // shadows outer err
        c.err(errorf("%v", err))
    }
}()
```
A Flush failure is reported via `c.eh` (so it surfaces through `parallel.errors`), but the named return `err` stays `nil`. If the caller relied solely on the return value it'd miss this. Currently safe because `p.exec` collects errors through both channels.

**9. `mentionsFunc` is still recursive — `init.go:124-175`**
Commit 4440e99 converted `firstToken` to a heap-allocated stack to avoid a 32-bit stack overflow. `mentionsFunc` (and `mentionsDecl` next to it) still walk AST nodes via direct recursion through reflect, with the same exposure on deep ASTs / 32-bit targets.

**10. Recursion-cycle protection for `inlineInfo` missing**
Acknowledged in the file-top TODO of `ccgo.go:12` — `c.f.inlineInfo` is a singly-linked chain (`parent`) but nothing checks for a function inlining itself transitively, so mutually-`inline` cycles would blow the stack at translate time. Documented limitation, not a regression.

**11. Typos / grammar**
- `link.go:551, 557` — `"defintions"` → `"definitions"`.
- `expr.go:3106` — `"pointer kind do not match"` → `"kinds"`.

**12. `reflect.StringHeader` use — `link.go:1706`**
Generated code uses `reflect.StringHeader`, deprecated since Go 1.20 (Go points users to `unsafe.StringData`). Still works today but worth queuing.

## Notes that aren't bugs but stood out

- `newLinker` copies `tags` (a value-typed array) and exposes it as `goTags[:]`. Correct in current Go, but worth a comment — the safety relies on `tags` being an array literal, not a slice.
- `getFileSymbols` (`link.go:514`) reads only `x.ConstSpecs[0]` for object metadata; the gen path emits a single spec, so OK, but the unguarded `[0]` is fragile.
- `extractPos` (`etc.go:487`) treats any `s[1] == ':'` as a Windows drive prefix. Fine on Linux paths; would misparse paths like `a:foo`.
