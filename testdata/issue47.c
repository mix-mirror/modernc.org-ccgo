// Issue #47 regression: nested union with pointer member.
//
// ccgo must correctly handle array-element unions whose active member is a
// nested struct containing a pointer, when the union has a larger alternative
// member.  Without the fix in d32226db (pre := lcaOff - off0, removing
// arrayElemOff), this produced invalid Go: "_ [-16]byte".
//
// See https://gitlab.com/cznic/ccgo/-/issues/47

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

int main() {
	if (table[0].action.act.a != 1)
		return 1;
	if (table[0].action.act.p != &g)
		return 2;
	if (table[1].action.act.a != 3)
		return 3;
	if (table[1].action.act.p != &g)
		return 4;
	return 0;
}
