// Copyright 2022 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// ~/src/modernc.org/ccorpus2/

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"sort"

	"modernc.org/cc/v4"
)

var (
	zeroFuncPtr = []byte(fmt.Sprintf("(%suintptr(0))", tag(preserve)))
)

type initPatch struct {
	d   *cc.Declarator
	off int64
	b   *buf
}

// initItem is a leaf of an initializer, ie. an assignment expression, and the
// offset of the subobject it initializes, relative to the object of the
// outermost initializer. The offset is in.Offset(), except for the copies of a
// leaf replicated over the elements of a [lo ... hi] range designator: cc
// reports the offset within the first element of the range and every copy is
// shifted to the element it initializes.
type initItem struct {
	in  *cc.Initializer
	off int64
}

func (c *ctx) initializerOuter(w writer, n *cc.Initializer, t cc.Type) (r *buf) {
	return c.initializer(w, n, c.initItems(n, t), t, 0, false)
}

// initItems returns the leaves of n, an initializer of an object of type t,
// in source order. A leaf designated for a range of array elements is
// replicated over the range. A leaf overridden by a later initializer is
// dropped, see initOverrides.
func (c *ctx) initItems(n *cc.Initializer, t cc.Type) []initItem {
	o := newInitOverrides()
	c.initItems0(n, t, []int64{0}, o)
	return o.items()
}

// initItemsList is initItems for a brace enclosed list that has no
// Initializer node of its own, ie. the list of a compound literal of type t.
func (c *ctx) initItemsList(l *cc.InitializerList, t cc.Type) []initItem {
	o := newInitOverrides()
	for ; l != nil; l = l.InitializerList {
		c.initItems0(l.Initializer, t, []int64{0}, o)
	}
	return o.items()
}

// initItems0 adds to o the leaves of n, an item of a brace enclosed list
// initializing an object of type t, shifted by every offset in shifts.
func (c *ctx) initItems0(n *cc.Initializer, t cc.Type, shifts []int64, o *initOverrides) {
	if ln := n.Len(); ln > 1 {
		// [lo ... hi]: n initializes ln consecutive elements of the array the
		// list initializes and so does every leaf of n. cc accepts a range only
		// in the outermost array of a brace enclosed list, so the stride is the
		// element size of t.
		x, ok := t.(*cc.ArrayType)
		if !ok {
			c.err(errorf("%v: TODO range designator in an initializer of %s", n.Position(), t))
			return
		}

		stride := x.Elem().Size()
		var a []int64
		for _, sh := range shifts {
			for i := int64(0); i < ln; i++ {
				a = append(a, sh+i*stride)
			}
		}
		shifts = a
	}
	switch n.Case {
	case cc.InitializerExpr: // AssignmentExpression
		for _, sh := range shifts {
			o.add(n, n.Offset()+sh, true)
		}
	case cc.InitializerInitList: // '{' InitializerList ',' '}'
		for _, sh := range shifts {
			o.add(n, n.Offset()+sh, false)
		}
		for l := n.InitializerList; l != nil; l = l.InitializerList {
			c.initItems0(l.Initializer, n.Type(), shifts, o)
		}
	default:
		c.err(errorf("internal error %T %v", n, n.Case))
	}
}

// initOverrides collects the leaves of an initializer and drops the ones a
// later initializer overrides.
//
// [0]6.7.9/19: The initialization shall occur in initializer list order, each
// initializer provided for a particular subobject overriding any previously
// listed initializer for the same subobject. Like gcc and clang, a brace
// enclosed initializer of an aggregate overrides the earlier initializers of
// all of its parts and an initializer of a union member overrides the earlier
// initializers of the other members: an initializer overrides every earlier
// leaf whose storage it overlaps. The exception is an expression initializing
// a whole aggregate, a string literal for instance, that cannot be split: a
// later initializer of a part of the aggregate keeps it.
type initOverrides struct {
	leaves []initItem // All of them, the overridden ones with in == nil.
	// Indices of the leaves, keyed by every 64 byte window of the initialized
	// object a leaf overlaps.
	windows map[int64][]int
}

