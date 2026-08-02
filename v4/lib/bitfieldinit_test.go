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

// TestBitfieldInitRegression verifies that initializing a bit field puts the
// value at the field's bit offset within its access unit.
//
// Two paths got this wrong. A union whose only member is a bit field was
// rendered as a plain Go struct literal storing the value as-is, and a struct
// containing a union member has its initializer emitted as a sequence of stores
// (initCode), which stored the declared type over the whole access unit. Both
// happen to work on little-endian targets, where a bit field group starts at bit
// 0, but corrupt the value on big-endian ones (linux/s390x), where a group is
// filled from the most significant bit of the access unit down. The initCode
// path additionally clobbered the other bit fields sharing the access unit,
// which is wrong everywhere.
//
// The union case surfaced as a TestCSmith failure for
// '--bitfields ... -s 2273393378' on linux/s390x after modernc.org/cc/v4 v4.29.1
// started allocating bit fields MSB-first on big-endian targets.
func TestBitfieldInitRegression(t *testing.T) {
	cSrc := []byte(`
#include <stdio.h>

union U30   { unsigned f0 : 30; };           /* access unit == declared type */
union U3in8 { unsigned long long f0 : 3; };  /* access unit narrower than the declared type */
union U12s  { signed f0 : 12; };             /* signed, access unit narrower */
union U9    { unsigned short f0 : 9; };
union UMix  { unsigned f0 : 30; int f1; };   /* bit field is not the only member */

/* A struct with a union member takes the initCode() path. */
struct WithUnion {
	int a;
	union { int u0; char u1[4]; } u;
	unsigned b : 5;
	signed c : 11;
	union U30 d;
};

static union U30   g30      = {6};
static union U3in8 g3in8    = {5};
static union U12s  g12s     = {-7};
static union U9    g9       = {300};
static union UMix  gmix     = {6};
static union U30   garr[3]  = {{1}, {2}, {3}};
static struct WithUnion gwu = {1, {2}, 3, -4, {5}};

int main() {
	union U30   l30     = {6};
	union U3in8 l3in8   = {5};
	union U12s  l12s    = {-7};
	union U9    l9      = {300};
	union UMix  lmix    = {6};
	union U30   larr[3] = {{1}, {2}, {3}};
	struct WithUnion lwu = {1, {2}, 3, -4, {5}};

	printf("g %u %llu %d %u %u %u %u %u\n", g30.f0, (unsigned long long)g3in8.f0,
		g12s.f0, g9.f0, gmix.f0, garr[0].f0, garr[1].f0, garr[2].f0);
	printf("l %u %llu %d %u %u %u %u %u\n", l30.f0, (unsigned long long)l3in8.f0,
		l12s.f0, l9.f0, lmix.f0, larr[0].f0, larr[1].f0, larr[2].f0);
	printf("gwu %d %d %u %d %u\n", gwu.a, gwu.u.u0, gwu.b, gwu.c, gwu.d.f0);
	printf("lwu %d %d %u %d %u\n", lwu.a, lwu.u.u0, lwu.b, lwu.c, lwu.d.f0);
	l30.f0 = 0x2ABCDEF;
	gwu.b = 30;
	printf("store %u %u %d\n", l30.f0, gwu.b, gwu.c);
	return 0;
}
`)

	dir, err := os.MkdirTemp("", "ccgo-bitfield-init-")
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
