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
	"testing"

	"modernc.org/cc/v4"
)

// testHostCCvsCcgo compiles and runs the C program src with the host C
// compiler and with ccgo, fails t when the outputs differ and returns the
// output.
func testHostCCvsCcgo(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, enforceBinaryExt("cbin"))
	if out, err := exec.Command(hostCC, "-o", bin, "-w", cFile, "-lm", "-lpthread").CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", hostCC, err, out)
	}

	cOut, err := exec.Command(bin).CombinedOutput()
	if err != nil {
		t.Fatalf("C binary: %v\n%s", err, cOut)
	}

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
			"-positions",
			"-full-paths",
			cFile,
		},
		&stdout, &stderr, nil,
	)
	if err := task.Main(); err != nil {
		t.Fatalf("ccgo: %v\nstdout: %s\nstderr: %s", err, stdout.Bytes(), stderr.Bytes())
	}

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

	goOut, err := exec.Command(goBin).CombinedOutput()
	if err != nil {
		t.Fatalf("Go binary: %v\n%s", err, goOut)
	}

	// On Windows the host C binary writes text-mode stdout (\r\n) while ccgo's
	// libc emits \n. TestExec normalizes the same way.
	cOut = bytes.ReplaceAll(bytes.TrimSpace(cOut), []byte("\r\n"), []byte("\n"))
	goOut = bytes.ReplaceAll(bytes.TrimSpace(goOut), []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:  %q\nGo: %q", cOut, goOut)
	}
	return string(cOut)
}