const initWindowBits = 9 // 2^9 bits = 64 bytes.

func newInitOverrides() *initOverrides { return &initOverrides{windows: map[int64][]int{}} }

// initBits returns the range of bits [lo, hi) of the object the initializer
// in, shifted to off, initializes.
func initBits(in *cc.Initializer, off int64) (lo, hi int64) {
	lo = 8 * off
	if f := in.Field(); f != nil && f.IsBitfield() {
		lo += int64(f.OffsetBits())
		return lo, lo + int64(f.ValueBits())
	}

	if sz := in.Type().Size(); sz > 0 {
		return lo, lo + 8*sz
	}

	return lo, lo
}

// add drops the earlier leaves the initializer in, shifted to off, overrides
// and appends in when it's a leaf.
func (o *initOverrides) add(in *cc.Initializer, off int64, leaf bool) {
	lo, hi := initBits(in, off)
	if lo < hi {
		for w := lo >> initWindowBits; w <= (hi-1)>>initWindowBits; w++ {
			for _, i := range o.windows[w] {
				it := &o.leaves[i]
				if it.in == nil {
					continue
				}

				lo2, hi2 := initBits(it.in, it.off)
				if lo2 >= hi || hi2 <= lo {
					continue
				}

				if lo2 <= lo && hi <= hi2 && (lo2 < lo || hi < hi2) {
					switch it.in.Type().Kind() {
					case cc.Array, cc.Struct, cc.Union:
						continue
					}
				}

				it.in = nil
			}
		}
	}
	if !leaf {
		return
	}

	i := len(o.leaves)
	o.leaves = append(o.leaves, initItem{in, off})
	if lo < hi {
		for w := lo >> initWindowBits; w <= (hi-1)>>initWindowBits; w++ {
			o.windows[w] = append(o.windows[w], i)
		}
	}
}

// items returns the leaves not overridden, in source order.
func (o *initOverrides) items() (r []initItem) {
	for _, v := range o.leaves {
		if v.in != nil {
			r = append(r, v)
		}
	}
	return r
}

// sortInitItems groups the items by the subobject they initialize, as
// identified by group(offset), and orders the groups and the items within
// them by offset.
func sortInitItems(a []initItem, group func(int64) int64) (r [][]initItem) {
	// [0]6.7.8/23: The order in which any side effects occur among the
	// initialization list expressions is unspecified.
	m := map[int64][]initItem{}
	for _, v := range a {
		off := group(v.off)
		m[off] = append(m[off], v)
	}
	for _, v := range m {
		sort.Slice(v, func(i, j int) bool {
			a, b := v[i].off, v[j].off
			if a < b {
				return true
			}

			if a > b {
				return false
			}

			c, d := v[i].in.Field(), v[j].in.Field()
			if c == nil || d == nil {
				return false
			}

			return c.Index() < d.Index()
		})
		r = append(r, v)
	}
	sort.Slice(r, func(i, j int) bool { return r[i][0].off < r[j][0].off })
	return r
}

