// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackedWarning checks that ccgo warns, once per type, where it uses a
// struct or union whose layout packing changes in the target's C compiler,
// see packed.go, and not where packing changes nothing or the type is only
// defined. The members are chosen so that packing changes the layout on every
// target.
func TestPackedWarning(t *testing.T) {
	const src = `
#include <stddef.h>

struct a { char c; int x; } __attribute__((packed));		// member access through a pointer
struct b { unsigned char sha[6]; int sip; } __attribute__((packed));	// sizeof
struct p { void *m; } __attribute__((packed));			// only its alignment changes
struct q { struct p m; char x; } __attribute__((packed));	// local variable
struct r { char c; struct p m; };				// not packed, embeds p
union un { char c; int x; } __attribute__((packed));		// _Alignof
typedef struct { char c; int x; } __attribute__((packed)) T;	// typedef name

#pragma pack(push, 1)
struct pp { char c; int x; };					// #pragma pack, offsetof
#pragma pack(pop)

struct d { char c; int x; } __attribute__((packed));		// defined only
struct n { char a, b; } __attribute__((packed));		// packing changes nothing
struct u { char c; int x; };					// not packed

int fa(unsigned char *buf) { return ((struct a *)buf)->x + ((struct a *)buf)->c; }
unsigned long fb(void) { return sizeof(struct b) + sizeof(struct b); }
int fq(void) { struct q v = {0}; return v.x; }
unsigned long fr(void) { return sizeof(struct r); }
unsigned long fun(void) { return _Alignof(union un); }
unsigned long ft(void) { return sizeof(T); }
unsigned long fpp(void) { return offsetof(struct pp, x); }
unsigned long fn(void) { struct n v = {0}; return sizeof v + v.a; }
unsigned long fu(void) { struct u v = {0}; return sizeof v + v.c; }
`
	all := []string{"struct a", "struct b", "struct q", "struct r", "union un", "T", "struct pp"}
	// The functions using the types, a warning is reported at the first use.
	use := map[string]string{
		"struct a":  "int fa(",
		"struct b":  "unsigned long fb(",
		"struct q":  "int fq(",
		"struct r":  "unsigned long fr(",
		"union un":  "unsigned long fun(",
		"T":         "unsigned long ft(",
		"struct pp": "unsigned long fpp(",
	}
	quiet := []string{"struct p", "struct d", "struct n", "struct u"}
	for _, tc := range []struct {
		flag string
		want []string
	}{
		{"", all},
		{"--no-packed-warning=a,T", []string{"struct b", "struct q", "struct r", "union un", "struct pp"}},
		{"--no-packed-warning", nil},
	} {
		dir := t.TempDir()
		cFile := filepath.Join(dir, "test.c")
		if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}

		args := []string{"ccgo", "-c", "-o", filepath.Join(dir, "test.o.go")}
		if tc.flag != "" {
			args = append(args, tc.flag)
		}
		var stdout, stderr bytes.Buffer
		if err := NewTask(goos, goarch, append(args, cFile), &stdout, &stderr, nil).Main(); err != nil {
			t.Errorf("%q: %v", tc.flag, err)
			continue
		}

		out := stderr.String()
		want := map[string]bool{}
		for _, v := range tc.want {
			want[v] = true
		}
		for _, nm := range append(all, quiet...) {
			switch g, e := strings.Count(out, "warning: "+nm+":"), want[nm]; {
			case e && g != 1:
				t.Errorf("%q: %s: got %d warnings, want 1\n%s", tc.flag, nm, g, out)
			case !e && g != 0:
				t.Errorf("%q: %s: got %d warnings, want none\n%s", tc.flag, nm, g, out)
			case e:
				line := strings.Count(src[:strings.Index(src, use[nm])], "\n") + 1
				if prefix := fmt.Sprintf("%s:%d:", cFile, line); !strings.Contains(out, "\n"+prefix) && !strings.HasPrefix(out, prefix) {
					t.Errorf("%q: %s: want the warning at %s\n%s", tc.flag, nm, prefix, out)
				}
			}
		}
	}
}

// TestWarnABILayout checks --warn-abi-layout: a warning, once per type, where
// ccgo uses a struct or union whose size or alignment differs from the
// target's C compiler for any reason, here bit-fields, and a single warning
// for a type packing changes, the packed one unless that is silenced.
func TestWarnABILayout(t *testing.T) {
	const src = `
struct bf { unsigned char a, b, c; unsigned f:4, g:4; unsigned x; };	// gcc: f and g in byte 3, size 8
struct m { int a; unsigned f:4, g:4; int x; };				// the same layout as gcc
struct u { char c; int x; };
struct a { char c; int x; } __attribute__((packed));

unsigned long fbf(void) { return sizeof(struct bf); }
unsigned long fm(void) { struct m v = {0}; return sizeof v + v.f; }
unsigned long fu(void) { struct u v = {0}; return sizeof v + v.c; }
unsigned long fa(void) { return sizeof(struct a); }
`
	// With the Microsoft bit-field rules, which MinGW gcc and clang use on
	// Windows, f starts a new unit after the char members, as in ccgo, so
	// struct bf is 12 bytes there too.
	bf := goos != "windows"
	const (
		packedTag = "[--no-packed-warning]"
		abiTag    = "[--warn-abi-layout]"
	)
	for _, tc := range []struct {
		flags []string
		want  map[string]string // name: tag of the expected warning
	}{
		{nil, map[string]string{"struct a": packedTag}},
		{[]string{"--warn-abi-layout"}, map[string]string{"struct a": packedTag, "struct bf": abiTag}},
		{[]string{"--warn-abi-layout", "--no-packed-warning"}, map[string]string{"struct a": abiTag, "struct bf": abiTag}},
		{[]string{"--no-packed-warning"}, nil},
	} {
		if !bf {
			delete(tc.want, "struct bf")
		}
		dir := t.TempDir()
		cFile := filepath.Join(dir, "test.c")
		if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}

		args := append([]string{"ccgo", "-c", "-o", filepath.Join(dir, "test.o.go")}, tc.flags...)
		var stdout, stderr bytes.Buffer
		if err := NewTask(goos, goarch, append(args, cFile), &stdout, &stderr, nil).Main(); err != nil {
			t.Errorf("%q: %v", tc.flags, err)
			continue
		}

		out := stderr.String()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if out == "" {
			lines = nil
		}
		if g, e := len(lines), len(tc.want); g != e {
			t.Errorf("%q: got %d warnings, want %d\n%s", tc.flags, g, e, out)
			continue
		}

		for nm, tag := range tc.want {
			if !strings.Contains(out, "warning: "+nm+": ") {
				t.Errorf("%q: %s: no warning\n%s", tc.flags, nm, out)
				continue
			}

			for _, line := range lines {
				if strings.Contains(line, "warning: "+nm+": ") && !strings.HasSuffix(line, tag) {
					t.Errorf("%q: %s: want a warning ending in %s\n%s", tc.flags, nm, tag, out)
				}
			}
		}
	}
}
