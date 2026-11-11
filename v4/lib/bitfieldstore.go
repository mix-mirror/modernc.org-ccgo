// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"fmt"
	"strings"

	"modernc.org/cc/v4"
)

// bitFieldStore is an expression statement assigning a bit-field, see
// bitFieldStores.
type bitFieldStore struct {
	f    *cc.Field
	key  string // identifies the object and the storage unit of f
	lhs  *cc.PostfixExpression
	rhs  cc.ExpressionNode
	stmt *cc.Statement
}

// bitFieldStores merges the assignments of bit-fields in a row starting at l,
// like
//
//	p->a = 0;
//	p->b = 1;
//	p->c = x;
//
// into one store when there are two or more of them. It emits that store and
// returns the last list item it covers, or nil if l does not start such a run.
//
// Each assignment is a read-modify-write of the storage unit of the bit-field,
// so a run of them is a chain of loads and stores to the same address, and the
// Go compiler does not merge them. The merged store is one read-modify-write, or
// a plain store where the run assigns every bit of the unit.
//
// The assignments must select bit-fields of the same storage unit of the same
// object, which must be a struct or union named by an identifier, possibly a
// member of one, or the target of an automatic pointer whose address is never
// taken, so no store of the run can change which object it is. The assigned
// values may only use constants and automatic variables whose address is never
// taken, so evaluating them before the stores reads the same values. The run
// ends at a label, at anything volatile and at a bit-field another one of the
// run overlaps, which only happens in a union. value reports whether the last
// item of the list is the value of a statement expression, which does not take
// part.
func (c *ctx) bitFieldStores(w writer, l *cc.BlockItemList, value bool) *cc.BlockItemList {
	first := c.bitFieldStore(l.BlockItem)
	if first == nil {
		return nil
	}

	run := []*bitFieldStore{first}
	mask := first.f.Mask()
	last := l
	for l2 := l.BlockItemList; l2 != nil && !(value && l2.BlockItemList == nil); l2 = l2.BlockItemList {
		s := c.bitFieldStore(l2.BlockItem)
		if s == nil || s.key != first.key || s.f.Mask()&mask != 0 {
			break
		}

		run = append(run, s)
		mask |= s.f.Mask()
		last = l2
	}
	if len(run) < 2 {
		return nil
	}

	if c.f.autovarNesting == 0 {
		defer c.f.rewindAutovars()
	}
	for _, s := range run {
		sep := sep(s.stmt)
		if c.task.positions {
			sep = strings.TrimRight(sep, "\n\r\t ")
		}
		w.w("%s%s", sep, c.posComment(s.stmt))
	}

	var p *buf
	switch x := first.lhs; x.Case {
	case cc.PostfixExpressionSelect:
		p = c.pin(x, c.expr(w, x.PostfixExpression, x.PostfixExpression.Type().Pointer(), exprUintptr))
	default:
		p = c.expr(w, x.PostfixExpression, nil, exprDefault)
	}
	bits := first.f.AccessBytes() * 8
	ut := fmt.Sprintf("%suint%d", tag(preserve), bits)
	var k uint64 // the constant bits
	var terms []string
	for _, s := range run {
		f := s.f
		switch v := s.rhs.Value().(type) {
		case cc.Int64Value:
			k |= c.bitFieldBits(f, uint64(v))
		case cc.UInt64Value:
			k |= c.bitFieldBits(f, uint64(v))
		default:
			terms = append(terms, fmt.Sprintf("%s(%s)<<%d&%#0x", ut, c.expr(w, s.rhs, f.Type(), exprDefault), f.OffsetBits(), f.Mask()))
		}
	}
	if k != 0 || len(terms) == 0 {
		terms = append([]string{fmt.Sprintf("%#0x", k)}, terms...)
	}
	switch {
	case bits == 64 && mask == ^uint64(0) || bits < 64 && mask == 1<<bits-1:
		w.w("*(*%s)(%s) = %s;", ut, unsafeFromUintptr(fmt.Sprintf("%s+%d", p, first.f.Offset())), strings.Join(terms, "|"))
	default:
		w.w("%sSetBitFieldPtr%dUint%[2]d(%s+%d, %s, 0, %#0x);", c.task.tlsQualifier, bits, p, first.f.Offset(), strings.Join(terms, "|"), mask)
	}
	return last
}