func (c *ctx) initializer(w writer, n cc.Node, a []initItem, t cc.Type, off0 int64, arrayElem bool) (r *buf) {
	if cc.IsScalarType(t) {
		if len(a) == 0 {
			c.err(errorf("TODO"))
			return nil
		}

		in := a[0]
		if in.off != off0 {
			c.err(errorf("TODO"))
			return nil
		}

		if t.Kind() == cc.Ptr && in.in.AssignmentExpression.Type().Undecay().Kind() == cc.Array {
			switch x := c.unparen(in.in.AssignmentExpression).(type) {
			case *cc.PostfixExpression:
				if x.Case != cc.PostfixExpressionComplit {
					break
				}

				t := in.in.AssignmentExpression.Type().Undecay().(*cc.ArrayType)
				r = c.topExpr(w, in.in.AssignmentExpression, t, exprDefault)
				switch {
				case c.initPatch != nil:
					nm := fmt.Sprintf("%s__ccgo_init_%d", tag(staticInternal), c.id())
					w.w("\nvar %s = %s;\n\n", nm, r)
					var b buf
					b.w("%suintptr(%s)", tag(preserve), unsafeAddr(nm))
					return &b
				default:
					return r
				}
			}
		}
		r = c.topExpr(w, in.in.AssignmentExpression, t, exprDefault)

		isFuncPtr := t.Kind() == cc.Ptr && t.(*cc.PointerType).Elem().Kind() == cc.Function || c.mentionsFunc(in.in.AssignmentExpression)
		isCyclic := c.declBeingInitialized != nil && c.mentionsDecl(in.in.AssignmentExpression, c.declBeingInitialized)

		if t.Kind() == cc.Ptr && c.initPatch != nil && (isFuncPtr || isCyclic) {
			c.initPatch(off0, r)
			var b buf
			b.w("%s", zeroFuncPtr)
			return &b
		}

		return r
	}

	switch x := t.(type) {
	case *cc.ArrayType:
		if len(a) == 1 && a[0].in.Type().Kind() == cc.Array && a[0].in.Value() != cc.Unknown {
			return c.expr(w, a[0].in.AssignmentExpression, t, exprDefault)
		}

		return c.initializerArray(w, n, a, x, off0)
	case *cc.StructType:
		if len(a) == 1 && a[0].in.Type().Kind() == cc.Struct && t.Size() == a[0].in.Type().Size() {
			return c.expr(w, a[0].in.AssignmentExpression, t, exprDefault)
		}

		return c.initializerStruct(w, n, a, x, off0)
	case *cc.UnionType:
		if len(a) == 1 && a[0].in.Type().Kind() == cc.Union && a[0].in.Type().Size() == x.Size() {
			r := c.expr(w, a[0].in.AssignmentExpression, t, exprDefault)
			r.n = a[0].in.AssignmentExpression
			return r
		}

		return c.initializerUnion(w, n, a, x, off0, arrayElem)
	default:
		c.err(errorf("TODO %T", x))
		return nil
	}
}

func (c *ctx) mentionsFunc(n cc.ExpressionNode) bool {
	if n == nil {
		return false
	}

	switch x := n.(type) {
	case *cc.ExpressionList:
		for ; x != nil; x = x.ExpressionList {
			if c.mentionsFunc(x.AssignmentExpression) {
				return true
			}
		}

		return false
	}

	if n.Type().Kind() == cc.Function || n.Type().Kind() == cc.Ptr && n.Type().(*cc.PointerType).Elem().Kind() == cc.Function {
		return true
	}

	t := reflect.TypeOf(n)
	v := reflect.ValueOf(n)
	var zero reflect.Value
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		v = v.Elem()
		if v == zero {
			return false
		}
	}

	if t.Kind() != reflect.Struct {
		return false
	}

	nf := t.NumField()
	for i := 0; i < nf; i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		if v == zero || v.IsZero() {
			continue
		}

		if m, ok := v.Field(i).Interface().(cc.ExpressionNode); ok && c.mentionsFunc(m) {
			return true
		}
	}
	return false
}

func (c *ctx) isZeroInitializerSlice(s []initItem) bool {
	for _, v := range s {
		if !c.isZero(v.in.AssignmentExpression.Value()) {
			return false
		}
	}

	return true
}

