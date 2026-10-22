// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUnionMemberFrame checks that reading or writing a union member of an
// automatic variable, of a by-value parameter or of a parameter of an inlined
// function does not move the variable into the TLS frame: the member is
// accessed through unsafe.Pointer(&v) within one expression and v stays a Go
// local. A variable whose address escapes keeps its frame.
func TestUnionMemberFrame(t *testing.T) {
	// ccgo inlines a static inline function defined in a header.
	const hdr = `
typedef union { int i; double d; unsigned long long u; } U;
typedef struct { U u; long long tag; } V;
typedef union { U u; struct { int a, b; } s; } N;

static inline unsigned long long twice(V v) { return v.u.u + (unsigned long long)v.tag; }
static inline unsigned long long once(V v) { return v.u.u; }
`
	const src = `
#include "inl.h"

V mk(unsigned long long x) { V v; v.u.u = x; v.tag = 1; return v; }

unsigned long long byval(V v) { return v.u.u; }
unsigned long long local(double d) { V v; v.u.d = d; v.tag = 1; return v.u.u; }
unsigned long long inlined(V v) { return twice(v) + once(v); }
int nested(void) { struct { V v; int x; } s; s.v.u.u = 0x100000002ull; s.x = 0; return s.v.u.i + s.x; }
unsigned long long nestedunion(void) { N n; n.s.a = 1; n.s.b = 2; return n.u.u + (unsigned long long)n.s.b; }

unsigned long long escapes(void) { V v; V *p = &v; p->u.u = 1; return v.u.u; }
unsigned long long inlinedcall(void) { return once(mk(2)); }
`
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	for _, v := range []struct{ fn, src string }{{filepath.Join(dir, "inl.h"), hdr}, {cfn, src}} {
		if err := os.WriteFile(v.fn, []byte(v.src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ofn := filepath.Join(dir, "main.go")
	var out bytes.Buffer
	if err := NewTask(goos, goarch, []string{"ccgo", "-o", ofn, cfn}, &out, &out, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, out.Bytes())
	}

	b, err := os.ReadFile(ofn)
	if err != nil {
		t.Fatal(err)
	}

	// Go source of every function, keyed by the C name.
	funcs := map[string]string{}
	re := regexp.MustCompile(`(?m)^func [A-Za-z_0-9]*?([a-z]+)\(`)
	s := string(b)
	m := re.FindAllStringSubmatchIndex(s, -1)
	for i, v := range m {
		end := len(s)
		if i+1 < len(m) {
			end = m[i+1][0]
		}
		funcs[s[v[2]:v[3]]] = s[v[0]:end]
	}
	// escapes takes the address of v explicitly. inlinedcall passes a call
	// result to a parameter that is read once, so the parameter is replaced by
	// the call expression, which is not addressable: it is spilled into the
	// frame as before.
	for _, nm := range []string{"byval", "local", "inlined", "nested", "nestedunion", "escapes", "inlinedcall"} {
		fn, ok := funcs[nm]
		if !ok {
			t.Fatalf("function %s not found in\n%s", nm, s)
		}

		hasFrame := strings.Contains(fn, "Alloc(")
		if g, e := hasFrame, nm == "escapes" || nm == "inlinedcall"; g != e {
			t.Errorf("%s: has TLS frame %v, expected %v\n%s", nm, g, e, fn)
		}
	}
}