// TestIssue63 verifies that a character constant with the high bit set is
// negative where char is signed, as cc evaluates it, instead of the value of
// the Go rune literal made from its source text.
//
// See https://gitlab.com/cznic/ccgo/-/issues/63
func TestIssue63(t *testing.T) {
	const src = `
#include <stdio.h>
enum opcode { PROTO = '\x80', FRAME = '\x95' };
int main(void) {
	char buf[2] = { (char)0x80, (char)0x95 };
	char fill = (char)0xff;
	char c = FRAME;
	unsigned char u = '\xff';
	printf("%d %d %d %d %d\n", PROTO, FRAME, buf[0] == PROTO, (int)'\xff', fill == '\xff');
	printf("%d %d %d %d %d\n", c, u, '\xff' < 0, (int)(unsigned char)'\x80', 'a' + '\x80');
	switch ((enum opcode)buf[1]) {
	case FRAME: puts("FRAME"); break;
	default: puts("default"); break;
	}
	switch (fill) {
	case '\xff': puts("fill"); break;
	default: puts("default"); break;
	}
	return 0;
}
`
	abi, err := cc.NewABI(goos, goarch)
	if err != nil {
		t.Fatal(err)
	}

	exp := "128 149 1 255 1\n149 255 0 128 225\nFRAME\nfill"
	if abi.SignedChar {
		exp = "-128 -107 1 -1 1\n-107 255 1 128 -31\nFRAME\nfill"
	}
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue64 verifies that a later initializer of a subobject overrides the
// earlier ones ([0]6.7.9/19): of a scalar, of the parts of an aggregate a
// brace enclosed initializer initializes, of the other members of a union.
//
// See https://gitlab.com/cznic/ccgo/-/issues/64
func TestIssue64(t *testing.T) {
	const src = `
#include <stdio.h>
struct O { int a, b; } o = { .a = 1, .b = 2, .a = 3 };
int r[4] = { [1] = 1, [1] = 2, 3 };
struct S17 { int x; struct { int m, n; }; int w; } s17 = { .n = 1, 2, .m = 3, 4 };
struct A { int x, y; };
struct B { struct A a; int z; } b1 = { .a.x = 1, .a = { .y = 2 } }, b2 = { .a = { 1, 2 }, .a.x = 3 }, b3 = { .a = { 1, 2 }, .a = {} };
union U { int i; float f; unsigned char c[4]; } u1 = { .i = 1, .f = 2.0f }, u2 = { .c = { 1, 2, 3, 4 }, .i = 0x100 }, u3 = { .i = 0x01020304, .c[1] = 9 };
struct BF { unsigned a:4, b:4; int c; } bf = { .a = 1, .b = 2, .a = 3 };
struct CS { char s[4]; int n; } cs = { .s = "ab", .s = "cd", .n = 1 };
int m[2][2] = { { 1, 2 }, [0] = { 3, 4 } };
int z[2] = { [0] = 1, [0] = 0 };
int main(void) {
	struct O lo = { .a = 1, .b = 2, .a = 3 };
	int lr[4] = { [1] = 1, [1] = 2, 3 };
	struct B lb = { .a.x = 1, .a = { .y = 2 } };
	printf("%d %d | %d %d %d %d | %d %d %d %d\n", o.a, o.b, r[0], r[1], r[2], r[3], s17.x, s17.m, s17.n, s17.w);
	printf("%d %d %d | %d %d %d | %d %d %d\n", b1.a.x, b1.a.y, b1.z, b2.a.x, b2.a.y, b2.z, b3.a.x, b3.a.y, b3.z);
	printf("%g %d %d %d\n", u1.f, u2.i, u3.c[1], u3.c[0] + u3.c[2] + u3.c[3]);
	printf("%d %d %d | %s %d | %d %d %d %d | %d %d\n", bf.a, bf.b, bf.c, cs.s, cs.n, m[0][0], m[0][1], m[1][0], m[1][1], z[0], z[1]);
	printf("%d %d | %d %d %d %d | %d %d %d\n", lo.a, lo.b, lr[0], lr[1], lr[2], lr[3], lb.a.x, lb.a.y, lb.z);
	return 0;
}
`
	// gcc 13.
	const exp = `3 2 | 0 2 3 0 | 0 3 4 2
0 2 0 | 3 2 0 | 0 0 0
2 256 9 0
3 2 0 | cd 1 | 3 4 0 0 | 0 0
3 2 | 0 2 3 0 | 0 2 0`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue65 verifies that a [lo ... hi] range designator replicates the
// initializer over all the elements, merged with the other initializers of
// the elements, also when the initializer is a brace enclosed list.
//
// See https://gitlab.com/cznic/ccgo/-/issues/65
func TestIssue65(t *testing.T) {
	const src = `
#include <stdio.h>
struct XY { int x, y; };
struct XY s[2] = { [0 ... 1].x = 1, 2 };
struct XY s2[2] = { [0 ... 1].x = 1 };
struct XY s3[3] = { [0 ... 1] = { 1, 2 } };
int a[4] = { [0 ... 1] = 7 };
int r1[4] = { [0 ... 3] = 7, [1] = 8 };
int r2[4] = { [1] = 8, [0 ... 3] = 7 };
struct XY r3[3] = { [0 ... 2].x = 1, [1].y = 5 };
int r4[2][3] = { { [0 ... 2] = 1 }, { [1 ... 2] = 2 } };
struct XY r5[4] = { [0 ... 1] = { 1, 2 }, 3, 4 };
struct XY r7[2] = { [1].y = 9, [0 ... 1] = { 1, 2 } };
struct XY r8[2][2] = { [0 ... 1] = { [0 ... 1] = { .y = 1, .x = 2, .y = 3 } } };
typedef struct XY arr_t[2];
arr_t ta = { [0 ... 1].x = 3 };
union U { int i; unsigned char c[4]; } ua[3] = { [0 ... 2].i = 1, [1].c[1] = 9 };
int main(void) {
	struct XY l3[2] = { [0 ... 1].x = 1, 2 };
	struct XY l4[3] = { [0 ... 1] = { 1, 2 } };
	int *p = (int[4]){ [0 ... 3] = 7, [1] = 8 };
	printf("%d %d %d %d | %d %d %d %d | %d %d %d %d %d %d | %d %d %d %d\n",
		s[0].x, s[0].y, s[1].x, s[1].y, s2[0].x, s2[0].y, s2[1].x, s2[1].y,
		s3[0].x, s3[0].y, s3[1].x, s3[1].y, s3[2].x, s3[2].y, a[0], a[1], a[2], a[3]);
	printf("%d %d %d %d | %d %d %d %d | %d %d %d %d %d %d | %d %d %d %d %d %d\n",
		r1[0], r1[1], r1[2], r1[3], r2[0], r2[1], r2[2], r2[3],
		r3[0].x, r3[0].y, r3[1].x, r3[1].y, r3[2].x, r3[2].y,
		r4[0][0], r4[0][1], r4[0][2], r4[1][0], r4[1][1], r4[1][2]);
	printf("%d %d %d %d %d %d %d %d | %d %d %d %d | %d %d %d %d %d %d %d %d\n",
		r5[0].x, r5[0].y, r5[1].x, r5[1].y, r5[2].x, r5[2].y, r5[3].x, r5[3].y,
		r7[0].x, r7[0].y, r7[1].x, r7[1].y,
		r8[0][0].x, r8[0][0].y, r8[0][1].x, r8[0][1].y, r8[1][0].x, r8[1][0].y, r8[1][1].x, r8[1][1].y);
	printf("%d %d %d %d | %d %d %d %d\n", ta[0].x, ta[0].y, ta[1].x, ta[1].y,
		ua[0].i, ua[1].c[1], ua[1].c[0] + ua[1].c[2] + ua[1].c[3], ua[2].i);
	printf("%d %d %d %d | %d %d %d %d %d %d | %d %d %d %d\n",
		l3[0].x, l3[0].y, l3[1].x, l3[1].y, l4[0].x, l4[0].y, l4[1].x, l4[1].y, l4[2].x, l4[2].y, p[0], p[1], p[2], p[3]);
	return 0;
}
`
	// gcc 13.
	const exp = `1 0 1 2 | 1 0 1 0 | 1 2 1 2 0 0 | 7 7 0 0
7 8 7 7 | 7 7 7 7 | 1 0 1 5 1 0 | 1 1 1 0 2 2
1 2 1 2 3 4 0 0 | 1 2 1 2 | 2 3 2 3 2 3 2 3
3 0 3 0 | 1 9 0 1
1 0 1 2 | 1 2 1 2 0 0 | 7 8 7 7`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue66 verifies that a string literal initializing a char array is
// split into its characters when later initializers designate elements of
// the array, which override the characters.
//
// See https://gitlab.com/cznic/ccgo/-/issues/66
func TestIssue66(t *testing.T) {
	const src = `
#include <stdio.h>
#include <wchar.h>
struct CS { char s[4]; int n; } cs = { .s = "abc", .s[1] = 'x' };
struct CS cs2 = { "abc", .s[3] = 'd' };
struct CS cs3 = { .s[1] = 'x', .s = "abc" };
struct S8 { char s[8]; int n; } s8 = { .s = "abc", .s[6] = 'q', .n = 1 };
struct S3 { char s[3]; int n; } s3 = { .s = "abc", .s[1] = 'x' };
struct CS z = { .s = "", .s[1] = 'x' };
struct CS z2 = { .s = "abc", .s[1] = 0 };
char s5[2][4] = { "abc", "def", [0][1] = 'x', [1][2] = 'y' };
struct WS { wchar_t w[4]; } ws = { .w = L"abc", .w[1] = L'x' };
union U { char s[4]; int i; } u = { .s = "abc", .s[1] = 'x' };
struct LU { union U u; char s[4]; };
int main(void) {
	struct CS l = { .s = "abc", .s[1] = 'x' };
	struct LU lu = { .s = "abc", .s[1] = 'x' };
	struct LU *plu = &lu;
	printf("%s %d | %.4s %d | %s | %s %d %d %d | %.3s %d | %d %d %d | %s | %s %s\n", cs.s, cs.n, cs2.s, cs2.n, cs3.s, s8.s, s8.s[6], s8.s[7], s8.n, s3.s, s3.n, z.s[0], z.s[1], z.s[2], z2.s, s5[0], s5[1]);
	printf("%d %d %d %d | %s | %s | %s\n", (int)ws.w[0], (int)ws.w[1], (int)ws.w[2], (int)ws.w[3], u.s, l.s, plu->s);
	return 0;
}
`
	// gcc 13.
	const exp = `axc 0 | abcd 0 | abc | abc 113 0 1 | axc 0 | 0 120 0 | a | axc dey
97 120 99 0 | axc | axc | axc`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}