func (c *ctx) initializerArray(w writer, n cc.Node, a []initItem, t *cc.ArrayType, off0 int64) (r *buf) {
	var b buf
	b.w("%s{", c.typ(n, t))
	if c.isZeroInitializerSlice(a) {
		b.w("}")
		return &b
	}

	et := t.Elem()
	esz := et.Size()
	// A string literal initializing the whole array is kept by initOverrides
	// when later initializers designate elements of the array, because it
	// cannot be split. Split it here into its characters, the designated
	// elements override them, like in gcc. See issue #66.
	var chars []string
	for i, v := range a {
		if v.off == off0 && v.in.Type().Kind() == cc.Array && v.in.Type().Size() == t.Size() {
			if chars = c.stringChars(v.in.Value(), t); chars != nil {
				a = append(a[:i:i], a[i+1:]...)
				break
			}
		}
	}
	next := int64(0) // Index of the first character in chars not yet rendered.
	renderChars := func(upto int64) {
		for ; next < upto && next < int64(len(chars)); next++ {
			if chars[next] != "" {
				b.w("\n%d: %s, ", next, chars[next])
			}
		}
	}
	s := sortInitItems(a, func(n int64) int64 { n -= off0; return n - n%esz })
	for _, v := range s {
		off := v[0].off - off0
		off -= off % esz
		i := off / esz
		renderChars(i)
		next = i + 1 // Overridden.
		if !c.isZeroInitializerSlice(v) || !cc.IsArithmeticType(et) {
			if s := c.initializer(w, n, v, et, off0+off, true); !bytes.Equal(s.bytes(), zeroFuncPtr) {
				b.w("\n%d: %s, ", i, s)
			}
		}
	}
	renderChars(int64(len(chars)))
	b.w("\n}")
	return &b
}

// stringChars returns the characters of v, the value of a string literal
// initializing an array of type t, rendered as constants of the element type,
// the zero ones as "", or nil when v is not a string literal.
func (c *ctx) stringChars(v cc.Value, t *cc.ArrayType) (r []string) {
	var a []uint64
	switch x := v.(type) {
	case cc.StringValue:
		for _, ch := range []byte(x) {
			a = append(a, uint64(ch))
		}
	case cc.UTF16StringValue:
		for _, ch := range x {
			a = append(a, uint64(ch))
		}
	case cc.UTF32StringValue:
		for _, ch := range x {
			a = append(a, uint64(ch))
		}
	default:
		return nil
	}
	if max := t.Len(); max >= 0 && int64(len(a)) > max {
		a = a[:max]
	}
	r = make([]string, len(a))
	for i, ch := range a {
		if ch != 0 {
			r[i] = c.charConst(ch, t.Elem())
		}
	}
	return r
}

