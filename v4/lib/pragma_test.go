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
	"testing/fstest"
)

const (
	pragmaOnceHeader = `#pragma once

#ifdef ONCE_H
#error once.h processed twice
#endif
#define ONCE_H

static const int once_marker = 1;

static int once_f(void) { return 42; }
`
	pragmaOnceMain = `#include "once.h"
#include "once.h"
#include "sub/../once.h"
#include <once.h>

int main(void) { return once_f() - 42; }
`
)

// TestPragmaOnce checks that a header guarded only by '#pragma once' is
// processed once per translation unit, even when it is reached through
// differently spelled include paths.
func TestPragmaOnce(t *testing.T) {
	dir := t.TempDir()
	hdr := filepath.Join(dir, "once.h")
	main := filepath.Join(dir, "main.c")
	for _, v := range []struct{ fn, src string }{{hdr, pragmaOnceHeader}, {main, pragmaOnceMain}} {
		if err := os.WriteFile(v.fn, []byte(v.src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// '#include <once.h>' is resolved through this -I path, '#include "once.h"'
	// through the directory of main.c, named absolutely above. Both must be
	// recognized as the same file.
	inc := dir
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, dir); err == nil {
			inc = rel
		}
	}

	t.Run("preprocess", func(t *testing.T) {
		var out, errs bytes.Buffer
		if err := NewTask(goos, goarch, []string{"ccgo", "-E", "-I", inc, main}, &out, &errs, nil).Main(); err != nil {
			t.Fatalf("%v\n%s", err, errs.Bytes())
		}

		if g, e := strings.Count(out.String(), "once_marker"), 1; g != e {
			t.Fatalf("got %d definitions of once_marker, expected %d:\n%s", g, e, out.Bytes())
		}
	})

	t.Run("compile", func(t *testing.T) {
		var out bytes.Buffer
		ofn := filepath.Join(dir, "main.go")
		if err := NewTask(goos, goarch, []string{"ccgo", "-o", ofn, "-I", inc, main}, &out, &out, nil).Main(); err != nil {
			t.Fatalf("%v\n%s", err, out.Bytes())
		}
	})

	t.Run("fs", func(t *testing.T) {
		fsys := fstest.MapFS{
			"once.h": &fstest.MapFile{Data: []byte(pragmaOnceHeader)},
			"main.c": &fstest.MapFile{Data: []byte(pragmaOnceMain)},
		}
		var out, errs bytes.Buffer
		if err := NewTask(goos, goarch, []string{"ccgo", "-E", "-I", ".", "main.c"}, &out, &errs, fsys).Main(); err != nil {
			t.Fatalf("%v\n%s", err, errs.Bytes())
		}

		if g, e := strings.Count(out.String(), "once_marker"), 1; g != e {
			t.Fatalf("got %d definitions of once_marker, expected %d:\n%s", g, e, out.Bytes())
		}
	})
}
