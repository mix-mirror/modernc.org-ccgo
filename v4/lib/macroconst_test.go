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

// TestFloatMacroConstants checks that a macro whose value is an infinity or a
// NaN is not emitted as a Go constant, which would be `const mvX = +Inf`, an
// undefined identifier in Go, while a finite floating macro still is.
func TestFloatMacroConstants(t *testing.T) {
	const src = `
#define MYINF (1.0/0.0)
#define MYNAN (0.0/0.0)
#define MYHALF (1.0/2.0)

int main(void) { return 0; }
`
	dir := t.TempDir()
	cfn := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfn, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ofn := filepath.Join(dir, "main.o.go")
	var out bytes.Buffer
	if err := NewTask(goos, goarch, []string{"ccgo", "-c", "-eval-all-macros", "-o", ofn, cfn}, &out, &out, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, out.Bytes())
	}

	b, err := os.ReadFile(ofn)
	if err != nil {
		t.Fatal(err)
	}

	if !regexp.MustCompile(`(?m)^const mvMYHALF = 0\.5$`).Match(b) {
		t.Errorf("missing mvMYHALF constant")
	}
	if m := regexp.MustCompile(`(?m)^const mvMY(INF|NAN) = .*$`).FindAll(b, -1); len(m) != 0 {
		t.Errorf("non-finite macro constants: %q", m)
	}
}
