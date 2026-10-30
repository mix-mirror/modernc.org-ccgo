// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIncludeOption checks the lookup of -include files: an absolute path is
// used as is, a relative path is searched in the working directory and then in
// the #include "..." search chain, and the files are included in command line
// order. An absolute path used to be joined to the working directory and was
// never found.
func TestIncludeOption(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "inc")
	if err := os.Mkdir(inc, 0o755); err != nil {
		t.Fatal(err)
	}

	// second.h uses the macro defined by first.h, so it must come after it.
	for _, v := range []struct{ fn, src string }{
		{filepath.Join(dir, "abs.h"), "#define ABS 1\n"},
		{filepath.Join(dir, "rel.h"), "#define REL 2\n"},
		{filepath.Join(inc, "first.h"), "#define FIRST 3\n"},
		{filepath.Join(inc, "second.h"), "#define SECOND (FIRST + 1)\n"},
		{filepath.Join(dir, "main.c"), "int x[ABS + REL + FIRST + SECOND == 10 ? 1 : -1];\n"},
	} {
		if err := os.WriteFile(v.fn, []byte(v.src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	rel, err := filepath.Rel(wd, filepath.Join(dir, "rel.h"))
	if err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		args = append(append([]string{"ccgo", "-c", "-o", filepath.Join(dir, "main.o.go"), "-I", inc}, args...), filepath.Join(dir, "main.c"))
		err := NewTask(goos, goarch, args, &out, &out, nil).Main()
		return out.String(), err
	}

	if out, err := run("-include", filepath.Join(dir, "abs.h"), "-include", rel, "-include", "first.h", "-include", "second.h"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	out, err := run("-include", filepath.Join(dir, "abs.h"), "-include", rel, "-include", "missing.h", "-include", "first.h", "-include", "second.h")
	if err == nil || !strings.Contains(err.Error()+out, "-include missing.h: not found") {
		t.Fatalf("expected a not found error, got %v\n%s", err, out)
	}
}
