// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo

import (
	"strings"
	"testing"
)

// TestIssue47Regression verifies that ccgo correctly handles nested unions
// containing a pointer member, when the union is an array element and has a
// larger alternative member.
//
// Before fix d32226db, this scenario produced invalid Go code: "_ [-16]byte".
// See https://gitlab.com/cznic/ccgo/-/issues/47
func TestIssue47Regression(t *testing.T) {
	// The C source is cznic's minimal reproducer from the MR #26 review.
	cSrc := []byte(`
int g = 42;

typedef union {
	struct { unsigned char a; void *p; } act;  // 16 bytes on 64-bit
	struct { unsigned long x, y, z; } big;      // 24 bytes on 64-bit
} Inner;

typedef union {
	Inner action;                                // 24 bytes
	struct { unsigned char count; _Bool reusable; } entry;
} Entry;

Entry table[2] = {
	{ .action = { .act = { .a = 1, .p = &g } } },
	{ .action = { .act = { .a = 3, .p = &g } } },
};

#include <stdio.h>

int main() {
	printf("a0=%d a1=%d\n", table[0].action.act.a, table[1].action.act.a);
	if (table[0].action.act.a != 1) return 1;
	if (table[0].action.act.p != &g) return 2;
	if (table[1].action.act.a != 3) return 3;
	if (table[1].action.act.p != &g) return 4;
	printf("PASS\n");
	return 0;
}
`)

	if out := testHostCCvsCcgo(t, string(cSrc)); !strings.HasSuffix(out, "PASS") {
		t.Fatalf("unexpected output %q", out)
	}
}
