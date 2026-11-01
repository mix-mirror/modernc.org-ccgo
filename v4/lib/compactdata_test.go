// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCompactData checks --compact-data: large const scalar arrays are
// emitted as a static [N]uint64 image reached through unsafe.Pointer, the
// program behaves the same as without it, and ineligible arrays keep their
// indexed literal. Element types cover signed and unsigned, negative values,
// an enum and an array size that is not a multiple of 8 bytes, so a wrong
// byte order in the packing changes the printed output.
//
// Three arrays stay literal, each for a different reason: small is below
// the threshold, mut is not const, and sloc is a static local, whose words
// variable would land in the function body while the array itself is
// declared at package level. The rows of the two-dimensional m2 and the
// array members v of the struct elements of sa are compacted one by one.
func TestCompactData(t *testing.T) {
	const src = `
#include <stdio.h>

enum color { RED = -3, GREEN = 7, BLUE = 11 };

const signed char   sc[13]  = {-1, 2, -3, 4, -5, 6, -7, 8, -9, 10, -11, 12, -13};
const short         sh[7]   = {-100, 200, -300, 400, -500, 600, -700};
const int           in[9]   = {-1, 2, -3, 4, -5, 6, -7, 8, -9};
const unsigned      un[11]  = {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11};
const long long     ll[5]   = {-1LL, 2LL, -3LL, 4LL, -5LL};
const float         fl[6]   = {1.5f, 2.5f, 3.5f, 4.5f, 5.5f, 6.5f};
const double        db[6]   = {1.25, 2.25, 3.25, 4.25, 5.25, 6.25};
const _Bool         bo[9]   = {1, 0, 1, 1, 0, 0, 1, 0, 1};
const enum color    co[5]   = {RED, GREEN, BLUE, RED, GREEN};

/* not compacted: below threshold */
const int small[3] = {1, 2, 3};

/* not compacted: not const */
int mut[13] = {-1, 2, -3, 4, -5, 6, -7, 8, -9, 10, -11, 12, -13};

/* compacted row by row and member by member */
const short m2[2][5] = {{-1, 2, -3, 4, -5}, {6, -7, 8, -9, 10}};
const struct { int k; short v[6]; } sa[2] = {{1, {-1, 2, -3, 4, -5, 6}}, {2, {7, -8, 9, -10, 11, -12}}};

int main(void) {
	static const short sloc[6] = {-6, -5, -4, -3, -2, -1};
	int i;
	for (i = 0; i < 13; i++) printf("%d ", sc[i]); printf("\n");
	for (i = 0; i < 7; i++)  printf("%d ", sh[i]); printf("\n");
	for (i = 0; i < 9; i++)  printf("%d ", in[i]); printf("\n");
	for (i = 0; i < 11; i++) printf("%u ", un[i]); printf("\n");
	for (i = 0; i < 5; i++)  printf("%lld ", ll[i]); printf("\n");
	for (i = 0; i < 6; i++)  printf("%.2f ", fl[i]); printf("\n");
	for (i = 0; i < 6; i++)  printf("%.2f ", db[i]); printf("\n");
	for (i = 0; i < 9; i++)  printf("%d ", bo[i]); printf("\n");
	for (i = 0; i < 5; i++)  printf("%d ", co[i]); printf("\n");
	for (i = 0; i < 3; i++)  printf("%d ", small[i]); printf("\n");
	for (i = 0; i < 13; i++) printf("%d ", mut[i]); printf("\n");
	for (i = 0; i < 6; i++)  printf("%d ", sloc[i]); printf("\n");
	for (i = 0; i < 10; i++) printf("%d ", m2[i / 5][i % 5]); printf("\n");
	for (i = 0; i < 12; i++) printf("%d ", sa[i / 6].v[i % 6]); printf("\n");
	printf("%d %d\n", sa[0].k, sa[1].k);
	return 0;
}
`

	want := strings.Join([]string{
		"-1 2 -3 4 -5 6 -7 8 -9 10 -11 12 -13 ",
		"-100 200 -300 400 -500 600 -700 ",
		"-1 2 -3 4 -5 6 -7 8 -9 ",
		"1 2 3 4 5 6 7 8 9 10 11 ",
		"-1 2 -3 4 -5 ",
		"1.50 2.50 3.50 4.50 5.50 6.50 ",
		"1.25 2.25 3.25 4.25 5.25 6.25 ",
		"1 0 1 1 0 0 1 0 1 ",
		"-3 7 11 -3 7 ",
		"1 2 3 ",
		"-1 2 -3 4 -5 6 -7 8 -9 10 -11 12 -13 ",
		"-6 -5 -4 -3 -2 -1 ",
		"-1 2 -3 4 -5 6 -7 8 -9 10 ",
		"-1 2 -3 4 -5 6 7 -8 9 -10 11 -12 ",
		"1 2",
	}, "\n") + "\n"

	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfn, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ofn := filepath.Join(dir, "main.go")
	var out strings.Builder
	// The threshold is small enough to compact every array above.
	args := []string{"ccgo", "--compact-data=4", "-o", ofn, cfn}
	if err := NewTask(goos, goarch, args, &out, &out, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	b, err := os.ReadFile(ofn)
	if err != nil {
		t.Fatal(err)
	}
	gen := string(b)

	// Every eligible array is initialized from a words variable of its own.
	const fromWords = `\*\(\*\[\d+\][^)]+\)\(unsafe\.Pointer\(&__ccgo_words_\d+\)\)`
	for _, name := range []string{"sc", "sh", "in", "un", "ll", "fl", "db", "bo", "co"} {
		if !regexp.MustCompile(`(?m)^var ` + name + ` = ` + fromWords + `$`).MatchString(gen) {
			t.Errorf("%s is not initialized from a words variable", name)
		}
	}
	if g, e := len(regexp.MustCompile(`(?m)^\s+[01]: `+fromWords+`,$`).FindAllString(gen, -1)), 2; g != e {
		t.Errorf("got %d rows of m2 initialized from a words variable, expected %d", g, e)
	}
	if g, e := len(regexp.MustCompile(`(?m)^\s+Fv: `+fromWords+`,$`).FindAllString(gen, -1)), 2; g != e {
		t.Errorf("got %d members sa[i].v initialized from a words variable, expected %d", g, e)
	}

	// small is below the threshold, mut is not const, sloc is a static
	// local: none of them may be compacted, they keep their literal form.
	for _, name := range []string{"small", "mut", "sloc"} {
		pat := fmt.Sprintf("var %s = [", name)
		if !strings.Contains(gen, pat) {
			t.Errorf("%s lost its literal form", name)
		}
		if strings.Contains(gen, fmt.Sprintf("var %s = *(*[", name)) {
			t.Errorf("%s was compacted but should not be", name)
		}
	}

	bin := filepath.Join(dir, "main"+enforceBinaryExt(""))
	if o, err := shell(false, "go", "build", "-o", bin, ofn); err != nil {
		t.Fatalf("%v\n%s", err, o)
	}
	got, err := shell(false, bin)
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if s := string(got); s != want {
		t.Fatalf("got\n%s\nwant\n%s", s, want)
	}
}