func (c *ctx) initializerStruct(w writer, n cc.Node, a []initItem, t *cc.StructType, off0 int64) (r *buf) {
	var b buf
	switch {
	case t.HasFlexibleArrayMember():
		b.w("%s{", c.initTyp(n, t))
	default:
		b.w("%s{", c.typ(n, t))
	}
	if c.isZeroInitializerSlice(a) {
		b.w("}")
		return &b
	}

	var flds []*cc.Field
	for i := 0; ; i++ {
		if f := t.FieldByIndex(i); f != nil {
			if f.Type().Size() <= 0 {
				switch x := f.Type().(type) {
				case *cc.StructType:
					if x.NumFields() != 0 {
						c.err(errorf("TODO %T", x))
						return nil
					}
				case *cc.UnionType:
					if x.NumFields() != 0 {
						c.err(errorf("TODO %T", x))
						return nil
					}
				case *cc.ArrayType:
					if x.Len() > 0 {
						c.err(errorf("TODO %T", x))
						return nil
					}
				default:
					c.err(errorf("TODO %T", x))
					return nil
				}
				continue
			}

			if f.IsBitfield() && f.ValueBits() == 0 {
				continue
			}

			flds = append(flds, f)
			continue
		}

		break
	}
	s := sortInitItems(a, func(off int64) int64 {
		off -= off0
		i := sort.Search(len(flds), func(i int) bool {
			return flds[i].OuterGroupOffset() >= off
		})
		if i < len(flds) && flds[i].OuterGroupOffset() == off {
			return off
		}

		return flds[i-1].OuterGroupOffset()
	})
	for _, v := range s {
		first := v[0]
		off := first.off - off0
		for off > flds[0].Offset()+flds[0].Type().Size()-1 {
			flds = flds[1:]
			if len(flds) == 0 {
				panic(todo("", n.Position()))
			}
		}
		f := flds[0]
		if f.IsBitfield() {
			for len(flds) != 0 && flds[0].OuterGroupOffset() == f.OuterGroupOffset() {
				flds = flds[1:]
			}
			b.w("\n%s__ccgo%d: ", tag(field), f.OuterGroupOffset())
			sort.Slice(v, func(i, j int) bool {
				a, b := v[i].in.Field(), v[j].in.Field()
				return a.Offset()*8+int64(a.OffsetBits()) < b.Offset()*8+int64(b.OffsetBits())
			})
			ogo := f.OuterGroupOffset()
			gsz := 8 * (int64(f.GroupSize()) + f.Offset() - ogo)
			for i, in := range v {
				if i != 0 {
					b.w("|")
				}
				f = in.in.Field()
				sh := f.OffsetBits() + 8*int(f.Offset()-ogo)
				b.w("(((%s)&%#0x)<<%d)", c.expr(w, in.in.AssignmentExpression, c.unsignedInts[gsz/8], exprDefault), uint64(1)<<f.ValueBits()-1, sh)
			}
			b.w(", ")
			continue
		}

		for isEmpty(v[0].in.Type()) {
			v = v[1:]
		}
		flds = flds[1:]
		if !c.isZeroInitializerSlice(v) {
			if s := c.initializer(w, n, v, f.Type(), off0+f.Offset(), false); !bytes.Equal(s.bytes(), zeroFuncPtr) {
				b.w("\n%s%s: %s, ", tag(field), c.fieldName(t, f), s)
			}
		}
	}
	b.w("\n}")
	return &b
}

func (c *ctx) initializerUnion(w writer, n cc.Node, a []initItem, t *cc.UnionType, off0 int64, arrayElem bool) (r *buf) {
	var b buf
	if c.isZeroInitializerSlice(a) {
		b.w("%s{}", c.typ(n, t))
		return &b
	}

	switch t.NumFields() {
	case 0:
		c.err(errorf("%v: cannot initialize empty union", n.Position()))
	case 1:
		// A single-field union is rendered as a Go struct with that one field, so
		// the initializer can name it directly. Not for a bit field though: the Go
		// field then covers the whole storage and the value must be masked and
		// shifted into its bit position within the access unit, which is what the
		// reinterpreted-struct form below does. Placing the value as-is happens to
		// work on little-endian targets, where a union bit field starts at bit 0,
		// but corrupts it on big-endian ones, where it starts at the most
		// significant bit of the access unit.
		if f := t.FieldByIndex(0); !f.IsBitfield() {
			b.w("%s{%s%s: %s}", c.typ(n, t), tag(field), c.fieldName(t, f), c.initializer(w, n, a, f.Type(), off0, false))
			return &b
		}
	}

	if r := c.compactUnionInit(n, a, t, off0); r != nil {
		return r
	}

	switch len(a) {
	case 1:
		b.w("(*(*%s)(%sunsafe.%sPointer(&struct{ ", c.typ(n, t), tag(importQualifier), tag(preserve))
		b.w("%s", c.initializerUnionOne(w, n, a, t, off0))
		b.w(")))")
	default:
		b.w("(*(*%s)(%sunsafe.%sPointer(&", c.typ(n, t), tag(importQualifier), tag(preserve))
		b.w("%s", c.initializerUnionMany(w, n, a, t, off0, arrayElem))
		b.w(")))")
	}
	return &b
}

