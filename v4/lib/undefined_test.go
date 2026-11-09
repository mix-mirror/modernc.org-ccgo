// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUndefinedNamePrefix checks the names of undefined external functions
// linked with -ignore-link-errors. A name that starts with one of the tags,
// like te in temu_hook, lost that prefix, and --prefix-undefined was applied
// twice, unless it was "_", itself a tag.
func TestUndefinedNamePrefix(t *testing.T) {
	const src = `
int temu_hook(void);
int _priv(void);
int Xmas(void);
int host_hook(void);

int main(void) { return temu_hook() + _priv() + Xmas() + host_hook(); }
`
	names := []string{"temu_hook", "_priv", "Xmas", "host_hook"}
	dir := t.TempDir()
	cFile := filepath.Join(dir, "test.c")
	if err := os.WriteFile(cFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	oFile := filepath.Join(dir, "test.o.go")
	var stdout, stderr bytes.Buffer
	if err := NewTask(goos, goarch, []string{"ccgo", "-c", "-o", oFile, cFile}, &stdout, &stderr, nil).Main(); err != nil {
		t.Fatalf("%v\n%s", err, stderr.Bytes())
	}

	for _, prefix := range []string{"", "U", "_"} {
		goFile := filepath.Join(dir, "test.go")
		args := []string{"ccgo", "-ignore-link-errors", "-o", goFile}
		if prefix != "" {
			args = append(args, "--prefix-undefined="+prefix)
		}
		stdout.Reset()
		stderr.Reset()
		if err := NewTask(goos, goarch, append(args, oFile), &stdout, &stderr, nil).Main(); err != nil {
			t.Errorf("prefix %q: %v\n%s", prefix, err, stderr.Bytes())
			continue
		}

		b, err := os.ReadFile(goFile)
		if err != nil {
			t.Fatal(err)
		}

		var line string
		for _, v := range strings.Split(string(b), "\n") {
			if strings.Contains(v, "return ") && strings.Contains(v, "hook(tls)") {
				line = v
				break
			}
		}
		if line == "" {
			t.Errorf("prefix %q: no call to the hooks in\n%s", prefix, b)
			continue
		}

		for _, nm := range names {
			// Each call is preceded by a space, an operator or a paren.
			want := prefix + nm + "(tls)"
			found := false
			for _, sep := range []string{" ", "(", "+"} {
				if strings.Contains(line, sep+want) {
					found = true
				}
			}
			if !found {
				t.Errorf("prefix %q: want a call to %s in %q", prefix, want, line)
			}
		}
	}
}
