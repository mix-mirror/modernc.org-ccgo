// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIssue47Regression verifies that ccgo correctly handles nested unions
// containing a pointer member, when the union is an array element and has a
// larger alternative member.
//
// Before fix d32226db, this scenario produced invalid Go code: "_ [-16]byte".
// See https://gitlab.com/cznic/ccgo/-/issues/47
func TestIssue47Regression(t *testing.T) {
	// The C source is cznic's minimal reproducer from the MR #26 review.
	cSrc := []byte(`
int g = 42;

typedef union {
	struct { unsigned char a; void *p; } act;  // 16 bytes on 64-bit
	struct { unsigned long x, y, z; } big;      // 24 bytes on 64-bit
} Inner;

typedef union {
	Inner action;                                // 24 bytes
	struct { unsigned char count; _Bool reusable; } entry;
} Entry;

Entry table[2] = {
	{ .action = { .act = { .a = 1, .p = &g } } },
	{ .action = { .act = { .a = 3, .p = &g } } },
};

#include <stdio.h>

int main() {
	printf("a0=%d a1=%d\n", table[0].action.act.a, table[1].action.act.a);
	if (table[0].action.act.a != 1) return 1;
	if (table[0].action.act.p != &g) return 2;
	if (table[1].action.act.a != 3) return 3;
	if (table[1].action.act.p != &g) return 4;
	printf("PASS\n");
	return 0;
}
`)

	dir, err := os.MkdirTemp("", "ccgo-issue47-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Write C source.
	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, cSrc, 0644); err != nil {
		t.Fatal(err)
	}

	// ---- Step 1: Compile & run with hostCC ----
	bin := filepath.Join(dir, "cbin")
	if out, err := exec.Command(hostCC, "-o", bin, "-w", cFile, "-lm", "-lpthread").CombinedOutput(); err != nil {
		t.Fatalf("hostCC failed: %v\n%s", err, out)
	}
	cOut, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("C binary failed: %v", err)
	}

	// ---- Step 2: Transpile with ccgo ----
	goFile := filepath.Join(dir, "test.go")
	var stdout, stderr bytes.Buffer
	task := NewTask(
		goos,
		goarch,
		[]string{
			"ccgo",
			"-o", goFile,
			"-verify-types",
			"--prefix-field=F",
			"-ignore-unsupported-alignment",
			"-ignore-vector-functions",
			"-keep-object-files",
			"-positions",
			"-full-paths",
			cFile,
		},
		&stdout, &stderr, nil,
	)
	if err := task.Main(); err != nil {
		t.Fatalf("ccgo failed:\nstdout: %s\nstderr: %s\nerr: %v", stdout.Bytes(), stderr.Bytes(), err)
	}
	if !t.Failed() {
		t.Logf("ccgo OK")
	}

	// ---- Step 3: Build Go binary ----
	if err := inDir(dir, func() error {
		if out, err := exec.Command("go", "mod", "init", "test").CombinedOutput(); err != nil {
			return fmt.Errorf("go mod init: %v\n%s", err, out)
		}
		if out, err := exec.Command("go", "get", *oLibc+libcVersion).CombinedOutput(); err != nil {
			return fmt.Errorf("go get: %v\n%s", err, out)
		}
		goBin := filepath.Join(dir, "gobin")
		if out, err := exec.Command("go", "build", "-o", goBin, goFile).CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// ---- Step 4: Run Go binary ----
	goBin := filepath.Join(dir, "gobin")
	goOut, err := exec.Command(goBin).Output()
	if err != nil {
		t.Fatalf("Go binary failed: %v", err)
	}

	// ---- Step 5: Compare outputs ----
	cOut = bytes.TrimSpace(cOut)
	goOut = bytes.TrimSpace(goOut)
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:  %q\nGo: %q", cOut, goOut)
	}
	t.Logf("output: %s", cOut)
}
