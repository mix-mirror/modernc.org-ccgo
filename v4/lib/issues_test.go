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
	"strings"
	"testing"

	"golang.org/x/mod/semver"
	"modernc.org/cc/v4"
)

// testHostCCvsCcgo compiles and runs the C program src with the host C
// compiler and with ccgo, fails t when the outputs differ and returns the
// output.
func testHostCCvsCcgo(t *testing.T, src string) string {
	t.Helper()
	return testHostCCvsCcgoFiles(t, map[string]string{"test.c": src})
}

// testHostCCvsCcgoFiles is testHostCCvsCcgo for a program spread over several
// files. The program is files["test.c"]; the other entries are written next to
// it under their names so that it can include them.
func testHostCCvsCcgoFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for nm, src := range files {
		if err := os.WriteFile(filepath.Join(dir, nm), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cFile := filepath.Join(dir, "test.c")

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

// TestIssue48 verifies that a parameter of an inlined function whose address is
// taken, explicitly or through a union member, is read and assigned as the
// frame slot the inline site spilled it to, so that a write through the address
// is seen by a later whole-value read and a whole-value assignment is seen by a
// later member read.
//
// See https://gitlab.com/cznic/ccgo/-/issues/48
func TestIssue48(t *testing.T) {
	const hdr = `
typedef union { unsigned long bits; } Ref;
typedef struct { int x, y; } Pt;
static void bump(int *p) { *p += 100; }
static inline Ref broken(Ref ref) { ref.bits &= ~1UL; return ref; }
static inline Ref fixed(Ref ref) { Ref result = { .bits = ref.bits & ~1UL }; return result; }
static inline int addr_scalar(int x) { bump(&x); return x; }
static inline int addr_cond(int x) { int *p = &x; *p += 1; if (x > 5) return x; return -x; }
static inline unsigned long assign_whole(Ref ref, Ref other) { ref.bits += 1; ref = other; ref.bits += 2; return ref.bits; }
static inline Ref nested(Ref ref) { ref.bits |= 8; return broken(ref); }
static inline Pt viaptr(Pt p) { Pt *q = &p; q->y = 77; return p; }
static inline int loop(int n) { int *p = &n; while (*p > 0) { (*p)--; } return n; }
static inline void noread(int x) { bump(&x); }
static inline int arrp(int a[]) { int **pp = &a; (*pp)++; return a[0]; }
static inline long swap2(long a, long b) { long *pa = &a, *pb = &b; long t = *pa; *pa = *pb; *pb = t; return a * 100 + b; }
`
	const src = `
#include <stdio.h>
#include "ref.h"
static inline __attribute__((always_inline)) int ai(int x) { bump(&x); return x; }
int main(void) {
	Ref r = { .bits = 3 }, o = { .bits = 40 };
	Pt p = { 1, 2 };
	int a[2] = { 5, 6 };
	printf("%lu %lu %lu\n", broken(r).bits, fixed(r).bits, broken(r).bits);
	printf("%d %d %d %d\n", addr_scalar(1), addr_cond(1), addr_cond(9), ai(7));
	printf("%lu %lu\n", assign_whole(r, o), nested(r).bits);
	printf("%d %d\n", viaptr(p).x, viaptr(p).y);
	printf("%d %d %ld\n", loop(5), arrp(a), swap2(4, 5));
	noread(1);
	printf("%d %d %d\n", r.bits == 3, p.y == 2, a[0] == 5);
	return 0;
}
`
	const exp = `2 2 2
101 -2 10 107
42 10
1 77
0 6 504
1 1 1`
	if g := testHostCCvsCcgoFiles(t, map[string]string{"test.c": src, "ref.h": hdr}); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue52 verifies that a call through a function pointer parameter of an
// inlined function is emitted as a call through the pointer, whatever the
// argument was: a struct field, a function designator, a value held in a
// variable, a pointer forwarded from an enclosing inlined call, or a parameter
// declared with function type, and that a parameter overwritten through its
// address is called through the frame slot.
//
// See https://gitlab.com/cznic/ccgo/-/issues/52
func TestIssue52(t *testing.T) {
	const hdr = `
typedef void (*freefunc)(void *);
typedef int (*binop)(int, int);
static int calls;
static void do_free(void *p) { (void)p; calls++; }
static int add(int a, int b) { return a + b; }
static int mul(int a, int b) { return a * b; }
static inline void call1(void *obj, freefunc f) { f(obj); }
static inline void call2(void *obj, freefunc f) { f(obj); f(obj); }
static inline int apply(binop op, int a, int b) { return op(a, b); }
static inline int apply2(binop op, int a, int b) { return op(a, b) + op(b, a); }
static inline int deref_call(binop op, int a, int b) { return (*op)(a, b); }
static inline int fwd(binop op, int a, int b) { return apply(op, a, b) * 2; }
static inline int ftype_param(int op(int, int), int a, int b) { return op(a, b); }
static inline int pinned_fp(binop op, int a, int b) { binop *pp = &op; return (*pp)(a, b); }
static inline int cond_call(binop op, int a, int b) { return op ? op(a, b) : -1; }
static inline int null_check(binop op) { return op == 0; }
static void set(binop *p) { *p = mul; }
static inline int repoint(binop op, int a, int b) { binop *pp = &op; *pp = mul; return op(a, b); }
static inline int repoint2(binop op, int a, int b) { set(&op); return op(a, b) + op(a, a); }
`
	const src = `
#include <stdio.h>
#include "cb.h"
struct type { const char *name; freefunc tp_free; binop op; };
static struct type T = { "T", do_free, add };
int main(void) {
	int x; struct type *t = &T;
	call1(&x, t->tp_free); call1(&x, do_free); call2(&x, t->tp_free);
	printf("%d\n", calls);
	printf("%d %d %d %d\n", apply(t->op, 2, 3), apply(mul, 2, 3), apply2(t->op, 2, 3), deref_call(mul, 4, 5));
	printf("%d %d %d %d %d\n", fwd(add, 1, 2), ftype_param(mul, 3, 3), pinned_fp(add, 5, 6), cond_call(add, 1, 1), cond_call(0, 1, 1));
	printf("%d %d %d %d\n", null_check(0), null_check(add), repoint(add, 3, 4), repoint2(add, 3, 4));
	return 0;
}
`
	const exp = `4
5 6 10 20
6 9 11 2 -1
1 0 12 21`
	if g := testHostCCvsCcgoFiles(t, map[string]string{"test.c": src, "cb.h": hdr}); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue53 verifies that a discarded value which has already been moved into
// a variable is consumed with an assignment: the result of an inlined call
// passed to a parameter the inlined body never reads, and likewise a postfix
// increment or an assignment expression in that position.
//
// See https://gitlab.com/cznic/ccgo/-/issues/53
func TestIssue53(t *testing.T) {
	const hdr = `
#include <stdlib.h>
struct type { long basicsize; };
struct obj { struct type *ob_type; long size; };
static int g_calls;
static int g(void) { g_calls++; return 7; }
static inline struct type *get_type(struct obj *o) { return o->ob_type; }
static inline void *realloc_with_type(struct type *tp, void *ptr, size_t size) {
#ifdef FREE_THREADED
	if (tp->basicsize < 0) return NULL;
#endif
	void *mem = realloc(ptr, size);
	return mem;
}
static inline int unused(int x) { return 1; }
static inline int unused2(int x, int y) { return y; }
static inline void unused_void(int x) { }
static inline int inl(int v) { return v * 2; }
static inline int assigned_only(int x) { x = 5; return 3; }
static inline int nested(int v) { return unused(inl(v)) + unused2(inl(v), inl(v + 1)); }
`
	const src = `
#include <stdio.h>
#include "un.h"
struct obj *resize(struct obj *op, long n) {
	char *mem = (char *)op;
	mem = (char *)realloc_with_type(get_type(op), mem, sizeof(struct obj) + (size_t)n);
	if (mem == NULL) return NULL;
	op = (struct obj *)mem;
	op->size = n;
	return op;
}
int main(void) {
	static struct type t = { 16 };
	struct obj *o = calloc(1, sizeof *o);
	int x = 1, a = 0, r1, r2, r3, r4, r5, r6, r7;
	o->ob_type = &t;
	o = resize(o, 3);
	printf("%ld\n", o->size);
	r1 = unused(x++); r2 = unused(a = 5); r3 = unused(g());
	printf("%d %d %d %d %d\n", r1, x, r2, a, r3);
	r4 = unused(inl(4)); r5 = unused(x); r6 = unused(3); r7 = unused2(inl(1), inl(2));
	printf("%d %d %d %d %d\n", r4, r5, r6, r7, g_calls);
	unused_void(inl(1)); unused_void(x++); unused_void(g());
	inl(9); (inl(1), inl(2)); unused(inl(1));
	r1 = assigned_only(inl(1)); r2 = nested(2);
	printf("%d %d %d %d\n", x, r1, r2, g_calls);
	return 0;
}
`
	const exp = `3
1 2 1 5 1
1 1 1 4 1
3 3 7 2`
	if g := testHostCCvsCcgoFiles(t, map[string]string{"test.c": src, "un.h": hdr}); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue56 verifies that an enumerator converted to an integer type that
// cannot hold its value is converted at run time, as an integer literal in the
// same place is, instead of by a Go constant conversion that does not compile.
// The enumerator appears in initializers, assignments, arguments, compound
// assignments, casts, a switch case, a function-local enum, and in contexts
// where the value fits, which must stay plain conversions.
//
// See https://gitlab.com/cznic/ccgo/-/issues/56
func TestIssue56(t *testing.T) {
	const src = `
#include <stdio.h>
#include <stdbool.h>
enum opcode { SMALL = 5, BINBYTES8 = 0x8e, NEG = -200, BIG = 70000 };
#define K 0x8e
static const int ci = 0x8e;
static char arr[] = { BINBYTES8, NEG, 1 };
struct S { char c; unsigned char u; } s = { BINBYTES8, NEG };
static void f(char c) { printf("%d ", c); }
int main(void) {
	char c = BINBYTES8, l[2] = { BINBYTES8, NEG };
	printf("%d %d %d %d %d %d %d %d\n", c, arr[0], arr[1], arr[2], s.c, s.u, l[0], l[1]);
	c = 0; c = BINBYTES8; f(BINBYTES8); f(NEG);
	{ short sh = BIG; unsigned char u = NEG; printf("%d %d %d\n", c, sh, u); }
	{ char c1 = K, c2 = ci, c3 = BINBYTES8 + 1, c4 = (char)BINBYTES8, c5 = BINBYTES8 * 2; printf("%d %d %d %d %d\n", c1, c2, c3, c4, c5); }
	{ char c = 0; c += BINBYTES8; unsigned char u = 0; u -= BINBYTES8; int i = (char)BINBYTES8; printf("%d %d %d %d\n", c, u, i, c == BINBYTES8); }
	switch (c) { case (char)BINBYTES8: puts("hit"); break; default: puts("miss"); }
	{ enum local { L8 = 0x8e, LN = -200 }; char c = L8; unsigned char u = LN; short sh = L8; printf("%d %d %d\n", c, u, sh); }
	{
		enum opcode e = BINBYTES8;
		bool b1 = BINBYTES8, b2 = SMALL;
		char cs = SMALL;
		unsigned u = BINBYTES8; unsigned long ul = BIG;
		double d = NEG; float fl = BIG;
		long lo = NEG; unsigned short us = NEG;
		BINBYTES8;
		printf("%d %d %d %d %u %lu %g %g %ld %u %d\n", e, b1, b2, cs, u, ul, d, fl, lo, us, e == BINBYTES8);
	}
	return 0;
}
`
	abi, err := cc.NewABI(goos, goarch)
	if err != nil {
		t.Fatal(err)
	}

	// gcc 13 with -funsigned-char and with the default signed char.
	exp := `142 142 56 1 142 56 142 56
142 56 142 4464 56
142 142 143 142 28
142 114 142 1
hit
142 56 142
142 1 1 5 142 70000 -200 70000 -200 65336 1`
	if abi.SignedChar {
		exp = `-114 -114 56 1 -114 56 -114 56
-114 56 -114 4464 56
-114 -114 -113 -114 28
-114 114 -114 0
hit
-114 56 142
142 1 1 5 142 70000 -200 70000 -200 65336 1`
	}
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue55 verifies that a parenthesized string literal initializes a
// character array like the bare literal does: a struct member, a plain array,
// doubly parenthesized, a wide string, an array without room for the NUL, a
// union member, elements of a two-dimensional array, a literal later overridden
// by a designated element, static and automatic storage, and a nested struct.
//
// See https://gitlab.com/cznic/ccgo/-/issues/55
func TestIssue55(t *testing.T) {
	const src = `
#include <stdio.h>
#include <stdint.h>
#include <wchar.h>
struct S { int len; uint8_t data[6]; };
#define INIT(LITERAL) { .len = sizeof(LITERAL) - 1, .data = (LITERAL) }
static struct S s = INIT("hello");
static char a[6] = ("hello");
static char a2[6] = (("hello"));
static wchar_t w[6] = (L"hello");
static char *p = ("hello");
static char e[5] = ("hello");
static union U { char s[6]; int i; } u = { .s = ("hello") };
static char two[2][6] = { ("hello"), ("world") };
static struct O { char s[4]; int n; } o = { .s = ("abc"), .s[1] = 'x' };
static char big[10] = ("hi");
static char plain[6] = "hello";
static struct O po = { .s = "abc", .s[1] = 'x' };
struct N { int k; struct S in; } n = { 1, INIT("abcde") };
int main(void) {
	char l[6] = ("hello");
	struct S ls = INIT("local");
	union U lu = { .s = ("wxyz") };
	printf("%d %s %zu\n", s.len, (char *)s.data, sizeof s);
	printf("%s %s %d %d %s %.5s %s %s %s\n", a, a2, (int)w[1], (int)w[5], p, e, u.s, two[0], two[1]);
	printf("%s %d %s %d %d %s %d %s %s\n", o.s, o.n, big, big[2], big[9], n.in.data, n.in.len, l, ls.data);
	char lp[4] = "abc";
	printf("%s %d %s %s %s\n", lu.s, ls.len, plain, po.s, lp);
	return 0;
}
`
	const exp = `5 hello 12
hello hello 101 0 hello hello hello hello world
axc 0 hi 0 0 abcde 5 hello local
wxyz 5 hello axc abc`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue57 verifies that a tagged enum declared inside a struct or union
// nested in another aggregate is defined at package level when a function
// refers to it by name: through a tagged nested struct, an anonymous nested
// struct, a struct inside a union, an array field, a union field, a typedef'd
// struct and an anonymous struct behind a pointer, as a variable, parameter and
// file-scope variable type.
//
// See https://gitlab.com/cznic/ccgo/-/issues/57
func TestIssue57(t *testing.T) {
	const src = `
#include <stdio.h>
struct result { int kind; struct error { enum error_kind { E_A = 0, E_B = 1 } kind; void *exc; } err; };
struct anon { int kind; struct { enum anon_kind { A_A = 5, A_B } kind; } err; };
union u { struct s { enum deep { D_A = 7 } kind; } s; int i; };
struct arr { struct { enum ak { K1 = 2, K2 } k; } a[2]; };
struct un { union { enum uk { U1 = 4 } k; int i; } u; };
typedef struct { struct { enum tk { T1 = 8 } k; } in; } T;
struct ptr { struct { enum pk { P1 = 6 } k; } *p; };
static enum error_kind g = E_B;
static int f(enum error_kind k) { return k + 10; }
int main(void) {
	struct result r = { .kind = 1, .err = { .kind = E_B, .exc = 0 } };
	enum error_kind k = r.err.kind;
	struct error e = { E_A, 0 };
	struct anon an = { 1, { A_B } };
	enum anon_kind ak = an.err.kind;
	union u x = { .s = { D_A } };
	enum deep d = x.s.kind;
	struct arr ar = { { { K2 }, { K1 } } };
	enum ak av = ar.a[0].k;
	struct un sn = { .u.k = U1 };
	enum uk uv = sn.u.k;
	T t = { { T1 } };
	enum tk tv = t.in.k;
	enum pk pv = P1;
	struct ptr p = { 0 };
	printf("%d %d %d %d %d\n", r.kind, k, e.kind, g, f(E_B));
	printf("%d %d %d %d %d %d %d %d\n", ak, A_A, d, av, ar.a[1].k, uv, tv, pv);
	printf("%d %d\n", p.p == 0, (int)sizeof(struct error) > 0);
	return 0;
}
`
	const exp = `1 1 0 1 11
6 5 7 3 2 4 8 6
1 1`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue62 verifies that the libm functions the compiler calls under their
// __builtin_ names link to the plain libc functions when libc provides no
// builtin of that name. The transcendental results are printed with ten
// significant digits because libc's implementations may differ from the host
// libm in the last digit. The float results are stored in volatile floats
// first because clang on i386 keeps the x87 excess precision of a float
// return value through a cast to double.
//
// See https://gitlab.com/cznic/ccgo/-/issues/62
func TestIssue62(t *testing.T) {
	// Only the musl based linux ports of libc v1.75.7 and older implement
	// nextafter, fma, erf, tgamma and several more of the functions below.
	// The other ports have them since libc commit afc4c418, which comes after
	// v1.75.7, so there the program does not link with an older libc.
	//
	// See https://gitlab.com/cznic/libc/-/issues/56
	if goos != "linux" && semver.Compare(strings.TrimPrefix(libcVersion, "@"), "v1.75.8") < 0 {
		t.Skipf("modernc.org/libc%s lacks some of the tested functions on %s/%s", libcVersion, goos, goarch)
	}

	const src = `
#include <stdio.h>
#include <math.h>
int main(void) {
	double x = 1.0, y = 3.0;
	int q = 0;
	volatile float af = asinhf(1.0f), lf = log1pf(1.0f);
	printf("%.17g %.17g %.17g %.17g\n", nextafter(x, 2.0), nexttoward(x, 2.0L), (double)nextafterf(1.0f, 2.0f), fma(x, 2.0, 3.0));
	printf("%.10g %.10g %.10g %.10g %.10g\n", asinh(x), log1p(x), acosh(2.0), atanh(0.5), erf(0.5));
	printf("%.10g %.10g %.10g %.10g %.10g\n", erfc(0.5), expm1(0.5), lgamma(4.5), (double)af, (double)lf);
	printf("%.17g %.17g %.17g %.17g %.17g %.17g\n", cbrt(27.0), exp2(3.0), fdim(5.0, 2.0), logb(1024.0), remainder(10.0, y), remquo(10.0, y, &q));
	printf("%.17g %.17g %.17g %d %.17g %.17g\n", rint(2.5), scalbn(1.5, 4), tgamma(5.0), q, hypot(3.0, 4.0), copysign(1.0, -2.0));
	return 0;
}
`
	const exp = `1.0000000000000002 1.0000000000000002 1.0000001192092896 5
0.881373587 0.6931471806 1.316957897 0.5493061443 0.5204998778
0.4795001222 0.6487212707 2.453736571 0.8813735843 0.6931471825
3 8 3 10 1 1
2 24 24 3 5 -1`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}

// TestIssue54 verifies that designators reaching into a union member of a
// struct without braces of their own, `.v.Name.id = p, .v.Name.ctx = 7`, lay
// the union out as that member when one of the values is an address: a
// pointer and a scalar, three members, a union first in the struct, a union
// in an array element, a union nested in a union, an array of pointers, the
// scalar before the pointer, and an automatic variable.
//
// See https://gitlab.com/cznic/ccgo/-/issues/54
func TestIssue54(t *testing.T) {
	const src = `
#include <stdio.h>
static const char empty[] = "";
static int g = 42;
struct expr { int kind; union { struct { const char *id; int ctx; } Name; struct { double value; } Constant; } v; int lineno; };
static struct expr d1 = { .kind = 1, .v.Name.id = empty, .v.Name.ctx = 7, .lineno = 1 };
struct S { long a; union { struct { const char *p; int c; } N; double d; } u; long b; };
static struct S s1 = { .a = 1, .u.N.p = empty, .u.N.c = 7, .b = 3 };
struct U0 { union { struct { const char *p; int c; } N; long l; } u; int after; };
static struct U0 u0 = { .u.N.p = empty, .u.N.c = 5, .after = 9 };
struct Three { int k; union { struct { const char *p; int c; short s; } N; double d; } u; };
static struct Three t3 = { .k = 2, .u.N.p = empty, .u.N.c = 3, .u.N.s = 4 };
struct Arr { int k; struct S arr[2]; };
static struct Arr ar = { .k = 1, .arr[1].u.N.p = empty, .arr[1].u.N.c = 8, .arr[0].a = 5 };
struct Nest { int k; union { union { struct { void *p; int c; } in; long l; } inner; double d; } u; };
static struct Nest ne = { .k = 1, .u.inner.in.p = &g, .u.inner.in.c = 6 };
struct PArr { int k; union { struct { void *ps[2]; } A; double d; } u; };
static struct PArr pa = { .k = 1, .u.A.ps[0] = &g, .u.A.ps[1] = empty };
struct Mixed { int k; union { struct { int c; const char *p; } N; double d; } u; };
static struct Mixed mx = { .k = 1, .u.N.c = 5, .u.N.p = empty };
int main(void) {
	struct expr l1 = { .kind = 1, .v.Name.id = empty, .v.Name.ctx = 7, .lineno = 1 };
	printf("%d %d %d %d\n", d1.kind, d1.v.Name.id == empty, d1.v.Name.ctx, d1.lineno);
	printf("%ld %d %d %ld\n", s1.a, s1.u.N.p == empty, s1.u.N.c, s1.b);
	printf("%d %d %d\n", u0.u.N.p == empty, u0.u.N.c, u0.after);
	printf("%d %d %d %d\n", t3.k, t3.u.N.p == empty, t3.u.N.c, t3.u.N.s);
	printf("%d %ld %d %d %ld\n", ar.k, ar.arr[0].a, ar.arr[1].u.N.p == empty, ar.arr[1].u.N.c, ar.arr[1].a);
	printf("%d %d %d\n", ne.k, ne.u.inner.in.p == &g, ne.u.inner.in.c);
	printf("%d %d %d\n", pa.k, pa.u.A.ps[0] == &g, pa.u.A.ps[1] == empty);
	printf("%d %d %d\n", mx.k, mx.u.N.c, mx.u.N.p == empty);
	printf("%d %d %d %d\n", l1.kind, l1.v.Name.id == empty, l1.v.Name.ctx, l1.lineno);
	return 0;
}
`
	const exp = `1 1 7 1
1 1 7 3
1 5 9
2 1 3 4
1 5 1 8 0
1 1 6
1 1 1
1 5 1
1 1 7 1`
	if g := testHostCCvsCcgo(t, src); g != exp {
		t.Fatalf("got\n%s\nexpected\n%s", g, exp)
	}
}
