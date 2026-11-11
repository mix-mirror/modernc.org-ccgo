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

// TestSingleExit checks the functions whose returns become jumps to a single
// return, see singleExit: the Go compiler open-codes the defer freeing their TLS
// frame only then. many and manyVoid have too many returns for that, few has
// few. sink stores its argument, so v is in the TLS frame. The longjmp unwinds
// many, whose deferred free must still run, which the address of a local in a
// frame allocated before and after shows. The first printf grows the TLS stack,
// p0 is taken after it.
func TestSingleExit(t *testing.T) {
	const src = `
#include <setjmp.h>
#include <stdint.h>
#include <stdio.h>

static jmp_buf jb;

int *gp;

void sink(int *p) { gp = p; *p += 1; }

uintptr_t probe(void) { int x = 0; sink(&x); return (uintptr_t)&x; }

#define R(n) if (x == n) return v + n;

int many(int x) {
	int v = 100;
	sink(&v);
	R(0) R(1) R(2) R(3) R(4) R(5) R(6) R(7) R(8) R(9)
	R(10) R(11) R(12) R(13) R(14) R(15) R(16)
	if (x == 99) {
		longjmp(jb, 1);
	}
	return -v;
}

int nvoid;

#define V(n) if (x == n) { nvoid += v + n; return; }

void manyVoid(int x) {
	int v = 10;
	sink(&v);
	V(0) V(1) V(2) V(3) V(4) V(5) V(6) V(7) V(8) V(9)
	V(10) V(11) V(12) V(13) V(14) V(15) V(16)
	nvoid -= v;
}

int few(int x) {
	int v = 1;
	sink(&v);
	if (x) {
		return v;
	}

	return -v;
}

int main(void) {
	printf("start\n");
	uintptr_t p0 = probe();
	int s = 0;
	for (int i = 0; i < 20; i++) {
		s += many(i);
		manyVoid(i);
	}
	printf("%d %d %d %d\n", s, nvoid, few(0), few(1));
	if (setjmp(jb) == 0) {
		many(99);
		printf("not reached\n");
	} else {
		printf("longjmp\n");
	}
	printf("balanced %d\n", probe() == p0);
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
		fn            string
		returns, gtos int
	}{
		{"many", 1, 18},
		{"manyVoid", 1, 17},
		{"few", 2, 0},
	} {
		body := string(b)
		i := strings.Index(body, "\nfunc "+v.fn+"(")
		if i < 0 {
			t.Errorf("%s not found\n%s", v.fn, b)
			continue
		}

		body = body[i:]
		body = body[:strings.Index(body, "\n}\n")]
		if g, e := strings.Count(body, "return"), v.returns; g != e {
			t.Errorf("%s: got %d returns, want %d\n%s", v.fn, g, e, body)
		}
		if g, e := strings.Count(body, "goto "), v.gtos; g != e {
			t.Errorf("%s: got %d gotos, want %d\n%s", v.fn, g, e, body)
		}
	}

	goBin := filepath.Join(dir, enforceBinaryExt("gobin"))
	var build []byte
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

		// -d=defer reports how the compiler implements each defer.
		var err error
		if build, err = exec.Command("go", "build", "-gcflags=-d=defer", "-o", goBin, goFile).CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, build)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	n := 0
	for _, v := range strings.Split(string(build), "\n") {
		if !strings.Contains(v, " defer") {
			continue
		}

		n++
		if !strings.HasSuffix(v, ": open-coded defer") {
			t.Errorf("want only open-coded defers: %s", v)
		}
	}
	if n == 0 {
		t.Errorf("no defers reported by go build -gcflags=-d=defer\n%s", build)
	}

	goOut, err := exec.Command(goBin).Output()
	if err != nil {
		t.Fatalf("Go binary failed: %v", err)
	}

	cOut = bytes.ReplaceAll(bytes.TrimSpace(cOut), []byte("\r"), nil)
	goOut = bytes.ReplaceAll(bytes.TrimSpace(goOut), []byte("\r"), nil)
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:  %s\nGo: %s", cOut, goOut)
	}
}
