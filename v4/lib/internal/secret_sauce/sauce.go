// Copyright 2025 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sauce

import (
	"reflect"
	"strings"

	"modernc.org/gc/v3"
)

var (
	zeroReflectValue reflect.Value
)

const (
	walkTok = iota
	walkPre
	walkPost
)

func walk(n gc.Node, fn func(n gc.Node, mode int)) {
	if n == nil {
		return
	}

	if _, ok := n.(gc.Token); ok {
		fn(n, walkTok)
		return
	}

	t := reflect.TypeOf(n)
	v := reflect.ValueOf(n)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		v = v.Elem()
	}
	if v == zeroReflectValue || v.IsZero() || t.Kind() != reflect.Struct {
		return
	}

	fn(n, walkPre)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		if m, ok := v.Field(i).Interface().(gc.Node); ok {
			walk(m, fn)
		}
	}
	fn(n, walkPost)
}

func declScope(nm string, scope *gc.Scope) (r *gc.Scope) {
	var stop bool
	for ; scope != nil; scope = scope.Parent() {
		scope.Iterate(func(name string, n gc.Node) bool {
			if name == nm {
				stop = true
				r = scope
			}
			return stop
		})
		if stop {
			break
		}
	}
	return r
}

func DeadVariableElimination(filename string, buf []byte) (out []byte, err error) {
	ast, err := gc.ParseFile(filename, buf)
	if err != nil {
		return nil, err
	}

	type varinfo struct{ referenced int }
	vars := map[*gc.Scope]map[string]*varinfo{}

	register := func(n gc.Node, sc *gc.Scope, nm string) {
		m := vars[sc]
		if m == nil {
			m = map[string]*varinfo{}
			vars[sc] = m
		}
		if m[nm] == nil {
			m[nm] = &varinfo{}
		}
	}

	reference := func(n gc.Node, sc *gc.Scope, nm string, delta int) {
		if sc = declScope(nm, sc); sc == nil {
			return
		}

		m := vars[sc]
		if m == nil {
			return
		}

		if info := m[nm]; info != nil {
			info.referenced += delta
		}
	}

	eliminate := func(n gc.Node, sc *gc.Scope, nm string) bool {
		if sc = declScope(nm, sc); sc == nil {
			return false
		}

		m := vars[sc]
		if m == nil {
			return false
		}

		if info := m[nm]; info != nil {
			return info.referenced == 0
		}

		return false
	}

	kill := func(n gc.Node) {
		walk(n, func(n gc.Node, mode int) {
			if mode != walkTok {
				return
			}

			if tok := n.(gc.Token); tok.IsValid() {
				tok.SetSep("")
				tok.SetSrc("")
			}
		})
	}

	cleanIdentList := func(n *gc.IdentifierListNode) {
		needComma := false
		for ; n != nil; n = n.List {
			s := n.IDENT.Src()
			switch {
			case needComma && s == "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
			case !needComma && s == "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
			case !needComma && s != "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
				needComma = true
			}
		}
	}

	cleanExpressionList := func(n *gc.ExpressionListNode) {
		needComma := false
		for ; n != nil; n = n.List {
			s := strings.TrimSpace(n.Expression.Source(false))
			switch {
			case needComma && s == "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
			case !needComma && s == "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
			case !needComma && s != "":
				if n.COMMA.IsValid() {
					n.COMMA.SetSrc("")
				}
				needComma = true
			}
		}
	}

	for n := ast.SourceFile.TopLevelDeclList; n != nil; n = n.List {
		switch x := n.TopLevelDecl.(type) {
		case *gc.FunctionDeclNode:
			// analyze
			walk(x, func(n gc.Node, mode int) {
				if mode != walkPre {
					return
				}

				switch x := n.(type) {
				case *gc.VarSpecNode:
					nm := x.IDENT.Src()
					sc := x.LexicalScope()
					register(x, sc, nm)
					if x.ExpressionList != nil {
						reference(x, sc, nm, 1)
					}
				case *gc.VarSpec2Node:
					sc := x.LexicalScope()
					for l := x.IdentifierList; l != nil; l = l.List {
						nm := l.IDENT.Src()
						register(x, sc, l.IDENT.Src())
						if x.ExpressionList != nil {
							reference(x, sc, nm, 1)
						}
					}
				case *gc.AssignmentNode:
					rhs := x.ExpressionList2
					for lhs := x.ExpressionList; lhs != nil; lhs, rhs = lhs.List, rhs.List {
						switch lExpr := lhs.Expression.(type) {
						case *gc.OperandNameNode:
							switch lNm := lExpr.Name.Src(); {
							case lNm != "_":
								reference(lExpr, lExpr.LexicalScope(), lNm, 1)
								switch rExpr := rhs.Expression.(type) {
								case *gc.OperandNameNode:
									rNm := rExpr.Name.Src()
									reference(rExpr, rExpr.LexicalScope(), rNm, 1)
								}
							default:
								switch rExpr := rhs.Expression.(type) {
								case *gc.OperandNameNode:
									rNm := rExpr.Name.Src()
									reference(rExpr, rExpr.LexicalScope(), rNm, -1)
								}
							}
						}
					}
				case *gc.OperandNameNode:
					nm := x.Name.Src()
					reference(x, x.LexicalScope(), nm, 1)
				case *gc.OperandQualifiedNameNode:
					qi := x.Name
					nm := qi.PackageName.Src()
					reference(x, x.LexicalScope(), nm, 1)
					nm = qi.IDENT.Src()
					reference(x, x.LexicalScope(), nm, 1)
				}
			})

			// eliminate
			walk(x, func(n gc.Node, mode int) {
				if mode != walkPre {
					return
				}

				switch x := n.(type) {
				case *gc.VarDeclNode:
					switch spec := x.VarSpec.(type) {
					case *gc.VarSpecNode:
						nm := spec.IDENT.Src()
						sc := spec.LexicalScope()
						if eliminate(spec, sc, nm) {
							kill(x)
						}
					case *gc.VarSpec2Node:
						sc := spec.LexicalScope()
						var specs, kills []*gc.IdentifierListNode
						for l := spec.IdentifierList; l != nil; l = l.List {
							specs = append(specs, l)
							nm := l.IDENT.Src()
							if eliminate(spec, sc, nm) {
								kills = append(kills, l)
							}
						}
						if len(kills) == len(specs) {
							kill(x)
							break
						}

						for _, v := range kills {
							v.IDENT.SetSrc("")
						}
						cleanIdentList(spec.IdentifierList)
					}
				case *gc.AssignmentNode:
					var left, right []*gc.ExpressionListNode
					var lKill, rKill []*gc.OperandNameNode
					for lhs, rhs := x.ExpressionList, x.ExpressionList2; lhs != nil; lhs, rhs = lhs.List, rhs.List {
						left = append(left, lhs)
						right = append(right, rhs)
						switch lExpr := lhs.Expression.(type) {
						case *gc.OperandNameNode:
							if lNm := lExpr.Name.Src(); lNm == "_" {
								switch rExpr := rhs.Expression.(type) {
								case *gc.OperandNameNode:
									rNm := rExpr.Name.Src()
									if eliminate(rExpr, rExpr.LexicalScope(), rNm) {
										lKill = append(lKill, lExpr)
										rKill = append(rKill, rExpr)
									}
								}
							}
						}
					}

					if len(left) == len(lKill) {
						kill(x)
						break
					}

					for _, v := range lKill {
						v.Name.SetSrc("")
					}
					for _, v := range rKill {
						v.Name.SetSrc("")
					}
					cleanExpressionList(x.ExpressionList)
					cleanExpressionList(x.ExpressionList2)
				}
			})
		}
	}
	return []byte(ast.SourceFile.Source(true)), nil
}
