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
)

// TestBitFieldStores checks the merging of bit-field assignments in a row, see
// bitFieldStores, against the C compiler, and where the result does not depend
// on the layout of the bit-fields, what is merged: full assigns every bit of a
// byte, which becomes a plain store, glob merges two assignments into one, the
// overlapping members of the union are not merged. The other functions cover
// values computed from locals, _Bool and signed bit-fields, wider units, members
// of members, a local struct, a statement expression and the runs that must
// not merge. Every field is printed, so a store clobbering its neighbours
// shows.
func TestBitFieldStores(t *testing.T) {
	const src = `
#include <stdio.h>
#include <string.h>

struct S {
	int i;
	unsigned char a:1, b:1, c:1, d:1, e:1, f:1, g:1, h:1;
	unsigned char k:3, m:2;
	_Bool t:1;
	signed char s:4;
	unsigned short x:5, y:11;
	unsigned long long u:40, v:24;
	struct { unsigned p:3, q:5; } in;
	int j;
};

union U { unsigned a:3; unsigned b:5; unsigned c; };

struct S gs;

void pr(const char *tag, struct S *p) {
	printf("%s %d %u%u%u%u%u%u%u%u %u %u %d %d %u %u %llx %llx %u %u %d\n", tag, p->i,
		p->a, p->b, p->c, p->d, p->e, p->f, p->g, p->h, p->k, p->m, p->t, p->s,
		p->x, p->y, (unsigned long long)p->u, (unsigned long long)p->v, p->in.p, p->in.q, p->j);
}

void full(struct S *p) {
	p->a = 1; p->b = 0; p->c = 1; p->d = 0;
	p->e = 1; p->f = 1; p->g = 0; p->h = 1;
}

void partial(struct S *p, int n) {
	p->k = n; p->m = n > 2 && n < 10;
	p->t = 2;
	p->s = -3;
}

void wide(struct S *p, unsigned n) {
	p->x = 31; p->y = n * 3 + 1;
	p->u = 0x123456789aULL; p->v = 0xabcdef;
	p->in.p = 5; p->in.q = ~n;
}

void local(int n) {
	struct S s;
	memset(&s, 0xff, sizeof s);
	s.a = 0; s.b = 0; s.k = n; s.m = 1; s.t = 0;
	pr("local", &s);
}

void unmerged(struct S *p, struct S *q) {
	p->a = 1; p->b = q->a;
lbl:
	p->c = 1; p->d = 1; p->e = 0;
	p->k = 1; p->k = 2;
	if (q->i == 12345) {
		q->i = 0;
		goto lbl;
	}
}

void taken(struct S *p) {
	struct S **pp = &p;
	(*pp)->a = 0; p->b = 1; p->c = 0;
}

void glob(void) {
	gs.a = 1; gs.h = 1;
}

void unionStores(union U *u) {
	u->c = 0;
	u->a = 5; u->b = 17;
}

int stmtExpr(struct S *p) {
	return ({ p->g = 0; p->h = 0; p->h + 40; });
}

int main(void) {
	struct S s, q;
	union U u;
	memset(&s, 0, sizeof s);
	memset(&q, 0xff, sizeof q);
	s.i = 42;
	s.j = -42;
	full(&s); pr("full", &s);
	partial(&s, 3); pr("partial", &s);
	wide(&s, 7); pr("wide", &s);
	local(6);
	q.i = 12345;
	unmerged(&s, &q); pr("unmerged", &s);
	taken(&s); pr("taken", &s);
	glob(); pr("glob", &gs);
	unionStores(&u); printf("union %u %u %u\n", u.a, u.b, u.c & 0x1f);
	printf("stmtExpr %d\n", stmtExpr(&s)); pr("stmtExpr", &s);
	return 0;
}
`
	dir := t.TempDir()
	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, enforceBinaryExt("cbin"))
	if out, err := exec.Command(hostCC, "-o", bin, "-w", cFile).CombinedOutput(); err != nil {
		t.Skipf("hostCC cannot build the reference: %v\n%s", err, out)
	}

	cOut, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("C binary failed: %v", err)
	}

	goFile := filepath.Join(dir, "test.go")
	var stdout, stderr bytes.Buffer
	if err := NewTask(goos, goarch, []string{"ccgo", "-o", goFile, cFile}, &stdout, &stderr, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, stderr.Bytes())
	}

	b, err := os.ReadFile(goFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range []struct {
		fn    string
		calls int
		store bool
	}{
		{"full", 0, true},
		{"glob", 1, false},
		{"unionStores", 2, false},
	} {
		body := string(b)
		i := strings.Index(body, "\nfunc "+v.fn+"(")
		if i < 0 {
			t.Errorf("%s not found\n%s", v.fn, b)
			continue
		}

		body = body[i:]
		body = body[:strings.Index(body, "\n}\n")]
		if g, e := strings.Count(body, "BitFieldPtr"), v.calls; g != e {
			t.Errorf("%s: got %d bit-field helper calls, want %d\n%s", v.fn, g, e, body)
		}
		if g, e := strings.Contains(body, "*(*uint8)("), v.store; g != e {
			t.Errorf("%s: got plain byte store %v, want %v\n%s", v.fn, g, e, body)
		}
	}

	goBin := filepath.Join(dir, enforceBinaryExt("gobin"))
	if err := inDir(dir, func() error {
		if out, err := exec.Command("go", "mod", "init", "test").CombinedOutput(); err != nil {
			return fmt.Errorf("go mod init: %v\n%s", err, out)
		}

		if out, err := exec.Command("go", "get", *oLibc+libcVersion).CombinedOutput(); err != nil {
			return fmt.Errorf("go get: %v\n%s", err, out)
		}

		if err := xwork(); err != nil {
			return err
		}

		if out, err := exec.Command("go", "build", "-o", goBin, goFile).CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	goOut, err := exec.Command(goBin).Output()
	if err != nil {
		t.Fatalf("Go binary failed: %v", err)
	}

	cOut = bytes.ReplaceAll(bytes.TrimSpace(cOut), []byte("\r"), nil)
	goOut = bytes.ReplaceAll(bytes.TrimSpace(goOut), []byte("\r"), nil)
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:\n%s\nGo:\n%s", cOut, goOut)
	}
	t.Logf("\n%s", goOut)
}