func (c *ctx) initializerUnionMany(w writer, n cc.Node, a []initItem, t *cc.UnionType, off0 int64, arrayElem bool) (r *buf) {
	var b buf
	var paths [][]*cc.Initializer
	for _, v := range a {
		var path []*cc.Initializer
		for p := v.in.Parent(); p != nil; p = p.Parent() {
			path = append(path, p)
		}
		paths = append(paths, path)
	}
	var lca *cc.Initializer
	for {
		var path *cc.Initializer
		for i, v := range paths {
			if len(v) == 0 {
				goto done
			}

			w := v[len(v)-1]
			if i == 0 {
				path = w
				continue
			}

			if w != path {
				goto done
			}
		}
		lca = path
		if lca.Type() == t {
			goto done
		}

		for i, v := range paths {
			paths[i] = v[:len(v)-1]
		}
	}
done:
	if lca == nil {
		w.w("panic(`TODO %v: (%v:)`);", pos(n), origin(1))
		b.w("(%s{})", c.typ(n, t))
		return &b
	}

	lcaType, lcaOff := c.fixLCA(t, lca, a, off0)
	if lcaType == nil {
		w.w("panic(`TODO %v: (%v:)`);", pos(n), origin(1))
		b.w("(%s{})", c.typ(n, t))
		return &b
	}

	if lcaType.Size() == t.Size() {
		return c.initializer(w, n, a, lcaType, off0, false)
	}

	// fixLCA returns lcaOff as the offset of the active member relative to the
	// top-level initialized object; off0 is the same-frame offset of the union
	// t. The padding before the active member, expressed within t, is therefore
	// lcaOff-off0. See issue #47.
	pre := lcaOff - off0
	post := t.Size() - lcaType.Size() - pre
	b.w("struct{")
	if lcaOff != 0 {
		b.w("%s_ [%d]byte;", tag(preserve), pre)
	}
	b.w("%sf ", tag(preserve))
	b.w("%s ", c.typ(n, lcaType))
	if post != 0 {
		b.w("; %s_ [%d]byte", tag(preserve), post)
	}
	b.w("}{%sf: ", tag(preserve))
	b.w("%s", c.initializer(w, n, a, lcaType, off0, false))
	b.w("}")
	return &b
}

func (c *ctx) fixLCA(t *cc.UnionType, lca *cc.Initializer, a []initItem, off0 int64) (rt cc.Type, off int64) {
	rt = lca.Type()
	// The items may be copies shifted from the first element of a range
	// designator, lca is shifted the same.
	lcaOff := lca.Offset() + a[0].off - a[0].in.Offset()
	switch {
	case rt.Size() > t.Size():
		return rt, lcaOff
	case rt != t:
		return rt, lcaOff
	}

	okField, okName := true, true
	for _, v := range a {
		if v.in.Field() == nil {
			okField = false
			okName = false
			break
		}

		if v.in.Field().Name() == "" {
			okName = false
			break
		}
	}

	if okField && okName {
	nextUf:
		for i := 0; i < t.NumFields(); i++ {
			uf := t.FieldByIndex(i)
		ok:
			for _, v := range a {
				af := v.in.Field()
				fs := c.findFields(uf.Type(), af.Name(), 0)
				if len(fs) == 0 {
					continue nextUf
				}

				for _, f := range fs {
					if v.off-off0 != f.off {
						continue
					}

					if v.in.Type().Size() != f.f.Type().Size() {
						continue
					}

					continue ok
				}

				continue nextUf
			}
			return uf.Type(), lcaOff + uf.Offset()
		}
	}

	f := t.FieldByIndex(0)
	return f.Type(), f.Offset()
}

type fld struct {
	f   *cc.Field
	off int64
}

func (c *ctx) findFields(t cc.Type, fn string, off int64) (r []fld) {
	x, ok := t.(interface {
		FieldByIndex(int) *cc.Field
		NumFields() int
	})
	if !ok {
		return nil
	}

	for i := 0; i < x.NumFields(); i++ {
		f := x.FieldByIndex(i)
		if f.Name() == fn {
			r = append(r, fld{f: f, off: off + f.Offset()})
		}

		r = append(r, c.findFields(f.Type(), fn, f.Offset())...)
	}
	return r
}