// bitFieldBits returns the constant v assigned to the bit-field f at its
// position in the storage unit.
func (c *ctx) bitFieldBits(f *cc.Field, v uint64) uint64 {
	if f.Type().Kind() == cc.Bool && v != 0 {
		v = 1
	}
	return v << f.OffsetBits() & f.Mask()
}

// bitFieldStore returns n as a bitFieldStore if it is one that bitFieldStores
// can merge.
func (c *ctx) bitFieldStore(n *cc.BlockItem) *bitFieldStore {
	if n == nil || n.Case != cc.BlockItemStmt || n.Statement.Case != cc.StatementExpr {
		return nil
	}

	es := n.Statement.ExpressionStatement
	if es.AttributeSpecifierList != nil {
		return nil
	}

	e, ok := es.ExpressionList.(*cc.AssignmentExpression)
	if !ok || e.Case != cc.AssignmentExpressionAssign || e.UnaryExpression.Type().Attributes().IsVolatile() {
		return nil
	}

	x, ok := c.unparen(e.UnaryExpression).(*cc.PostfixExpression)
	if !ok || x.Case != cc.PostfixExpressionSelect && x.Case != cc.PostfixExpressionPSelect {
		return nil
	}

	f := x.Field()
	if f == nil || !f.IsBitfield() || !cc.IsIntegerType(f.Type()) || !c.bitFieldValue(e.AssignmentExpression) {
		return nil
	}

	base := c.bitFieldBase(x.PostfixExpression, x.Case == cc.PostfixExpressionPSelect)
	if base == "" {
		return nil
	}

	return &bitFieldStore{
		f:    f,
		key:  fmt.Sprintf("%s %d %d", base, f.Offset(), f.AccessBytes()),
		lhs:  x,
		rhs:  e.AssignmentExpression,
		stmt: n.Statement,
	}
}

// bitFieldBase returns a key naming n, the object or, if ptr is true, the
// pointer to the object a bit-field is selected from, when bitFieldStores can
// use it, or "" otherwise. n must be an identifier of a struct or union, a
// member of one, a member of the target of an automatic pointer whose address
// is never taken, or, if ptr is true, such a pointer, and nothing in it may be
// volatile.
func (c *ctx) bitFieldBase(n cc.ExpressionNode, ptr bool) string {
	if n.Type().Attributes().IsVolatile() {
		return ""
	}

	if ptr {
		pt, ok := n.Type().(*cc.PointerType)
		if !ok || pt.Elem().Attributes().IsVolatile() {
			return ""
		}
	}

	switch x := c.unparen(n).(type) {
	case *cc.PrimaryExpression:
		if x.Case != cc.PrimaryExpressionIdent {
			return ""
		}

		d := c.bitFieldDeclarator(x)
		if d == nil || ptr && (d.StorageDuration() != cc.Automatic || d.AddressTaken()) {
			return ""
		}

		return fmt.Sprintf("%p", d)
	case *cc.PostfixExpression:
		if ptr {
			return ""
		}

		var op string
		switch x.Case {
		case cc.PostfixExpressionSelect:
			op = "."
		case cc.PostfixExpressionPSelect:
			op = "->"
		default:
			return ""
		}

		f := x.Field()
		if f == nil {
			return ""
		}

		if s := c.bitFieldBase(x.PostfixExpression, x.Case == cc.PostfixExpressionPSelect); s != "" {
			return fmt.Sprintf("%s%s%d", s, op, f.Offset())
		}
	}
	return ""
}

