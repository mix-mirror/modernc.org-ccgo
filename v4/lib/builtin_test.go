// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestHasBuiltin checks the __has_builtin answers: 1 for builtins ccgo
// translates itself, for those the builtin preamble declares and, when linking
// against libc, for those libc exports; 0 for anything else, such as clang's
// __builtin_elementwise_sqrt, which cc used to report as present.
func TestHasBuiltin(t *testing.T) {
	const src = `
#if __has_builtin(__builtin_add_overflow)
intrinsic_yes
#endif
#if __has_builtin(__builtin_sqrt)
preamble_yes
#endif
#if __has_builtin(__builtin_round)
libc_yes
#endif
#if !__has_builtin(__builtin_elementwise_sqrt)
elementwise_no
#endif
#if !__has_builtin(__builtin_ccgo_no_such_builtin)
unknown_no
#endif
`
	cFile := filepath.Join(t.TempDir(), "test.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		flags []string
		want  string
	}{
		{nil, "intrinsic_yes preamble_yes libc_yes elementwise_no unknown_no"},
		// No libc to link against: its builtins are not available.
		{[]string{"-nostdlib"}, "intrinsic_yes preamble_yes elementwise_no unknown_no"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"ccgo", "-E"}, test.flags...)
		if err := NewTask(goos, goarch, append(args, cFile), &stdout, &stderr, nil).Main(); err != nil {
			t.Fatalf("%v: %v\n%s", test.flags, err, stderr.Bytes())
		}

		var got []string
		for _, v := range strings.Fields(stdout.String()) {
			if strings.HasSuffix(v, "_yes") || strings.HasSuffix(v, "_no") {
				got = append(got, v)
			}
		}
		if g := strings.Join(got, " "); g != test.want {
			t.Errorf("%v: got %q, want %q", test.flags, g, test.want)
		}
	}
}

// TestIntrinsicBuiltins checks intrinsicBuiltins against the code: every
// builtin name the translator matches by its literal must be listed there or
// declared by the builtin preamble, and every listed name must still be matched
// somewhere. A builtin added to the translator but not to the list would make
// __has_builtin deny a builtin ccgo supports.
func TestIntrinsicBuiltins(t *testing.T) {
	// Literal builtin names that are not callable builtins.
	notCalls := map[string]bool{
		"__builtin_va_arg_impl": true, // the expansion of the preamble's va_arg
		"__builtin_va_list":     true, // a type
	}
	re := regexp.MustCompile(`"` + reBuiltinName.String() + `"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") || fn == "builtin.go" {
			continue
		}

		b, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}

	var missing, stale []string
	for nm := range seen {
		_, intrinsic := intrinsicBuiltins[nm]
		_, declared := preambleBuiltins()[nm]
		if !intrinsic && !declared && !notCalls[nm] {
			missing = append(missing, nm)
		}
	}
	for nm := range intrinsicBuiltins {
		if !seen[nm] {
			stale = append(stale, nm)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) != 0 {
		t.Errorf("matched by the translator but neither in intrinsicBuiltins nor declared in the preamble: %q", missing)
	}
	if len(stale) != 0 {
		t.Errorf("in intrinsicBuiltins but no longer matched by the translator: %q", stale)
	}
}

// TestFloat16Storage checks that __bf16 and _Float16 translate as opaque 2-byte
// storage on targets whose ABI has them. mingw-w64 GCC 14's Windows headers
// declare __bf16 typedefs (avx512bf16intrin.h), which made every translation
// including them fail with "TODO *cc.PredefinedType __bfloat16 __bf16".
func TestFloat16Storage(t *testing.T) {
	const src = `
typedef __bf16 bf;
typedef _Float16 h;
typedef __bf16 v8bf __attribute__((__vector_size__(16))); // declared, unused, as in the headers
struct s { char c; bf b; h f; };
struct s gs;
int sz = sizeof(struct s);
`
	dir := t.TempDir()
	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	goFile := filepath.Join(dir, "test.go")
	var stdout, stderr bytes.Buffer
	task := NewTask("windows", "amd64", []string{"ccgo", "-o", goFile, "-ffreestanding", "-nostdlib", "--package-name=main", cFile}, &stdout, &stderr, nil)
	if err := task.Main(); err != nil {
		t.Fatalf("%v\n%s", err, stderr.Bytes())
	}

	b, err := os.ReadFile(goFile)
	if err != nil {
		t.Fatal(err)
	}

	got := string(b)
	for _, want := range []string{
		"[1]uint16", // bf, h
		"(6)",       // sizeof(struct s): char, padding, two 2-byte fields
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
}