func (c *ctx) initializerUnionOne(w writer, n cc.Node, a []initItem, t *cc.UnionType, off0 int64) (r *buf) {
	var b buf
	in := a[0]
	pre := in.off - off0
	if pre != 0 {
		b.w("%s_ [%d]byte;", tag(preserve), pre)
	}
	b.w("%sf ", tag(preserve))
	f := in.in.Field()
	// Size of the emitted f. A bit field is accessed through its access unit,
	// which can be narrower than the declared type, so the padding below must
	// account for the emitted width, not for in.in.Type().Size(), or the struct
	// comes out shorter than the union it is reinterpreted as.
	fsize := in.in.Type().Size()
	switch {
	case f != nil && f.IsBitfield():
		fsize = f.AccessBytes()
		b.w("%suint%d", tag(preserve), fsize*8)
	default:
		b.w("%s ", c.typ(n, in.in.Type()))
	}
	if post := t.Size() - (pre + fsize); post != 0 {
		b.w("; %s_ [%d]byte", tag(preserve), post)
	}
	b.w("}{%sf: ", tag(preserve))
	switch f := in.in.Field(); {
	case f != nil && f.IsBitfield():
		b.w("(((%s)&%#0x)<<%d)", c.expr(w, in.in.AssignmentExpression, c.unsignedInts[f.AccessBytes()], exprDefault), uint64(1)<<f.ValueBits()-1, f.OffsetBits())
	default:
		b.w("%s", c.expr(w, in.in.AssignmentExpression, in.in.Type(), exprDefault))
	}
	b.w("}")
	return &b
}

// compactUnionInit renders a non-zero, multi-field union initializer as a
// compact reinterpreted word-array literal
//
//	(*(*T)(unsafe.Pointer(&[K]uintW{...})))
//
// instead of the verbose per-element inline-struct form produced by
// initializerUnionOne / initializerUnionMany (which costs ~15 gofmt'd lines per
// element for aggregate-active-member unions, see issue #46). It succeeds only
// when every leaf initializer is a compile-time-constant, non-pointer,
// non-bitfield scalar, i.e. pure data with no relocations. For anything else
// (function pointers, address constants, self-referencing/cyclic initializers,
// bitfields, strings, complex, long double, ...) it returns nil and the caller
// falls back to the verbose form, which keeps the existing initPatch deferral
// working.
//
// The word width W equals the union's alignment (1, 2, 4 or 8), so the backing
// literal is at least as aligned as the union itself. Each word is decoded from
// the raw image using the target byte order, so the in-memory bytes on the
// target reproduce the image regardless of endianness; the emitted literals
// differ between little- and big-endian targets, as ccgo already emits
// per-target output.
func (c *ctx) compactUnionInit(n cc.Node, a []initItem, t *cc.UnionType, off0 int64) *buf {
	size := t.Size()
	al := int64(t.Align())
	if size <= 0 || al <= 0 || al > 8 || size%al != 0 {
		return nil
	}

	bo := c.ast.ABI.ByteOrder
	img := make([]byte, size)
	for _, in := range a {
		if f := in.in.Field(); f != nil && f.IsBitfield() {
			return nil
		}

		ft := in.in.Type()
		fsz := ft.Size()
		rel := in.off - off0
		if fsz <= 0 || rel < 0 || rel+fsz > size || in.in.AssignmentExpression == nil {
			return nil
		}

		if !encodeScalarConst(img[rel:rel+fsz], in.in.AssignmentExpression.Value(), ft, bo) {
			return nil
		}
	}

	var b buf
	b.w("(*(*%s)(%sunsafe.%sPointer(&", c.typ(n, t), tag(importQualifier), tag(preserve))
	w := int(al)
	b.w("[%d]%suint%d{", size/al, tag(preserve), w*8)
	for i := 0; i < len(img); i += w {
		if i != 0 {
			b.w(", ")
		}

		var v uint64
		switch w {
		case 1:
			v = uint64(img[i])
		case 2:
			v = uint64(bo.Uint16(img[i:]))
		case 4:
			v = uint64(bo.Uint32(img[i:]))
		case 8:
			v = bo.Uint64(img[i:])
		}
		b.w("%#x", v)
	}
	b.w("})))")
	return &b
}

