// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"fmt"

	"modernc.org/cc/v4"
)

// ccgo lays out structs and unions as cc/v4 does without Config.ABILayout,
// ignoring __attribute__((packed)) and #pragma pack: a Go struct cannot have
// a misaligned field. Bit-fields are laid out so that they never share bytes
// with other members. Code compiled by ccgo agrees with itself, but not
// always with the target's C compiler, so for example a packed network
// header, file format or kernel structure laid over bytes is read wrong.
//
// checkLayout warns about that, once per translation unit and type, where
// ccgo uses such a type: where it renders the type outside of a type
// definition, where an expression has the type or a pointer to it, and where
// sizeof or _Alignof takes it. Defining or declaring the type, for example by
// including a header, does not warn.
//
// By default it warns where packing changes the size, alignment or a member
// offset of the type. --no-packed-warning silences these warnings,
// --no-packed-warning=<names> those for the listed tags or typedef names.
//
// --warn-abi-layout also warns where the size or alignment of the type
// differs from the C compiler's for any other reason, bit-fields for example,
// and where a packed warning is silenced. Offsets that differ while the size
// and alignment agree are not reported.

// checkLayout warns when ccgo uses the layout of t, a struct or union or an
// array of them, and it differs from the layout in the target's C compiler,
// see above. deref also looks through one level of pointer, for the type of
// an expression.
func (c *ctx) checkLayout(n cc.Node, t cc.Type, deref bool) {
	if t == nil || c.task.noPackedWarning && !c.task.warnABILayout {
		return
	}

	if p, ok := t.(*cc.PointerType); ok && deref {
		t = p.Elem()
	}
	for {
		a, ok := t.(*cc.ArrayType)
		if !ok {
			break
		}

		t = a.Elem()
	}
	var size int64
	var align int
	var changed bool
	var kind, tagName string
	var f fielder
	switch x := t.(type) {
	case *cc.StructType:
		size, align, changed = x.PackedABILayout()
		tok := x.Tag()
		kind, tagName, f = "struct", tok.SrcStr(), x
	case *cc.UnionType:
		size, align, changed = x.PackedABILayout()
		tok := x.Tag()
		kind, tagName, f = "union", tok.SrcStr(), x
	default:
		return
	}

	differs := size != t.Size() || align != t.Align()
	if !changed && !differs {
		return
	}

	var typedefName string
	if d := t.Typedef(); d != nil {
		typedefName = d.Name()
	}
	packed := changed && !c.task.noPackedWarning && !c.task.noPackedWarningNames.has(tagName) && !c.task.noPackedWarningNames.has(typedefName)
	abi := differs && c.task.warnABILayout
	if !packed && !abi {
		return
	}

	// A type has copies, the fields of the copies are copies, but their
	// declarators are shared.
	var key any = t
	for i := 0; i < f.NumFields(); i++ {
		if d := f.FieldByIndex(i).Declarator(); d != nil {
			key = d
			break
		}
	}
	if _, ok := c.layoutWarned[key]; ok {
		return
	}

	if c.layoutWarned == nil {
		c.layoutWarned = map[any]struct{}{}
	}
	c.layoutWarned[key] = struct{}{}
	var nm string
	switch {
	case tagName != "":
		nm = kind + " " + tagName
	case typedefName != "":
		nm = typedefName
	default:
		nm = "anonymous " + kind
	}
	p := "-"
	if n != nil {
		p = n.Position().String()
	}
	layout := "member offsets differ"
	if differs {
		layout = fmt.Sprintf("ccgo: size %d, align %d; C: size %d, align %d", t.Size(), t.Align(), size, align)
	}
	switch {
	case packed:
		c.task.warn("%s: warning: %s: ccgo ignores its packing, the target's C compiler lays it out differently (%s) [--no-packed-warning]", p, nm, layout)
	default:
		c.task.warn("%s: warning: %s: the target's C compiler lays it out differently (%s) [--warn-abi-layout]", p, nm, layout)
	}
}

// warn writes a warning to the task's stderr. Translation units compile
// concurrently.
func (t *Task) warn(s string, args ...any) {
	t.stderrMu.Lock()

	defer t.stderrMu.Unlock()

	fmt.Fprintf(t.stderr, s+"\n", args...)
}
