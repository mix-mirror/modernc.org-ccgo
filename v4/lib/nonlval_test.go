// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestNonLvalueMember checks members of expressions that are not lvalues: the
// result of a call, a conditional expression, an assignment, a comma
// expression and a compound literal. The member can be a union member, an
// array that is indexed or passed to a function, a bit field or a member of an
// anonymous union, and a store through it modifies a temporary. The expected
// output is that of gcc.
func TestNonLvalueMember(t *testing.T) {
	const src = `
#include <stdio.h>
#include <string.h>

typedef union { long long u; double f; } U;
typedef struct { int tag; U u; char z[4]; } V;
typedef struct { int tag; union { long long l; int i; }; unsigned bf : 3; unsigned bf2 : 5; } A;
typedef struct { int tag; char name[40]; } N;

static V a = {1, {.u = 5}, "ab"}, b = {2, {.u = 7}, "cd"};
static A a2 = {1, {.l = 77}, 5, 9}, b2 = {2, {.l = 88}, 6, 10};
static int c = 1;

static V f(void) { V r = {3, {.u = 9}, "ef"}; return r; }
static V g(void) { V r = {4, {.u = 11}, "gh"}; return r; }
static A h(void) { A r = {3, {.l = 99}, 7, 11}; return r; }
static N nm(void) { N r = {1, "0123456789012345678901234567890123456789"}; return r; }

int main(void) {
	V d;
	long long guard1 = 0x1111111111111111LL, guard2 = 0x2222222222222222LL, guard3 = 0x3333333333333333LL;
	long long *p1 = &guard1, *p2 = &guard2, *p3 = &guard3;

	// conditional expression
	printf("%d %lld %c\n", (c ? a : b).tag, (c ? a : b).u.u, (c ? a : b).z[1]);
	printf("%d %lld %c\n", (!c ? a : b).tag, (!c ? a : b).u.u, (!c ? a : b).z[1]);
	printf("%lld %d\n", (c ? f() : g()).u.u, (!c ? f() : g()).tag);
	printf("%lld %lld\n", (c ? (c ? b : a) : a).u.u, (1 ? a : b).u.u + (0 ? a : b).u.u);
	printf("%lld %d %u %u\n", (c ? a2 : b2).l, (!c ? a2 : b2).i, (c ? a2 : b2).bf, (!c ? a2 : b2).bf2);
	(c ? a : b).z[0] = 1;
	printf("%d %d\n", a.z[0], b.z[0]);
	printf("%d\n", (int)strlen((c ? a : b).z));

	// call: the member copied into the frame must not overrun the slot
	printf("%d %lld %c %d\n", f().tag, f().u.u, g().z[0], (int)strlen(g().z));
	printf("%lld %d %u %u\n", h().l, h().i, h().bf, h().bf2);
	puts(nm().name);
	printf("%llx %llx %llx\n", *p1, *p2, *p3);

	// assignment, comma expression, compound literal
	printf("%d %lld %c\n", (d = a).tag, (d = b).u.u, (d = b).z[1]);
	printf("%d %lld %c\n", (c, b).tag, (c, a).u.u, (c, a).z[0]);
	printf("%lld %c\n", ((V){6, {.u = 13}, "ij"}).u.u, ((V){6, {.u = 13}, "ij"}).z[1]);
	return 0;
}
`
	const expected = `1 5 b
2 7 d
9 4
7 12
77 88 5 10
97 99
2
3 9 g 2
99 99 7 11
0123456789012345678901234567890123456789
1111111111111111 2222222222222222 3333333333333333
1 7 d
2 5 a
13 j
`
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfn, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ofn := filepath.Join(dir, "main.go")
	var out bytes.Buffer
	if err := NewTask(goos, goarch, []string{"ccgo", "-o", ofn, cfn}, &out, &out, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, out.Bytes())
	}

	bin := filepath.Join(dir, "main"+enforceBinaryExt(""))
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
}
