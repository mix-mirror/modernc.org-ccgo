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
	"testing"
)

// setjmpLowered reports whether ccgo lowers setjmp and longjmp for the target,
// which needs the musl headers of the Linux targets: MinGW makes setjmp a macro
// of compiler intrinsics, the other systems have headers of their own, and the
// jmp_buf of linux/ppc64le has an alignment ccgo does not support.
func setjmpLowered() bool {
	return goos == "linux" && goarch != "ppc64le"
}

// TestSetjmpNames checks the names the setjmp lowering declares and uses
// against C variables of the same name. They were kept verbatim: the recovered
// value x hid the parameter x in the handler, a return in the handler assigned
// the local r instead of the renamed result, the try closure's result caught
// took the assignments to the local caught, and a parameter tls was used as
// the TLS. The last two did not compile.
func TestSetjmpNames(t *testing.T) {
	if !setjmpLowered() {
		t.Skipf("ccgo does not lower setjmp on %s/%s", goos, goarch)
	}

	const src = `
#include <setjmp.h>
#include <stdio.h>

static jmp_buf jb;

void jump(int v) { if (v) longjmp(jb, v); }

int fx(int x) {
	if (setjmp(jb)) {
		if (x == 1) return 100;
		return 200;
	}
	jump(x);
	return 0;
}

int fr(int a) {
	int r = 5;
	if (setjmp(jb)) {
		return r + 1000;
	}
	r += a;
	jump(a);
	return r;
}

int ftls(int tls) {
	if (setjmp(jb)) {
		return tls + 10;
	}
	if (tls) longjmp(jb, 1);
	return tls;
}

int fcaught(int n) {
	int caught = 1;
	if (setjmp(jb) == 0) {
		caught = 5;
		jump(n);
	} else {
		caught += 100;
	}
	return caught;
}

int main(void) {
	printf("%d %d %d %d %d\n", fx(1), fr(1), ftls(3), fcaught(0), fcaught(1));
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

	cOut = bytes.TrimSpace(cOut)
	goOut = bytes.TrimSpace(goOut)
	if !bytes.Equal(cOut, goOut) {
		t.Fatalf("output mismatch\nC:  %s\nGo: %s", cOut, goOut)
	}
}
