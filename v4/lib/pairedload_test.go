// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHoistedPairedLoads checks that a load through a C pointer in a
// conditional block is not merged with a load in the block before it into one
// arm64 LDP. Since Go 1.27 the arm64 backend pairs adjacent loads from a
// single-predecessor successor block when they share a base. ccgo 6cf0225
// (v4.38.0) wrote C pointer dereferences as *(*T)(unsafe.Add(nil, p)), which
// the compiler folds into p, so p[0] and p[1] shared one base. The paired load
// read p[1] unconditionally and faulted when the C object ended at the end of
// a mapping: libsqlite3's mptest died with SIGSEGV in
// sqlite3BtreeIntegrityCheck on linux/arm64.
//
// The code is transpiled for the host, but only uses types of the same size
// on every target, and compiled for linux/arm64 with -gcflags=-S.
func TestNoHoistedPairedLoads(t *testing.T) {
	const src = `
unsigned f(unsigned *a) {
	if (a[0] == 0) {
		if (a[1] != 1) {
			return 2;
		}

		return 1;
	}

	return 0;
}

unsigned long long g(unsigned long long *p) {
	if (*p == 0) {
		if (*(p + 1) != 1) {
			return 2;
		}

		return 1;
	}

	return 0;
}
`
	const support = `package main

type TLS struct{}

func UintptrFromInt32(n int32) uintptr { return uintptr(n) }

func main() {
	_ = Xf
	_ = Xg
}
`
	cFile := filepath.Join(t.TempDir(), "lib.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "support.go"), []byte(support), 0644); err != nil {
		t.Fatal(err)
	}

	goFile := filepath.Join(dir, "lib.go")
	var out bytes.Buffer
	task := NewTask(
		goos,
		goarch,
		[]string{
			"ccgo",
			"-o", goFile,
			"--package-name=main",
			"--prefix-external=X",
			"-ffreestanding",
			"-nostdlib",
			cFile,
		},
		&out, &out, nil,
	)
	if err := task.Main(); err != nil {
		t.Fatalf("ccgo: %v\n%s", err, out.Bytes())
	}

	b, err := os.ReadFile(goFile)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(b), "unsafe.Add(nil") {
		t.Errorf("generated code uses unsafe.Add(nil, ...)\n%s", b)
	}

	// Drop the host //go:build constraint, the file is built for linux/arm64.
	b = regexp.MustCompile(`(?m)^//go:build .*$`).ReplaceAll(b, nil)
	if err := os.WriteFile(goFile, b, 0644); err != nil {
		t.Fatal(err)
	}

	var asm []byte
	if err := inDir(dir, func() error {
		if out, err := exec.Command("go", "mod", "init", "test").CombinedOutput(); err != nil {
			return fmt.Errorf("go mod init: %v\n%s", err, out)
		}

		cmd := exec.Command("go", "build", "-gcflags=-S", "-o", filepath.Join(dir, "x.bin"), ".")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}

		asm = out
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Function text starts with a "main.Xf STEXT" line and ends at the next
	// symbol line, which starts in column 1 as well.
	for _, fn := range []string{"Xf", "Xg"} {
		m := regexp.MustCompile(`(?ms)^main\.` + fn + ` STEXT.*?\n(.*?)^\S`).FindSubmatch(asm)
		if m == nil {
			t.Errorf("%s: function not found in the -S output\n%s", fn, asm)
			continue
		}

		if bytes.Contains(m[1], []byte("LDP")) {
			t.Errorf("%s: loads paired into LDP\n%s", fn, m[1])
		}
	}
}