// bitFieldDeclarator returns the variable the identifier n names, or nil.
func (c *ctx) bitFieldDeclarator(n *cc.PrimaryExpression) *cc.Declarator {
	switch x := n.ResolvedTo().(type) {
	case *cc.Declarator:
		return x
	case *cc.Parameter:
		return x.Declarator
	}
	return nil
}

// bitFieldValue reports whether n, the value assigned to a bit-field, has an
// integer type and uses only constants and automatic variables whose address is
// never taken, so it neither reads memory nor has side effects. The check is
// structural: the value cc computes for a constant expression does not mean it
// has no side effects, (f(), 1) has one.
func (c *ctx) bitFieldValue(n cc.ExpressionNode) bool {
	if n == nil || !cc.IsIntegerType(n.Type()) || n.Type().Attributes().IsVolatile() {
		return false
	}

	switch x := n.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionInt, cc.PrimaryExpressionChar, cc.PrimaryExpressionLChar:
			return true
		case cc.PrimaryExpressionIdent:
			if _, ok := x.ResolvedTo().(*cc.Enumerator); ok {
				return true
			}

			d := c.bitFieldDeclarator(x)
			return d != nil && d.StorageDuration() == cc.Automatic && !d.AddressTaken()
		case cc.PrimaryExpressionExpr:
			return c.bitFieldValue(c.unparen(x))
		}
	case *cc.CastExpression:
		return x.Case == cc.CastExpressionCast && c.bitFieldValue(x.CastExpression)
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionMinus, cc.UnaryExpressionCpl, cc.UnaryExpressionNot, cc.UnaryExpressionPlus:
			return c.bitFieldValue(x.CastExpression)
		case cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionSizeofType, cc.UnaryExpressionAlignofExpr, cc.UnaryExpressionAlignofType:
			// The operand is not evaluated unless it is a VLA, which has no
			// constant size.
			switch x.Value().(type) {
			case cc.Int64Value, cc.UInt64Value:
				return true
			}
		}
	case *cc.MultiplicativeExpression:
		// Not / or %, those can trap.
		return x.Case == cc.MultiplicativeExpressionMul && c.bitFieldValue(x.MultiplicativeExpression) && c.bitFieldValue(x.CastExpression)
	case *cc.AdditiveExpression:
		return c.bitFieldValue(x.AdditiveExpression) && c.bitFieldValue(x.MultiplicativeExpression)
	case *cc.ShiftExpression:
		return c.bitFieldValue(x.ShiftExpression) && c.bitFieldValue(x.AdditiveExpression)
	case *cc.RelationalExpression:
		return c.bitFieldValue(x.RelationalExpression) && c.bitFieldValue(x.ShiftExpression)
	case *cc.EqualityExpression:
		return c.bitFieldValue(x.EqualityExpression) && c.bitFieldValue(x.RelationalExpression)
	case *cc.AndExpression:
		return c.bitFieldValue(x.AndExpression) && c.bitFieldValue(x.EqualityExpression)
	case *cc.ExclusiveOrExpression:
		return c.bitFieldValue(x.ExclusiveOrExpression) && c.bitFieldValue(x.AndExpression)
	case *cc.InclusiveOrExpression:
		return c.bitFieldValue(x.InclusiveOrExpression) && c.bitFieldValue(x.ExclusiveOrExpression)
	case *cc.LogicalAndExpression:
		return c.bitFieldValue(x.LogicalAndExpression) && c.bitFieldValue(x.InclusiveOrExpression)
	case *cc.LogicalOrExpression:
		return c.bitFieldValue(x.LogicalOrExpression) && c.bitFieldValue(x.LogicalAndExpression)
	case *cc.ConditionalExpression:
		return c.bitFieldValue(x.LogicalOrExpression) && c.bitFieldValue(x.ExpressionList) && c.bitFieldValue(x.ConditionalExpression)
	}
	return false
}
