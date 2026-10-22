// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestInlinePrototype checks that a call of a static inline header function
// is inlined when the call resolves to a prototype of the function instead
// of its definition, whatever the order of the prototype, the caller and the
// definition, see !35.
func TestInlinePrototype(t *testing.T) {
	for _, v := range []struct {
		name  string
		files map[string]string
		out   string
	}{
		{
			// The prototype precedes an inline caller, the definition follows.
			name: "proto-caller-def",
			files: map[string]string{
				"h.h": `
static inline int f(int);
static inline int g(void) { return f(1); }
static inline int f(int x) { return x + 1; }
`,
				"main.c": `
#include <stdio.h>
#include "h.h"
int main(void) { printf("%d\n", g()); return 0; }
`,
			},
			out: "2\n",
		},
		{
			// A plain function calls f before its definition is seen.
			name: "proto-plaincaller-def",
			files: map[string]string{
				"h1.h": `
static inline int f(int);
static inline int g(void) { return f(1); }
`,
				"h2.h": `
static inline int f(int x) { return x + 1; }
`,
				"main.c": `
#include <stdio.h>
#include "h1.h"
int h(void) { return f(2); }
#include "h2.h"
int main(void) { printf("%d %d\n", g(), h()); return 0; }
`,
			},
			out: "2 3\n",
		},
		{
			// The definition precedes a redundant prototype.
			name: "def-proto",
			files: map[string]string{
				"h.h": `
static inline int f(int x) { return x + 1; }
static inline int f(int);
`,
				"main.c": `
#include <stdio.h>
#include "h.h"
int main(void) { printf("%d\n", f(1)); return 0; }
`,
			},
			out: "2\n",
		},
		{
			// A prototype in a header, the definition in the .c file: not
			// inlined, the function is emitted and called.
			name: "proto-def-in-c",
			files: map[string]string{
				"h.h": `
static inline int f(int);
static inline int g(void) { return f(1); }
`,
				"main.c": `
#include <stdio.h>
#include "h.h"
static inline int f(int x) { return x + 1; }
int main(void) { printf("%d\n", g()); return 0; }
`,
			},
			out: "2\n",
		},
	} {
		t.Run(v.name, func(t *testing.T) {
			dir := t.TempDir()
			for fn, src := range v.files {
				if err := os.WriteFile(filepath.Join(dir, fn), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			ofn := filepath.Join(dir, "main.go")
			var out bytes.Buffer
			if err := NewTask(goos, goarch, []string{"ccgo", "-o", ofn, filepath.Join(dir, "main.c")}, &out, &out, nil).Main(); err != nil {
				t.Fatalf("%v\n%s", err, out.Bytes())
			}

			b, err := os.ReadFile(ofn)
			if err != nil {
				t.Fatal(err)
			}

			if g, e := regexp.MustCompile(`(?m)^func f\(`).Match(b), v.name == "proto-def-in-c"; g != e {
				t.Errorf("f emitted: %v, expected %v", g, e)
			}

			bin := filepath.Join(dir, "main"+enforceBinaryExt(""))
			if shOut, err := shell(false, "go", "build", "-o", bin, ofn); err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			shOut, err := shell(false, bin)
			if err != nil {
				t.Fatalf("%s\n%v", shOut, err)
			}

			if g, e := string(shOut), v.out; g != e {
				t.Errorf("got %q, expected %q", g, e)
			}
		})
	}
}
