// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"regexp"
	"sync"
)

// intrinsicBuiltins are the builtins ccgo translates itself instead of calling
// a function of that name; see the cases in (*ctx).postfixExpression and its
// helpers. TestIntrinsicBuiltins keeps the list in step with the code.
var intrinsicBuiltins = map[string]struct{}{
	"__atomic_compare_exchange":            {},
	"__atomic_exchange":                    {},
	"__atomic_fetch_add":                   {},
	"__atomic_fetch_and":                   {},
	"__atomic_fetch_nand":                  {},
	"__atomic_fetch_or":                    {},
	"__atomic_fetch_sub":                   {},
	"__atomic_fetch_xor":                   {},
	"__atomic_load":                        {},
	"__atomic_load_n":                      {},
	"__atomic_store":                       {},
	"__atomic_store_n":                     {},
	"__builtin_add_overflow":               {},
	"__builtin_alloca":                     {},
	"__builtin_choose_expr":                {},
	"__builtin_constant_p":                 {},
	"__builtin_mul_overflow":               {},
	"__builtin_object_size":                {},
	"__builtin_popcount":                   {},
	"__builtin_popcountl":                  {},
	"__builtin_popcountll":                 {},
	"__builtin_sub_overflow":               {},
	"__builtin_va_end":                     {},
	"__builtin_va_start":                   {},
	"__c11_atomic_compare_exchange_strong": {},
	"__c11_atomic_exchange":                {},
	"__c11_atomic_fetch_add":               {},
	"__c11_atomic_fetch_and":               {},
	"__c11_atomic_fetch_nand":              {},
	"__c11_atomic_fetch_or":                {},
	"__c11_atomic_fetch_sub":               {},
	"__c11_atomic_fetch_xor":               {},
	"__c11_atomic_load":                    {},
	"__c11_atomic_load_n":                  {},
	"__c11_atomic_store":                   {},
	"__c11_atomic_store_n":                 {},
	"__sync_add_and_fetch":                 {},
	"__sync_sub_and_fetch":                 {},
	"__sync_val_compare_and_swap":          {},
}

// reBuiltinName matches the names __has_builtin is asked about.
var reBuiltinName = regexp.MustCompile(`\b(__builtin_[A-Za-z0-9_]+|__sync_[A-Za-z0-9_]+|__atomic_[A-Za-z0-9_]+|__c11_atomic_[A-Za-z0-9_]+)\b`)

// preambleBuiltins are the builtin names patchedBuiltin, the <builtin> source
// every translation unit starts with, declares or defines as macros.
var preambleBuiltins = sync.OnceValue(func() map[string]struct{} {
	r := map[string]struct{}{}
	for _, nm := range reBuiltinName.FindAllString(patchedBuiltin, -1) {
		r[nm] = struct{}{}
	}
	return r
})

// hasBuiltin implements cc.Config.HasBuiltin, the __has_builtin operator: it
// reports whether a call to the builtin nm translates. It does when ccgo
// translates it itself, when the builtin preamble declares it, or, when
// linking against libc, when libc exports X<nm>.
//
// Without it cc reports every __builtin_ name as present, so C code that
// prefers a builtin it can probe for and falls back to plain C otherwise
// takes the builtin branch even for builtins nothing here implements, and
// translates to a call to an undefined function. WABT 1.0.42's wasm2c output
// is the case that showed it: wasm_sqrt prefers clang's
// __builtin_elementwise_sqrt.
func (t *Task) hasBuiltin(nm string) bool {
	if _, ok := intrinsicBuiltins[nm]; ok {
		return true
	}

	if _, ok := preambleBuiltins()[nm]; ok {
		return true
	}

	if t.nostdlib || t.freeStanding || t.libc == "" {
		return false
	}

	_, ok := t.libcExports()["X"+nm]
	return ok
}

// libcExports returns the names the libc package exports for the target,
// loading them the first time __has_builtin needs them. The linker loads the
// same package again later, so a translation unit that never asks pays
// nothing. A libc that does not load yields no names: then only the builtins
// above are reported, and the link, which needs libc too, reports the error.
func (t *Task) libcExports() map[string]struct{} {
	t.libcExportsOnce.Do(func() {
		if obj, err := t.getPkgSymbols(t.libc); err == nil {
			t.libcExportsSet = obj.externs
		}
	})
	return t.libcExportsSet
}
