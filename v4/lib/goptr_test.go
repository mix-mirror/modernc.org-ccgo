// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGoPtr checks the conversion of pointer parameters and locals to Go
// pointers, see goptr.go: the program behaves as compiled by a C compiler,
// the parameters that only dereference, compare or pass on the pointer are
// typed unsafe.Pointer, the callers' variables stay out of the TLS frame, and
// a parameter whose value escapes keeps its uintptr type and pins the
// caller's variable.
func TestGoPtr(t *testing.T) {
	const src = `
#include <stdarg.h>
#include <stdio.h>
#include <string.h>
#include "inl.h"

typedef union { long u64; double f64; int i32; void *ptr; } U;
typedef struct { int tag; U u; } V;
typedef struct { V v; int tab[2][3]; char name[8]; } S;

/* out parameters */
static int geti(int *pres, int v) { *pres = v * 2; return v > 0; }
static int getl(long *pres, long v) { if (pres) *pres = v + 1; return 0; }
int getx(int *pres, int v) { return geti(pres, v) + 1; } /* exported and called through a pointer: keeps a wrapper */
static int chain(int *p, int v) { return getx(p, v); }
static int (*fp)(int *, int) = getx;

/* objects through a pointer */
static long rd(const V *v) { return v->u.u64 + v->u.i32 + v->tag; }
static void wr(V *v, int t) { v->tag = t; v->u.u64 = 0; v->u.i32 = t * 2; }
static int grid(S *s) { int r = 0; for (int i = 0; i < 2; i++) for (int j = 0; j < 3; j++) r += s->tab[i][j]; s->tab[1][2] = 99; s->name[0] = 'n'; s->name[1] = 0; return r + (*s).tab[1][2] + s->v.u.i32; }
static int row(int (*p)[3]) { return (*p)[0] + p[0][1] + (*p)[2]; }
static V cp(V *v) { V r = *v; r.tag++; *v = r; return r; }

/* alias locals, arithmetic, comparisons, increments */
static int walk(int *p, int n) { int s = 0; int *e = p + n; while (p < e) { s += *p; p++; } for (int *q = e - 1; q >= e - n; q--) s += *q; p -= n; return s + *p; }
static int cmp(const int *p, const int *q) { return p == q ? 1 : p < q ? 2 : p != NULL ? 3 : 0; }
static int tricky(int *p, int *q, int k) { int a = (*p)++, b = ++*p, c = (*p += k), d = (*p -= 1), e = (k > 0 ? *p : *q), f = (*q = 7, *q + *p); *(char *)p = 1; return a + b + c + d + e + f + *(int *)(void *)q; }
static int mixed(S *s, int use) { int local = 11; int *p; if (use) p = &local; else p = &s->v.tag; *p += 1; return *p + local; }
static int rec(int *p, int n) { if (n == 0) return *p; *p += n; return rec(p, n - 1); }
static int vsum(int *out, int n, ...) { va_list ap; int s = 0; va_start(ap, n); for (int i = 0; i < n; i++) s += va_arg(ap, int); va_end(ap); *out = s; return s; }

/* sinks: the caller's variable stays in the frame */
static int *keep;
static void store(int *p) { keep = p; }
static int *ret(int *p) { return p + 1; }
static long toint(int *p) { return (long)p & 1; }
static int sentinel(int *p) { return p == (int *)1; }
static void vialibc(char *p) { memset(p, 'a', 3); p[3] = 0; }
static int viava(int n, ...) { va_list ap; va_start(ap, n); int *p = va_arg(ap, int *); va_end(ap); return *p; }

/* recursion: a cycle whose only frame the conversion removes keeps one frame, see goPtrFrames */
static int selfrec(int n) { int x = n; if (n <= 0) return 0; return geti(&x, 1) + selfrec(n - 1); }
static int mb(int n);
static int ma(int n) { int x = n; if (n <= 0) return 0; return geti(&x, 1) + mb(n - 1); }
static int mb(int n) { int y = n; store(&y); int k = *keep; return ma(n - 1) + k; }
static int c2(int n);
static int c1(int n) { int x = n; if (n <= 0) return 0; return geti(&x, 1) + c2(n - 1); }
static int c2(int n) { int y = n; return geti(&y, 1) + c1(n - 1); }
int cyc_a(int n);
int cyc_b(int n) { int z = n; if (n <= 0) return 0; return geti(&z, 1) + cyc_a(n - 1); }
int cyc_a(int n) { int w = n; return viainl(&w, n); }
static int vrec(int n) { int x = n; if (n <= 0) return 0; printf("%d", geti(&x, 1)); return vrec(n - 1); }
static int plain(int n) { return n <= 0 ? 0 : plain(n - 1) + 1; }

int main(void) {
	int x = 10, y = 20, z, out;
	long l;
	V v = {3, {.i32 = 4}};
	S s = {{1, {.i32 = 2}}, {{1, 2, 3}, {4, 5, 6}}, "s"};
	int arr[5] = {1, 2, 3, 4, 5};
	int sk, sk2 = 0, sk3 = 5;
	char cb[8];
	int r;

	r = geti(&x, 21); printf("%d %d\n", r, x);
	r = getl(&l, 41); printf("%d %ld\n", r, l);
	r = getx(&y, 5); printf("%d %d\n", r, y);
	r = chain(&z, 6); printf("%d %d\n", r, z);
	r = fp(&z, 8); printf("%d %d\n", r, z);
	printf("%ld\n", rd(&v));
	wr(&v, 6); printf("%d %d\n", v.tag, v.u.i32);
	printf("%d %s\n", grid(&s), s.name);
	printf("%d\n", row(&s.tab[1]));
	{ V c = cp(&v); printf("%d %d %d\n", c.tag, v.tag, c.u.i32); }
	printf("%d\n", walk(arr, 5));
	printf("%d %d %d %d\n", cmp(&x, &x), cmp(&arr[0], &arr[1]), cmp(&x, NULL), cmp(NULL, NULL));
	r = tricky(&x, &y, 3); printf("%d %d %d\n", r, x, y);
	r = mixed(&s, 1); printf("%d\n", r); r = mixed(&s, 0); printf("%d %d\n", r, s.v.tag);
	x = 1; r = rec(&x, 4); printf("%d %d\n", r, x);
	r = vsum(&out, 3, 1, 2, 3); printf("%d %d\n", r, out);
	store(&sk); *keep = 9; printf("%d\n", sk);
	r = *ret(&arr[1]); printf("%d\n", r);
	printf("%d %d %d\n", toint(&sk2) == toint(&sk2), sentinel(&sk3), viava(1, &sk3));
	vialibc(cb); printf("%s\n", cb);
	r = selfrec(3); printf("%d\n", r);
	r = ma(4); printf("%d\n", r);
	r = c1(5); printf("%d\n", r);
	r = cyc_a(2); printf("%d\n", r);
	r = vrec(2); printf("\n%d %d\n", r, plain(7));
	return 0;
}
`
	// ccgo inlines a static inline function defined in a header.
	const hdr = `
int cyc_b(int n);
static inline int viainl(int *p, int n) { return *p + cyc_b(n); }
`
	// gcc -O0
	const expected = `1 42
0 42
2 10
2 12
2 16
11
6 12
122 n
108
7 7 12
31
1 2 3 1
285 1 7
24
13 2
11 11
6 6
9
3
1 0 5
aaa
3
6
6
5
11
0 7
`
	// The parameters converted to unsafe.Pointer, in the order of definition.
	converted := []string{"geti.pres", "getl.pres", "getx.pres", "chain.p", "rd.v", "wr.v", "grid.s", "row.p", "cp.v", "walk.p", "cmp.p", "cmp.q", "tricky.p", "tricky.q", "mixed.s", "rec.p", "vsum.out"}
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	for _, v := range []struct{ fn, src string }{{filepath.Join(dir, "inl.h"), hdr}, {cfn, src}} {
		if err := os.WriteFile(v.fn, []byte(v.src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, mode := range []string{"on", "all", "off"} {
		t.Run(mode, func(t *testing.T) {
			ofn := filepath.Join(dir, "main_"+mode+".go")
			var out bytes.Buffer
			if err := NewTask(goos, goarch, []string{"ccgo", "--goptr=" + mode, "-o", ofn, cfn}, &out, &out, nil).Main(); err != nil {
				t.Fatalf("%v\n%s", err, out.Bytes())
			}

			b, err := os.ReadFile(ofn)
			if err != nil {
				t.Fatal(err)
			}

			bin := filepath.Join(dir, "main_"+mode+enforceBinaryExt(""))
			if shOut, err := shell(false, "go", "build", "-o", bin, ofn); err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			shOut, err := shell(false, bin)
			if err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			if g, e := string(shOut), expected; g != e {
				t.Fatalf("got\n%s\nexpected\n%s", g, e)
			}

			// Signatures: name(params...)
			var got []string
			for _, m := range regexp.MustCompile(`(?m)^func (\w+)\((.*?)\)`).FindAllStringSubmatch(string(b), -1) {
				for _, p := range strings.Split(m[2], ", ") {
					if a := strings.Fields(p); len(a) == 2 && a[1] == "unsafe.Pointer" {
						got = append(got, strings.TrimSuffix(m[1], goPtrSuffix)+"."+a[0])
					}
				}
			}
			var want []string
			if mode != "off" {
				want = converted
			}
			if g, e := fmt.Sprint(got), fmt.Sprint(want); g != e {
				t.Errorf("Go pointer parameters:\ngot  %s\nwant %s", g, e)
			}

			// The wrapper of the exported function keeps the C ABI.
			if mode != "off" {
				if !regexp.MustCompile(`(?m)^func getx\(tls \*libc\.TLS, pres uintptr, v int32\) int32 \{\s+return getx__ccgo_gp\(tls, unsafe\.Pointer\(pres\), v\)`).Match(b) {
					t.Errorf("missing or malformed wrapper of getx")
				}
			}

			// Frames: main keeps one for sk, sk2, sk3, cb and arr; mb for
			// store(&y) and vrec for printf. selfrec, c1 and cyc_a lost
			// theirs and keep a one-slot frame for the recursion, their
			// partners c2 and cyc_b do not need one.
			var frames []string
			for _, fn := range strings.Split(string(b), "\nfunc ")[1:] {
				if i := strings.Index(fn, "("); i > 0 && strings.Contains(fn, "tls.Alloc(") {
					frames = append(frames, fn[:i])
				}
			}
			want = []string{"selfrec", "mb", "c1", "cyc_a", "vrec", "main1"}
			if mode == "off" {
				want = []string{"mixed", "selfrec", "ma", "mb", "c1", "c2", "cyc_b", "cyc_a", "vrec", "main1"}
			}
			if g, e := fmt.Sprint(frames), fmt.Sprint(want); g != e {
				t.Errorf("functions with a TLS frame:\ngot  %s\nwant %s", g, e)
			}
		})
	}
}

// TestGoPtrSideEffects checks that an operand whose value is a constant but
// which has side effects, such as (f(), 0), is evaluated and not replaced by
// its value: in a comparison with a Go pointer, as an offset or index of one,
// assigned to one, and in the address of a local passed to a converted
// parameter. Found by TestCSmith, seed 2631221827025167448.
func TestGoPtrSideEffects(t *testing.T) {
	const src = `
#include <stdio.h>

static int n;
static int f(void) { return ++n; }

static int cmp(int *p) { return (f(), (void *)0) != p; }
static int cmp2(int *p) { return p == (f(), (int *)0); }
static int off(int *p) { return *(p + (f(), 1)); }
static int idx(int *p) { return p[(f(), 1)]; }
static int neg(int *p) { int *q = p + 2; return *(q - (f(), 1)); }
static int asg(int *p) { int *q = p; int r = *q; q = (f(), (int *)0); return r + (q == 0); }
static int cpa(int *p) { int *q = p; q += (f(), 1); return *q; }
static int cps(int *p) { int *q = p + 2; q -= (f(), 2); return *q; }
static int deref(int *p) { return *p; }

int main(void) {
	int a[3] = {10, 20, 30};
	int x = 5;
	int arr[2] = {7, 8};
	int r;

	r = cmp(&x); printf("cmp %d %d\n", r, n);
	r = cmp2(&x); printf("cmp2 %d %d\n", r, n);
	r = off(a); printf("off %d %d\n", r, n);
	r = idx(a); printf("idx %d %d\n", r, n);
	r = neg(a); printf("neg %d %d\n", r, n);
	r = asg(&x); printf("asg %d %d\n", r, n);
	r = cpa(a); printf("cpa %d %d\n", r, n);
	r = cps(a); printf("cps %d %d\n", r, n);
	r = deref(&arr[(f(), 1)]); printf("root-index %d %d\n", r, n);
	r = deref(arr + (f(), 1)); printf("root-offset %d %d\n", r, n);
	r = deref(&a[0] + (f(), 2)); printf("root-addr-offset %d %d\n", r, n);
	return 0;
}
`
	// gcc -O0
	const expected = `cmp 1 1
cmp2 0 2
off 20 3
idx 20 4
neg 20 5
asg 6 6
cpa 20 7
cps 10 8
root-index 8 9
root-offset 8 10
root-addr-offset 30 11
`
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfn, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"on", "all", "off"} {
		t.Run(mode, func(t *testing.T) {
			ofn := filepath.Join(dir, "main_"+mode+".go")
			var out bytes.Buffer
			if err := NewTask(goos, goarch, []string{"ccgo", "--goptr=" + mode, "-o", ofn, cfn}, &out, &out, nil).Main(); err != nil {
				t.Fatalf("%v\n%s", err, out.Bytes())
			}

			bin := filepath.Join(dir, "main_"+mode+enforceBinaryExt(""))
			if shOut, err := shell(false, "go", "build", "-o", bin, ofn); err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			shOut, err := shell(false, bin)
			if err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			if g, e := string(shOut), expected; g != e {
				t.Fatalf("got\n%s\nexpected\n%s", g, e)
			}
		})
	}
}