// encodeScalarConst writes the target-endian byte representation of the
// compile-time-constant scalar value v (of C type t) into dst, which must be
// exactly t.Size() bytes long. It returns false for anything that cannot be
// rendered as a fixed byte pattern: pointers/relocations, strings, complex,
// long double, __int128, _Float16/_Float128 or any non-constant (unknown)
// value.
func encodeScalarConst(dst []byte, v cc.Value, t cc.Type, bo binary.ByteOrder) bool {
	switch t.Kind() {
	case cc.Bool:
		u, ok := scalarUint64(v)
		if !ok {
			return false
		}

		if u != 0 {
			u = 1 // _Bool normalizes any nonzero value to 1.
		}

		putUintBytes(dst, u, bo)
		return true
	case
		cc.Char, cc.SChar, cc.UChar, cc.Short, cc.UShort,
		cc.Int, cc.UInt, cc.Long, cc.ULong, cc.LongLong, cc.ULongLong, cc.Enum:

		u, ok := scalarUint64(v)
		if !ok {
			return false
		}

		putUintBytes(dst, u, bo)
		return true
	case cc.Float, cc.Float32:
		f, ok := scalarFloat64(v)
		if !ok || len(dst) != 4 {
			return false
		}

		putUintBytes(dst, uint64(math.Float32bits(float32(f))), bo)
		return true
	case cc.Double, cc.Float64:
		f, ok := scalarFloat64(v)
		if !ok || len(dst) != 8 {
			return false
		}

		putUintBytes(dst, math.Float64bits(f), bo)
		return true
	default:
		return false
	}
}

// scalarUint64 returns the integer value of a constant integer/enum/bool value.
func scalarUint64(v cc.Value) (uint64, bool) {
	switch x := v.(type) {
	case cc.Int64Value:
		return uint64(x), true
	case cc.UInt64Value:
		return uint64(x), true
	case *cc.ZeroValue:
		return 0, true
	default:
		return 0, false
	}
}

// scalarFloat64 returns the value of a constant float/integer value as float64.
func scalarFloat64(v cc.Value) (float64, bool) {
	switch x := v.(type) {
	case cc.Float64Value:
		return float64(x), true
	case cc.Int64Value:
		return float64(x), true
	case cc.UInt64Value:
		return float64(x), true
	case *cc.ZeroValue:
		return 0, true
	default:
		return 0, false
	}
}

// putUintBytes stores the low len(dst) bytes of u into dst using byte order bo.
func putUintBytes(dst []byte, u uint64, bo binary.ByteOrder) {
	le := bo == binary.LittleEndian
	n := len(dst)
	for i := 0; i < n; i++ {
		b := byte(u >> (8 * i))
		if le {
			dst[i] = b
		} else {
			dst[n-1-i] = b
		}
	}
}

func (c *ctx) mentionsDecl(n cc.ExpressionNode, d *cc.Declarator) bool {
	if n == nil || d == nil {
		return false
	}

	target := d.Name()
	var found bool

	walkC(n, func(node cc.Node, mode int) {
		if found || mode != walkPre {
			return
		}

		// If the source code of this specific AST node exactly matches our identifier,
		// we assume it's a cyclic reference.
		if cc.NodeSource(node) == target {
			found = true
		}
	})

	return found
}
