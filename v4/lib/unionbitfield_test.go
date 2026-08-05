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

// TestUnionBitfieldLayoutRegression verifies the size and the alignment of the
// Go struct rendering a union having a bit field member.
//
// The Go struct has one field for the first positive-sized member, sized to
// cover the whole union. For a bit field that field is only the storage of the
// field's access unit and it was rendered using the declared type instead. That
// type can be wider than the access unit and even wider than the union itself,
// eg. sizeof(union { unsigned long long f0:3; }) is 4 on linux/386, where the
// declared type produced a Go field wider than the union and the invalid
// padding '_ [-4]byte' that made the linux/386 builder fail.
//
// Additionally, the alignment of the Go struct was computed from all members,
// including the bit fields that are not rendered at all. A union like
// 'union { char c; unsigned long long b:40; }' then got no alignment pseudo
// field, so the C union was 8-aligned while its Go rendering was 1-aligned,
// which moved the union within an enclosing struct.
func TestUnionBitfieldLayoutRegression(t *testing.T) {
	cSrc := []byte(`
#include <stdio.h>
#include <stddef.h>

union U1 { unsigned long long f0 : 3; };          /* access unit narrower than the declared type */
union U2 { signed f0 : 12; };                     /* the same, signed */
union U3 { char c; unsigned long long b : 40; };  /* the widest member is a bit field */
union U4 { unsigned a : 3; unsigned long long b : 40; };
union U5 { unsigned b : 9; char c[3]; };          /* the group size is not a power of two */
union U6 { unsigned : 0; int i; };                /* the first member is a zero width bit field */

/* Every union in a struct, at a non zero offset and in an array, to have its
   size and alignment show up in an offset. */
#define WRAP(n) \
	struct W##n { char c; union U##n u; }; \
	struct V##n { union U##n u; char c; }; \
	struct A##n { union U##n a[3]; char c; };
WRAP(1) WRAP(2) WRAP(3) WRAP(4) WRAP(5) WRAP(6)

#define SHOW(n) printf("U%d %d %d | %d %d | %d %d | %d %d\n", n, \
	(int)sizeof(union U##n), (int)_Alignof(union U##n), \
	(int)sizeof(struct W##n), (int)offsetof(struct W##n, u), \
	(int)sizeof(struct V##n), (int)offsetof(struct V##n, c), \
	(int)sizeof(struct A##n), (int)offsetof(struct A##n, c));

static union U1  g1 = {5};
static union U4  g4 = {5};
static union U5  g5 = {300};
static struct W1 w1 = {'a', {5}};

int main() {
	union U1 l1 = {5};
	struct W4 l4 = {'b', {5}};

	SHOW(1) SHOW(2) SHOW(3) SHOW(4) SHOW(5) SHOW(6)
	printf("g %llu %u %u %c %llu %c %u\n", (unsigned long long)g1.f0, g4.a, g5.b,
		w1.c, (unsigned long long)w1.u.f0, l4.c, l4.u.a);
	l1.f0 = 7;
	g4.b = 0x123456789ULL;
	printf("store %llu %u %llu\n", (unsigned long long)l1.f0, g4.a,
		(unsigned long long)g4.b);
	return 0;
}
`)

	dir, err := os.MkdirTemp("", "ccgo-union-bitfield-")
	if err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(dir)

	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, cSrc, 0644); err != nil {
		t.Fatal(err)
	}

	// ---- Step 1: Compile & run with hostCC ----
	bin := filepath.Join(dir, enforceBinaryExt("cbin"))
	if out, err := exec.Command(hostCC, "-o", bin, "-w", cFile, "-lm", "-lpthread").CombinedOutput(); err != nil {
		t.Skipf("hostCC cannot build the reference: %v\n%s", err, out)
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
			cFile,
		},
		&stdout, &stderr, nil,
	)
	if err := task.Main(); err != nil {
		t.Fatalf("ccgo failed:\nstdout: %s\nstderr: %s\nerr: %v", stdout.Bytes(), stderr.Bytes(), err)
	}

	// ---- Step 3: Build Go binary ----
	goBin := filepath.Join(dir, enforceBinaryExt("gobin"))
	if err := inDir(dir, func() error {
		if out, err := exec.Command("go", "mod", "init", "test").CombinedOutput(); err != nil {
			return fmt.Errorf("go mod init: %v\n%s", err, out)
		}

		if out, err := exec.Command("go", "get", *oLibc+libcVersion).CombinedOutput(); err != nil {
			return fmt.Errorf("go get: %v\n%s", err, out)
		}

		if out, err := exec.Command("go", "build", "-o", goBin, goFile).CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// ---- Step 4: Run Go binary ----
	goOut, err := exec.Command(goBin).Output()
	if err != nil {
		t.Fatalf("Go binary failed: %v", err)
	}

	// ---- Step 5: Compare outputs ----
	cOut = bytes.TrimSpace(cOut)
	goOut = bytes.TrimSpace(goOut)
	// Normalize CRLF -> LF, see TestIssue47Regression.
	if bytes.Contains(cOut, []byte("\r\n")) {
		cOut = bytes.ReplaceAll(cOut, []byte("\r"), nil)
	}
	if bytes.Contains(goOut, []byte("\r\n")) {
		goOut = bytes.ReplaceAll(goOut, []byte("\r"), nil)
	}
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:  %s\nGo: %s", cOut, goOut)
	}
	t.Logf("output: %s", cOut)
}
