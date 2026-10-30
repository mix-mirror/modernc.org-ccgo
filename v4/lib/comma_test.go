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

// TestCommaConditionSideEffects checks that a condition whose value is a
// constant is still evaluated when it is a comma expression with a side effect
// after a pure first operand, such as ((x, ++g), 1). cc before v4.30.5
// reported such an expression as pure, and the conditional expression was
// folded to one of its branches without the side effect, also with
// --goptr=off. Found by TestCSmith, seed 8898061851462809438.
func TestCommaConditionSideEffects(t *testing.T) {
	const src = `
#include <stdio.h>

static int g;
struct S { int a, b; };
static struct S mk(int v) { struct S s = {v, v + 1}; return s; }

int main(void) {
	int x = 0, r = 0;

	r = ((x, ++g), 1) ? 5 : 6; printf("cond %d %d\n", r, g);
	r = (((x, ++g), 0) ? mk(1) : mk(2)).b; printf("member %d %d\n", r, g);
	if ((x, ++g), 0) r = 7; printf("if %d %d\n", r, g);
	while ((x, ++g), 0) r = 8; printf("while %d %d\n", r, g);
	return 0;
}
`
	// gcc -O0
	const expected = `cond 5 1
member 3 2
if 3 3
while 3 4
`
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfn, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"on", "off"} {
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
