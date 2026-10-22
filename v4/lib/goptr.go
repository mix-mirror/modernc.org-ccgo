// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"fmt"
	"regexp"
	"strconv"

	"modernc.org/cc/v4"
)

// Go pointers for C pointers that do not escape.
//
// A C local whose address is taken is pinned into the TLS frame of its
// function, because the address travels as a uintptr, which the Go garbage
// collector does not follow and a stack copy does not adjust. The frame costs
// an allocation and a defer per call.
//
// This file keeps such a local a Go local when its address only ever reaches
// code visible in the translation unit that uses it in the ways Go allows for
// an unsafe.Pointer: dereferences, comparisons, pointer arithmetic whose
// result is used the same way, assignments to other such variables and
// arguments of direct calls whose parameter is such a variable. The variables
// holding the address, parameters and locals of pointer type, get the Go type
// unsafe.Pointer instead of uintptr. Go's escape analysis then keeps the local
// on the goroutine stack and a stack copy adjusts the pointers.
//
// The analysis, analyzeGoPtr, runs once per translation unit before any
// function is emitted. It is a greatest fixpoint over the candidate variables:
// a variable is safe if every use of its value is safe, where an assignment or
// an argument is safe if the receiving variable is. Anything the analysis does
// not recognize is a sink. A safe variable is converted when the address of an
// automatic variable reaches it, directly or through other converted
// variables, so that a conversion without a benefit does not change the
// generated code. --goptr=all converts every safe variable, which exercises
// the emitter, and --goptr=off disables the conversion.
//
// A function with a converted parameter is emitted with the new signature. If
// it has external linkage or is referenced other than by a direct call, the
// body goes under a private name and a wrapper with the original name and the
// original uintptr signature converts the arguments, so that the ABI seen from
// other translation units and through function pointers stays the same.
//
// In the emitter, exprUnsafePointer is the mode of a C pointer value rendered
// as a Go unsafe.Pointer. goPtrExpr produces it for any pointer expression.
// Reading a converted variable in any other value mode is an internal error:
// the analysis must have classified every such use as safe, and a silent
// conversion to uintptr would be exactly the bug this file exists to avoid.

// goPtrSuffix is appended to the link name of a function body emitted with
// converted parameters when the original name is taken by the wrapper.
const goPtrSuffix = "__ccgo_gp"

// goPtrDefault is the default of --goptr. Tests may set it.
var goPtrDefault = "on"

type goPtrUseKind int

const (
	goPtrSafe goPtrUseKind = iota // the value is used in place: dereferenced, compared, ...
	goPtrSink                     // the value escapes: stored, returned, converted to an integer, ...
	goPtrFlow                     // the value is assigned to target or passed as target
)

type goPtrUse struct {
	kind   goPtrUseKind
	target *cc.Declarator // goPtrFlow
}

// goPtrFn describes a function definition with at least one converted
// parameter.
type goPtrFn struct {
	converted []bool // per normalized parameter
	linkName  string // the body is emitted under this link name
	wrapper   bool   // a wrapper with the original name and signature calls linkName
}

type goPtrFnCand struct {
	d      *cc.Declarator
	params []*cc.Parameter // normalized
}

// goPtrInfo is the per translation unit result of the analysis.
type goPtrInfo struct {
	byName map[string]*goPtrFn         // function name -> info, for call sites
	fns    map[*cc.Declarator]*goPtrFn // function definition declarator -> info
	vars   map[*cc.Declarator]bool     // converted parameters and locals

	// analysis state
	all      bool                    // --goptr=all
	cands    map[*cc.Declarator]bool // candidate variables
	fnCands  map[*cc.FunctionDefinition]*goPtrFnCand
	fnByName map[string]*goPtrFnCand
	refs     map[string]bool         // candidate functions referenced other than by a direct call
	roots    map[*cc.Declarator]bool // candidates that receive the address of an automatic variable
	uses     map[*cc.Declarator][]goPtrUse

	// frame analysis, see goPtrFrames
	calls      map[*cc.FunctionDefinition]map[*cc.FunctionDefinition]bool // direct calls
	fnDefs     map[string]*cc.FunctionDefinition                          // every function definition, by name
	fnOrder    []*cc.FunctionDefinition                                   // in translation unit order
	forceFrame map[*cc.Declarator]bool                                    // function declarator: keep a frame
	framed     map[*cc.Declarator]bool                                    // function declarator: has a frame, set after its first pass
	inlined    map[*cc.FunctionDefinition]bool                            // inlined by ccgo, not emitted
	lostCand   map[*cc.Declarator]bool                                    // function declarator: the address of a local flows into a converted variable
	markers    []*cc.Declarator                                           // frame markers emitted, by index
	rootFlows  map[*cc.FunctionDefinition][]*cc.Declarator                // candidates receiving the address of a local
}

// callee returns the info of the function d names, if it has a converted
// parameter.
func (g *goPtrInfo) callee(d *cc.Declarator) *goPtrFn {
	if g == nil || d == nil || d.Type().Kind() != cc.Function || d.Linkage() == cc.None {
		return nil
	}

	return g.byName[d.Name()]
}

func (c *ctx) goPtrVar(d *cc.Declarator) bool { return c.goPtr != nil && d != nil && c.goPtr.vars[d] }

func goPtrType() string {
	return fmt.Sprintf("%sunsafe.%sPointer", tag(importQualifier), tag(preserve))
}

func sameNode(a cc.ExpressionNode, b cc.Node) bool { return a != nil && cc.Node(a) == b }

// analyzeGoPtr computes c.goPtr for the translation unit in c.ast.
func (c *ctx) analyzeGoPtr() {
	c.goPtr = nil
	if c.task.goPtr == "off" || c.task.header {
		return
	}

	g := &goPtrInfo{
		all:      c.task.goPtr == "all",
		byName:   map[string]*goPtrFn{},
		cands:    map[*cc.Declarator]bool{},
		fnByName: map[string]*goPtrFnCand{},
		fnCands:  map[*cc.FunctionDefinition]*goPtrFnCand{},
		fns:      map[*cc.Declarator]*goPtrFn{},
		refs:     map[string]bool{},
		roots:    map[*cc.Declarator]bool{},
		uses:     map[*cc.Declarator][]goPtrUse{},
		vars:     map[*cc.Declarator]bool{},

		calls:      map[*cc.FunctionDefinition]map[*cc.FunctionDefinition]bool{},
		fnDefs:     map[string]*cc.FunctionDefinition{},
		forceFrame: map[*cc.Declarator]bool{},
		framed:     map[*cc.Declarator]bool{},
		inlined:    map[*cc.FunctionDefinition]bool{},
		lostCand:   map[*cc.Declarator]bool{},
		rootFlows:  map[*cc.FunctionDefinition][]*cc.Declarator{},
	}

	// A function that is the target of an alias keeps its signature: the
	// alias is emitted as a wrapper that passes the parameters on as they
	// are.
	aliased := map[string]bool{}
	for _, nodes := range c.ast.Scope.Nodes {
		for _, v := range nodes {
			d, ok := v.(*cc.Declarator)
			if !ok {
				continue
			}

			if s := c.aliasAttr(d.Type()); s != "" {
				aliased[s] = true
			}
			if cd := c.aliasAttrDecl(d.Type()); cd != nil {
				aliased[cd.Name()] = true
			}
		}
	}

	for n := c.ast.TranslationUnit; n != nil; n = n.TranslationUnit {
		ed := n.ExternalDeclaration
		if ed == nil || ed.Case != cc.ExternalDeclarationFuncDef {
			continue
		}

		fd := ed.FunctionDefinition
		d := fd.Declarator
		if _, ok := d.Type().(*cc.FunctionType); !ok {
			continue
		}

		g.fnDefs[d.Name()] = fd
		g.fnOrder = append(g.fnOrder, fd)
		if c.isInlineFunc(d) {
			g.inlined[fd] = true
		}
		if aliased[d.Name()] || !c.goPtrFnCandidate(fd) {
			continue
		}

		fc := &goPtrFnCand{d: d, params: c.normalizeParams(d.Type().(*cc.FunctionType).Parameters())}
		g.fnCands[fd] = fc
		g.fnByName[d.Name()] = fc
		for _, p := range fc.params {
			if c.goPtrVarCandidate(p.Declarator) {
				g.cands[p.Declarator] = true
			}
		}
	}

	for n := c.ast.TranslationUnit; n != nil; n = n.TranslationUnit {
		if ed := n.ExternalDeclaration; ed != nil {
			c.goPtrWalk(g, ed)
		}
	}

	// Greatest fixpoint: a variable is safe when all its uses are.
	safe := map[*cc.Declarator]bool{}
	for d := range g.cands {
		safe[d] = true
	}
	for changed := true; changed; {
		changed = false
		for d := range safe {
			for _, u := range g.uses[d] {
				if u.kind == goPtrSink || u.kind == goPtrFlow && !safe[u.target] {
					delete(safe, d)
					changed = true
					break
				}
			}
		}
	}

	// A safe variable is converted when an address of an automatic variable
	// can reach it.
	wanted := map[*cc.Declarator]bool{}
	switch {
	case g.all:
		wanted = safe
	default:
		var work []*cc.Declarator
		for d := range g.roots {
			if safe[d] {
				wanted[d] = true
				work = append(work, d)
			}
		}
		for len(work) != 0 {
			d := work[len(work)-1]
			work = work[:len(work)-1]
			for _, u := range g.uses[d] {
				if u.kind == goPtrFlow && safe[u.target] && !wanted[u.target] {
					wanted[u.target] = true
					work = append(work, u.target)
				}
			}
		}
	}
	for d := range wanted {
		g.vars[d] = true
	}

	for _, fc := range g.fnCands {
		var converted []bool
		any := false
		for _, p := range fc.params {
			v := g.vars[p.Declarator]
			converted = append(converted, v)
			any = any || v
		}
		if !any {
			continue
		}

		nm := fc.d.Name()
		fn := &goPtrFn{converted: converted, wrapper: fc.d.Linkage() == cc.External || g.refs[nm]}
		fn.linkName = c.declaratorTag(fc.d) + nm
		if fn.wrapper {
			fn.linkName = tag(staticInternal) + nm + goPtrSuffix
		}
		g.fns[fc.d] = fn
		g.byName[nm] = fn
	}
	// Functions whose frame the conversion may have removed: the address of
	// a local flows into a converted variable, in the function itself or in
	// a function ccgo inlines into it.
	for _, fd := range g.fnOrder {
		seen := map[*cc.FunctionDefinition]bool{}
		var walk func(fd *cc.FunctionDefinition) bool
		walk = func(fd *cc.FunctionDefinition) bool {
			if seen[fd] {
				return false
			}

			seen[fd] = true
			for _, d := range g.rootFlows[fd] {
				if g.vars[d] {
					return true
				}
			}
			for callee := range g.calls[fd] {
				if g.inlined[callee] && walk(callee) {
					return true
				}
			}
			return false
		}
		if walk(fd) {
			g.lostCand[fd.Declarator] = true
		}
	}
	c.goPtr = g
}

// goPtrFnCandidate reports whether the parameters and locals of the function
// defined by fd may be converted.
func (c *ctx) goPtrFnCandidate(fd *cc.FunctionDefinition) bool {
	d := fd.Declarator
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok || fd.UsesVectors() || fd.DeclarationList != nil {
		return false
	}

	switch d.Linkage() {
	case cc.External, cc.Internal:
		// ok
	default:
		return false
	}

	nm := d.Name()
	if nm == "main" || c.task.hidden.has(nm) {
		return false
	}

	// Functions ccgo inlines, see externalDeclaration.
	if c.isInlineFunc(d) {
		return false
	}

	if c.weakAttr(d.Type()) || c.aliasAttr(d.Type()) != "" || c.aliasAttrDecl(d.Type()) != nil {
		return false
	}

	// registerLocal pins every local of such a function.
	if re := c.task.hotSwitchRE; re != nil && re.MatchString(nm) {
		return false
	}

	for _, p := range c.normalizeParams(ft.Parameters()) {
		if p.Declarator == nil {
			return false
		}
	}
	return true
}

// goPtrVarCandidate reports whether d, a parameter or a local, may hold a Go
// pointer.
func (c *ctx) goPtrVarCandidate(d *cc.Declarator) bool {
	if d == nil || d.IsTypename() || d.AddressTaken() || d.IsVolatile() || d.IsAtomic() || d.IsThreadLocal() || d.IsExtern() || d.IsStatic() {
		return false
	}

	if d.StorageDuration() != cc.Automatic || d.Linkage() != cc.None {
		return false
	}

	pt, ok := d.Type().(*cc.PointerType)
	if !ok || c.isVaList(d.Type()) || d.Type().Attributes().IsVolatile() {
		return false
	}

	elem := pt.Elem()
	if elem.Kind() == cc.Function || elem.Attributes().IsVolatile() {
		return false
	}

	if _, ok := c.isVLA(elem); ok {
		return false
	}

	return true
}

// goPtrWalk records the uses of candidate variables in root, the references
// to candidate functions, the roots, automatic variables whose address is
// assigned to a candidate, and the direct calls.
func (c *ctx) goPtrWalk(g *goPtrInfo, root cc.Node) {
	var anc []cc.Node
	var fd *cc.FunctionDefinition
	walkC(root, func(n cc.Node, mode int) {
		switch mode {
		case walkPost:
			anc = anc[:len(anc)-1]
			if _, ok := n.(*cc.FunctionDefinition); ok {
				fd = nil
			}
			return
		case walkTok:
			return
		}

		anc = append(anc, n)
		switch x := n.(type) {
		case *cc.FunctionDefinition:
			fd = x
		case *cc.InitDeclarator:
			c.goPtrInitDeclarator(g, fd, x)
		case *cc.PrimaryExpression:
			if x.Case == cc.PrimaryExpressionIdent {
				c.goPtrIdent(g, anc)
				c.goPtrRootUse(g, fd, anc)
			}
		case *cc.PostfixExpression:
			switch x.Case {
			case cc.PostfixExpressionCall:
				c.goPtrCallRoots(g, x)
				c.goPtrCall(g, fd, x)
			case cc.PostfixExpressionSelect, cc.PostfixExpressionIndex:
				c.goPtrRootUse(g, fd, anc)
			}
		case *cc.AssignmentExpression:
			if x.Case == cc.AssignmentExpressionAssign {
				c.goPtrAssignRoots(g, anc, x)
			}
		case *cc.UnaryExpression:
			if x.Case == cc.UnaryExpressionAddrof {
				c.goPtrRootUse(g, fd, anc)
			}
		}
	})
}

// goPtrFrames keeps the stack bound of recursion: a C program running on a
// TLS stack can bound its recursion by the number of TLS frames (libquickjs
// compares libc.TLS.StackSlots with a limit), which works as long as every
// recursive cycle holds at least one frame. A conversion that removes the
// last frame of a cycle would leave the recursion to Go's fatal stack
// overflow. So in every cycle of direct calls within the translation unit
// whose remaining frames the conversion took away, one of the functions that
// lost its frame keeps a one-slot frame.
//
// It runs when the translation unit has been emitted, so that every
// function's first pass has decided its frame (framed). A function that has
// none and in which the address of a local only flows into converted
// variables (lostCand) lost its frame to the conversion; it has emitted a
// marker where the frame would go, see goPtrFrameMarker, which
// goPtrResolveFrames then turns into the frame or removes.
func (c *ctx) goPtrFrames(g *goPtrInfo) {
	type node struct {
		fd      *cc.FunctionDefinition
		callees map[*node]bool
		lost    bool
		index   int // translation unit order
	}
	nodes := map[*cc.FunctionDefinition]*node{}
	var order []*node
	for i, fd := range g.fnOrder {
		d := fd.Declarator
		if framed, ok := g.framed[d]; !ok || framed {
			continue // not emitted, or framed
		}

		n := &node{fd: fd, callees: map[*node]bool{}, lost: g.lostCand[d], index: i}
		nodes[fd] = n
		order = append(order, n)
	}
	// The calls, with those of the functions ccgo inlines made by the
	// caller. A call into a framed function does not matter: it leaves no
	// edge, so a cycle through it is not a cycle here.
	for _, n := range order {
		seen := map[*cc.FunctionDefinition]bool{}
		var walk func(fd *cc.FunctionDefinition)
		walk = func(fd *cc.FunctionDefinition) {
			if seen[fd] {
				return
			}

			seen[fd] = true
			for callee := range g.calls[fd] {
				switch {
				case g.inlined[callee]:
					walk(callee)
				default:
					if m := nodes[callee]; m != nil {
						n.callees[m] = true
					}
				}
			}
		}
		walk(n.fd)
	}

	// Tarjan's strongly connected components over the active nodes.
	active := map[*node]bool{}
	for _, n := range order {
		active[n] = true
	}
	sccs := func() (r [][]*node) {
		index := 0
		indices := map[*node]int{}
		low := map[*node]int{}
		onStack := map[*node]bool{}
		var stack []*node
		var connect func(v *node)
		connect = func(v *node) {
			indices[v] = index
			low[v] = index
			index++
			stack = append(stack, v)
			onStack[v] = true
			for w := range v.callees {
				if !active[w] {
					continue
				}

				switch _, ok := indices[w]; {
				case !ok:
					connect(w)
					low[v] = min(low[v], low[w])
				case onStack[w]:
					low[v] = min(low[v], indices[w])
				}
			}
			if low[v] == indices[v] {
				var scc []*node
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					scc = append(scc, w)
					if w == v {
						break
					}
				}
				r = append(r, scc)
			}
		}
		for _, n := range order {
			if active[n] {
				if _, ok := indices[n]; !ok {
					connect(n)
				}
			}
		}
		return r
	}

	for {
		changed := false
		for _, scc := range sccs() {
			if len(scc) == 1 && !scc[0].callees[scc[0]] {
				delete(active, scc[0]) // not recursive
				continue
			}

			var pick *node
			for _, n := range scc {
				if n.lost && (pick == nil || n.index < pick.index) {
					pick = n
				}
			}
			switch {
			case pick != nil:
				g.forceFrame[pick.fd.Declarator] = true
				delete(active, pick)
				changed = true
			default:
				// No frame was lost here: the recursion was not bounded
				// before either.
				for _, n := range scc {
					delete(active, n)
				}
			}
		}
		if !changed {
			return
		}
	}
}

// goPtrFrameMarker returns the marker a function emits where its frame
// would be, see goPtrFrames.
func (c *ctx) goPtrFrameMarker(d *cc.Declarator) string {
	g := c.goPtr
	g.markers = append(g.markers, d)
	return fmt.Sprintf("/*%s%d*/", goPtrMarker, len(g.markers)-1)
}

const goPtrMarker = "__ccgo_frame_marker_"

// goPtrResolveFrames replaces the frame markers in the emitted translation
// unit b with a frame or with nothing.
func (c *ctx) goPtrResolveFrames(b []byte) []byte {
	g := c.goPtr
	if g == nil || len(g.markers) == 0 {
		return b
	}

	c.goPtrFrames(g)
	frame := fmt.Sprintf("%stls.%sAlloc(16); defer %[1]stls.%[2]sFree(16);", tag(ccgo), tag(preserve))
	re := regexp.MustCompile(`/\*` + goPtrMarker + `(\d+)\*/`)
	return re.ReplaceAllFunc(b, func(m []byte) []byte {
		i, err := strconv.Atoi(string(re.FindSubmatch(m)[1]))
		if err != nil || i < 0 || i >= len(g.markers) {
			c.err(errorf("internal error: frame marker %s", m))
			return nil
		}

		if g.forceFrame[g.markers[i]] {
			return []byte(frame)
		}

		return nil
	})
}

// goPtrCall records the direct call n made by fd.
func (c *ctx) goPtrCall(g *goPtrInfo, fd *cc.FunctionDefinition, n *cc.PostfixExpression) {
	if fd == nil {
		return
	}

	d := c.declaratorOf(n.PostfixExpression)
	if d == nil || d.Type().Kind() != cc.Function || d.Linkage() == cc.None {
		return
	}

	if callee := g.fnDefs[d.Name()]; callee != nil {
		m := g.calls[fd]
		if m == nil {
			m = map[*cc.FunctionDefinition]bool{}
			g.calls[fd] = m
		}
		m[callee] = true
	}
}

// goPtrRootUse records, for the frame analysis, the address of an automatic
// variable, the operand of & or a decaying array at the top of anc, that
// flows into a candidate.
func (c *ctx) goPtrRootUse(g *goPtrInfo, fd *cc.FunctionDefinition, anc []cc.Node) {
	if fd == nil {
		return
	}

	n := anc[len(anc)-1].(cc.ExpressionNode)
	if _, ok := n.(*cc.UnaryExpression); !ok {
		// A decaying array. Not where it stays an object, see goPtrClassify.
		if !c.goPtrIsArrayLvalue(n) {
			return
		}

		cur := anc[len(anc)-1]
		for i := len(anc) - 2; i >= 0; i-- {
			switch x := anc[i].(type) {
			case *cc.PrimaryExpression:
				if x.Case == cc.PrimaryExpressionExpr {
					cur = anc[i]
					continue
				}
			case *cc.ExpressionList:
				cur = anc[i]
				continue
			case *cc.PostfixExpression:
				if x.Case == cc.PostfixExpressionIndex {
					if base, _ := c.goPtrIndexParts(x); sameNode(base, cur) {
						return
					}
				}
			case *cc.UnaryExpression:
				switch x.Case {
				case cc.UnaryExpressionAddrof, cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionAlignofExpr:
					return
				}
			}
			break
		}
	}
	if !c.goPtrAutomaticRoot(n) {
		return
	}

	cur := anc[len(anc)-1]
	for i := len(anc) - 2; i >= 0; i-- {
		switch p := anc[i].(type) {
		case *cc.PrimaryExpression:
			if p.Case == cc.PrimaryExpressionExpr {
				cur = anc[i]
				continue
			}
		case *cc.ExpressionList:
			if sameNode(p.AssignmentExpression, cur) && p.ExpressionList != nil {
				return // discarded
			}

			cur = anc[i]
			continue
		case *cc.CastExpression:
			if p.Type().Kind() == cc.Ptr {
				cur = anc[i]
				continue
			}
		case *cc.AdditiveExpression:
			if pe, _, _ := c.goPtrAdditiveParts(p); sameNode(pe, cur) {
				cur = anc[i]
				continue
			}
		case *cc.UnaryExpression:
			switch p.Case {
			case cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionAlignofExpr:
				return // not evaluated
			}
		case *cc.TypeSpecifier:
			return
		case *cc.ArgumentExpressionList:
			if u := c.goPtrArgUse(g, anc, i); u.kind == goPtrFlow {
				g.rootFlows[fd] = append(g.rootFlows[fd], u.target)
				return
			}
		case *cc.AssignmentExpression:
			if p.Case == cc.AssignmentExpressionAssign && sameNode(p.AssignmentExpression, cur) {
				if d := c.goPtrIdentDecl(p.UnaryExpression); d != nil && g.cands[d] && c.goPtrValueUnused(anc, i) {
					g.rootFlows[fd] = append(g.rootFlows[fd], d)
					return
				}
			}
		case *cc.Initializer:
			if i > 0 && p.Case == cc.InitializerExpr {
				if id, ok := anc[i-1].(*cc.InitDeclarator); ok && id.Initializer == p && g.cands[id.Declarator] {
					g.rootFlows[fd] = append(g.rootFlows[fd], id.Declarator)
					return
				}
			}
		}
		break
	}
}

func (c *ctx) goPtrInitDeclarator(g *goPtrInfo, fd *cc.FunctionDefinition, n *cc.InitDeclarator) {
	d := n.Declarator
	if fd == nil || g.fnCands[fd] == nil || !c.goPtrVarCandidate(d) {
		return
	}

	g.cands[d] = true
	if n.Case != cc.InitDeclaratorInit {
		return
	}

	init := c.unbracedInitilizer(n.Initializer)
	switch {
	case init.Case != cc.InitializerExpr:
		g.uses[d] = append(g.uses[d], goPtrUse{kind: goPtrSink})
	case c.goPtrNonZeroConst(init.AssignmentExpression):
		// A sentinel like (void*)1 in a pointer-typed Go slot is an invalid
		// pointer to the runtime.
		g.uses[d] = append(g.uses[d], goPtrUse{kind: goPtrSink})
	case c.goPtrAutomaticRoot(init.AssignmentExpression):
		g.roots[d] = true
	}
}

func (c *ctx) goPtrIdent(g *goPtrInfo, anc []cc.Node) {
	n := anc[len(anc)-1].(*cc.PrimaryExpression)
	d := c.declaratorOf(n)
	if d == nil {
		return
	}

	if g.cands[d] {
		g.uses[d] = append(g.uses[d], c.goPtrClassify(g, anc))
		return
	}

	if d.Type().Kind() == cc.Function && d.Linkage() != cc.None && g.fnByName[d.Name()] != nil && !goPtrIsCallee(anc) {
		g.refs[d.Name()] = true
	}
}

// goPtrIsCallee reports whether the identifier at the top of anc is the
// callee of a direct call.
func goPtrIsCallee(anc []cc.Node) bool {
	cur := anc[len(anc)-1]
	for i := len(anc) - 2; i >= 0; i-- {
		switch x := anc[i].(type) {
		case *cc.PrimaryExpression:
			if x.Case != cc.PrimaryExpressionExpr {
				return false
			}
		case *cc.ExpressionList:
			if x.ExpressionList != nil {
				return false
			}
		case *cc.PostfixExpression:
			return x.Case == cc.PostfixExpressionCall && sameNode(x.PostfixExpression, cur)
		default:
			return false
		}
		cur = anc[i]
	}
	return false
}

func (c *ctx) goPtrCallRoots(g *goPtrInfo, n *cc.PostfixExpression) {
	d := c.declaratorOf(n.PostfixExpression)
	if d == nil || d.Type().Kind() != cc.Function || d.Linkage() == cc.None {
		return
	}

	fc := g.fnByName[d.Name()]
	if fc == nil {
		return
	}

	i := 0
	for l := n.ArgumentExpressionList; l != nil && i < len(fc.params); l, i = l.ArgumentExpressionList, i+1 {
		pd := fc.params[i].Declarator
		if !g.cands[pd] {
			continue
		}

		switch arg := l.AssignmentExpression; {
		case c.goPtrNonZeroConst(arg):
			g.uses[pd] = append(g.uses[pd], goPtrUse{kind: goPtrSink})
		case c.goPtrAutomaticRoot(arg):
			g.roots[pd] = true
		}
	}
}

func (c *ctx) goPtrAssignRoots(g *goPtrInfo, anc []cc.Node, n *cc.AssignmentExpression) {
	d := c.goPtrIdentDecl(n.UnaryExpression)
	if d == nil || !g.cands[d] {
		return
	}

	switch {
	case c.goPtrNonZeroConst(n.AssignmentExpression):
		g.uses[d] = append(g.uses[d], goPtrUse{kind: goPtrSink})
	case c.goPtrAutomaticRoot(n.AssignmentExpression) && c.goPtrValueUnused(anc, len(anc)-1):
		g.roots[d] = true
	}
}

// goPtrIdentDecl returns the declarator of n if n is an identifier, possibly
// parenthesized.
func (c *ctx) goPtrIdentDecl(n cc.ExpressionNode) *cc.Declarator {
	if x, ok := unparen(n).(*cc.PrimaryExpression); ok && x.Case == cc.PrimaryExpressionIdent {
		return c.declaratorOf(x)
	}

	return nil
}

// goPtrClassify classifies the use of the candidate variable named by the
// identifier at the top of anc.
//
// Walking up from the identifier, cur is either the pointer value derived from
// the variable, or, after a dereference, an lvalue located at an address
// derived from it (lv). Reading or writing such an lvalue is safe, taking its
// address makes a pointer value again, and an array lvalue decays into one.
//
// A pointer value that is not the bare variable, parenthesized or not, such
// as p + 1 or (T *)p, is only accepted where the emitter renders it as an
// unsafe.Pointer: dereferenced, compared, flowing into another converted
// variable, or combined further. As a condition or a discarded value only the
// bare variable is handled.
func (c *ctx) goPtrClassify(g *goPtrInfo, anc []cc.Node) goPtrUse {
	safe := goPtrUse{kind: goPtrSafe}
	sink := goPtrUse{kind: goPtrSink}
	cur := anc[len(anc)-1]
	lv := false
	bare := true
	for i := len(anc) - 2; i >= 0; i-- {
		par := anc[i]
		if lv && c.goPtrIsArrayLvalue(cur.(cc.ExpressionNode)) {
			// Decay, except where an array stays an object.
			switch x := par.(type) {
			case *cc.PostfixExpression:
				if x.Case != cc.PostfixExpressionIndex {
					lv, bare = false, false
				}
			case *cc.UnaryExpression:
				switch x.Case {
				case cc.UnaryExpressionAddrof, cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionAlignofExpr:
					// ok
				default:
					lv, bare = false, false
				}
			case *cc.PrimaryExpression, *cc.ExpressionList:
				// parentheses, decided one level up
			default:
				lv, bare = false, false
			}
		}
		switch p := par.(type) {
		case *cc.PrimaryExpression: // '(' ExpressionList ')'
			if p.Case != cc.PrimaryExpressionExpr {
				return sink
			}
		case *cc.ExpressionList:
			if sameNode(p.AssignmentExpression, cur) && p.ExpressionList != nil {
				if lv || bare {
					return safe // not the last: the value is discarded
				}

				return sink
			}

			if lv && !sameNode(p.AssignmentExpression, cur) {
				// The last of several elements, an object: ((void)1,
				// x->c).tab[0] in gcc's pr16566-1.c. Neither path of the
				// emitter addresses it.
				return sink
			}
		case *cc.CastExpression:
			if lv {
				return safe
			}

			switch p.Type().Kind() {
			case cc.Ptr:
				bare = false // still the same address
			case cc.Void:
				if bare {
					return safe
				}

				return sink
			default:
				return sink
			}
		case *cc.UnaryExpression:
			switch p.Case {
			case cc.UnaryExpressionAddrof:
				if !lv {
					return sink // the address of the variable itself
				}

				lv, bare = false, false
			case cc.UnaryExpressionDeref:
				if lv {
					return safe // the pointer stored in the lvalue is read
				}

				if c.isVolatileOrAtomicExpr(p) {
					return sink
				}

				lv = true
			case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
				if lv || bare && c.goPtrValueUnused(anc, i) {
					return safe
				}

				return sink
			case cc.UnaryExpressionSizeofExpr, cc.UnaryExpressionAlignofExpr:
				return safe
			case cc.UnaryExpressionNot:
				if lv || bare {
					return safe
				}

				return sink
			case cc.UnaryExpressionPlus, cc.UnaryExpressionMinus, cc.UnaryExpressionCpl:
				if lv {
					return safe
				}

				return sink
			default:
				return sink
			}
		case *cc.PostfixExpression:
			switch p.Case {
			case cc.PostfixExpressionIndex:
				if base, _ := c.goPtrIndexParts(p); !sameNode(base, cur) {
					if lv {
						return safe // the index is read from the lvalue
					}

					return sink
				}

				if c.isVolatileOrAtomicExpr(p) {
					return sink
				}

				lv = true
			case cc.PostfixExpressionPSelect:
				if lv {
					return safe // the pointer stored in the lvalue is read
				}

				if p.Field().IsBitfield() || c.isVolatileOrAtomicExpr(p) {
					return sink
				}

				lv = true
			case cc.PostfixExpressionSelect:
				if !lv || p.Field().IsBitfield() || c.isVolatileOrAtomicExpr(p) {
					return sink
				}
			case cc.PostfixExpressionCall:
				if lv {
					return safe // a call through the function pointer stored in the lvalue
				}

				return sink
			case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
				if lv || bare && c.goPtrValueUnused(anc, i) {
					return safe
				}

				return sink
			default:
				return sink
			}
		case *cc.ArgumentExpressionList:
			if lv {
				return safe // the value is passed
			}

			return c.goPtrArgUse(g, anc, i)
		case *cc.AdditiveExpression:
			if lv {
				return safe
			}

			x, y := p.AdditiveExpression, p.MultiplicativeExpression
			switch {
			case p.Case == cc.AdditiveExpressionAdd && sameNode(x, cur) && cc.IsIntegerType(y.Type()):
				// p + i
			case p.Case == cc.AdditiveExpressionAdd && sameNode(y, cur) && cc.IsIntegerType(x.Type()):
				// i + p
			case p.Case == cc.AdditiveExpressionSub && sameNode(x, cur) && cc.IsIntegerType(y.Type()):
				// p - i
			default:
				return sink
			}
			bare = false
		case *cc.EqualityExpression:
			if lv {
				return safe
			}

			if other := p.EqualityExpression; sameNode(other, cur) {
				other = p.RelationalExpression
				if c.goPtrNonZeroConst(other) {
					return sink // compared with a sentinel
				}
			} else if c.goPtrNonZeroConst(other) {
				return sink
			}
			return safe
		case *cc.RelationalExpression:
			if lv {
				return safe
			}

			if other := p.RelationalExpression; sameNode(other, cur) {
				other = p.ShiftExpression
				if c.goPtrNonZeroConst(other) {
					return sink
				}
			} else if c.goPtrNonZeroConst(other) {
				return sink
			}
			return safe
		case *cc.AssignmentExpression:
			if sameNode(p.UnaryExpression, cur) { // assigned to
				if lv {
					return safe
				}

				switch p.Case {
				case cc.AssignmentExpressionAssign, cc.AssignmentExpressionAdd, cc.AssignmentExpressionSub:
					if bare && c.goPtrValueUnused(anc, i) {
						return safe
					}
				}
				return sink
			}

			// the right operand
			if lv {
				return safe
			}

			if p.Case == cc.AssignmentExpressionAssign {
				if d := c.goPtrIdentDecl(p.UnaryExpression); d != nil && g.cands[d] && c.goPtrValueUnused(anc, i) {
					return goPtrUse{kind: goPtrFlow, target: d}
				}
			}
			return sink
		case *cc.ConditionalExpression:
			if lv || bare && sameNode(p.LogicalOrExpression, cur) {
				return safe
			}

			return sink
		case *cc.LogicalAndExpression, *cc.LogicalOrExpression:
			if lv || bare {
				return safe
			}

			return sink
		case *cc.MultiplicativeExpression, *cc.ShiftExpression, *cc.AndExpression, *cc.ExclusiveOrExpression, *cc.InclusiveOrExpression:
			if lv {
				return safe
			}

			return sink
		case *cc.Initializer:
			if lv {
				return safe
			}

			if i > 0 && p.Case == cc.InitializerExpr {
				if id, ok := anc[i-1].(*cc.InitDeclarator); ok && id.Initializer == p && g.cands[id.Declarator] {
					return goPtrUse{kind: goPtrFlow, target: id.Declarator}
				}
			}
			return sink
		case *cc.ExpressionStatement:
			if lv || bare && !c.goPtrInStmtExpr(anc, i) {
				return safe
			}

			return sink
		case *cc.JumpStatement:
			if lv {
				return safe
			}

			return sink
		case *cc.SelectionStatement, *cc.IterationStatement:
			if lv || bare {
				return safe
			}

			return sink
		case *cc.LabeledStatement:
			return safe
		case *cc.TypeSpecifier: // typeof
			return safe
		case *cc.GenericSelection:
			if sameNode(p.AssignmentExpression, cur) {
				return safe // the controlling expression is not evaluated
			}

			return sink
		default:
			return sink
		}
		cur = par
	}
	return sink
}

// goPtrArgUse classifies the argument at the top of anc, a child of the
// ArgumentExpressionList anc[i].
func (c *ctx) goPtrArgUse(g *goPtrInfo, anc []cc.Node, i int) goPtrUse {
	sink := goPtrUse{kind: goPtrSink}
	index := 0
	for i > 0 {
		if _, ok := anc[i-1].(*cc.ArgumentExpressionList); !ok {
			break
		}

		i--
		index++
	}
	if i == 0 {
		return sink
	}

	call, ok := anc[i-1].(*cc.PostfixExpression)
	if !ok || call.Case != cc.PostfixExpressionCall {
		return sink
	}

	d := c.declaratorOf(call.PostfixExpression)
	if d == nil || d.Type().Kind() != cc.Function || d.Linkage() == cc.None {
		return sink
	}

	fc := g.fnByName[d.Name()]
	if fc == nil || index >= len(fc.params) {
		return sink
	}

	if pd := fc.params[index].Declarator; g.cands[pd] {
		return goPtrUse{kind: goPtrFlow, target: pd}
	}

	return sink
}

// goPtrValueUnused reports whether the value of the expression anc[i] is
// discarded.
func (c *ctx) goPtrValueUnused(anc []cc.Node, i int) bool {
	for j := i - 1; j >= 0; j-- {
		cur := anc[j+1]
		switch p := anc[j].(type) {
		case *cc.PrimaryExpression:
			if p.Case != cc.PrimaryExpressionExpr {
				return false
			}
		case *cc.ExpressionList:
			if sameNode(p.AssignmentExpression, cur) && p.ExpressionList != nil {
				return true
			}
		case *cc.CastExpression:
			return p.Type().Kind() == cc.Void
		case *cc.ExpressionStatement:
			return !c.goPtrInStmtExpr(anc, j)
		case *cc.IterationStatement:
			switch p.Case {
			case cc.IterationStatementFor:
				return sameNode(p.ExpressionList, cur) || sameNode(p.ExpressionList3, cur)
			case cc.IterationStatementForDecl:
				return sameNode(p.ExpressionList2, cur)
			}
			return false
		default:
			return false
		}
	}
	return false
}

// goPtrInStmtExpr reports whether the statement anc[i] is part of a statement
// expression, whose value it may produce.
func (c *ctx) goPtrInStmtExpr(anc []cc.Node, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch x := anc[j].(type) {
		case *cc.PrimaryExpression:
			if x.Case == cc.PrimaryExpressionStmt {
				return true
			}
		case *cc.FunctionDefinition:
			return false
		}
	}
	return false
}

// goPtrIndexParts returns the pointer and the index operand of n, an index
// expression.
func (c *ctx) goPtrIndexParts(n *cc.PostfixExpression) (base, index cc.ExpressionNode) {
	if _, ok := n.PostfixExpression.Type().(*cc.PointerType); ok {
		return n.PostfixExpression, n.ExpressionList
	}

	return n.ExpressionList, n.PostfixExpression
}

// goPtrIsArrayLvalue reports whether n is an object of array type: a variable,
// a member or an element of array type, or the pointee of a pointer to an
// array. An array parameter is a pointer, and so is pointer arithmetic on
// one, even though its type still undecays to the array.
func (c *ctx) goPtrIsArrayLvalue(n cc.ExpressionNode) bool {
	if n == nil || n.Type() == nil {
		return false
	}

	switch x := unparen(n).(type) {
	case *cc.PrimaryExpression:
		if x.Case != cc.PrimaryExpressionIdent {
			return false
		}

		d := c.declaratorOf(x)
		return d != nil && !d.IsParam() && !d.IsTypename() && d.Type().Undecay().Kind() == cc.Array
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect:
			return x.Field().Type().Undecay().Kind() == cc.Array
		case cc.PostfixExpressionIndex:
			base, _ := c.goPtrIndexParts(x)
			pt, ok := base.Type().(*cc.PointerType)
			return ok && pt.Elem().Undecay().Kind() == cc.Array
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionDeref {
			pt, ok := x.CastExpression.Type().(*cc.PointerType)
			return ok && pt.Elem().Undecay().Kind() == cc.Array
		}
	}
	return false
}

// goPtrIntConst returns the value of n if it is a non-negative integer
// constant.
func (c *ctx) goPtrIntConst(n cc.ExpressionNode) (int64, bool) {
	if n == nil || !cc.IsIntegerType(n.Type()) {
		return 0, false
	}

	switch x := n.Value().(type) {
	case cc.Int64Value:
		return int64(x), x >= 0
	case cc.UInt64Value:
		return int64(x), x <= 1<<62
	}
	return 0, false
}

// goPtrNonZeroConst reports whether n is an integer constant other than zero,
// a C sentinel such as (void*)1 when used as a pointer.
func (c *ctx) goPtrNonZeroConst(n cc.ExpressionNode) bool {
	if n == nil {
		return false
	}

	switch x := n.Value().(type) {
	case cc.Int64Value:
		return x != 0
	case cc.UInt64Value:
		return x != 0
	}
	return false
}

// goPtrIsZeroConst reports whether n is a null pointer constant.
func (c *ctx) goPtrIsZeroConst(n cc.ExpressionNode) bool {
	if n == nil {
		return false
	}

	switch x := n.Value().(type) {
	case cc.Int64Value:
		return x == 0
	case cc.UInt64Value:
		return x == 0
	}
	return false
}

// goPtrAutomaticRoot reports whether n is the address of an automatic variable
// that can be a Go local, see goPtrRootDecl.
func (c *ctx) goPtrAutomaticRoot(n cc.ExpressionNode) bool {
	d, ok := c.goPtrRootDecl(n)
	return ok && d.StorageDuration() == cc.Automatic
}

// goPtrRootDecl returns the variable whose address, or the address of a part
// of which at a constant offset, n evaluates to, provided the variable is a Go
// variable: a local that is not pinned, a static or an extern. Such an address
// is produced as unsafe.Pointer(&v) without pinning v.
func (c *ctx) goPtrRootDecl(n cc.ExpressionNode) (*cc.Declarator, bool) {
	switch x := unparen(n).(type) {
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionAddrof {
			return c.goPtrRootLvalueDecl(x.CastExpression)
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast && x.Type().Kind() == cc.Ptr {
			return c.goPtrRootDecl(x.CastExpression)
		}
	case *cc.AdditiveExpression:
		// Only a constant offset: a pointer past a Go object is not valid.
		if pe, ie, _ := c.goPtrAdditiveParts(x); pe != nil && c.goPtrIsIntConst(ie) {
			return c.goPtrRootDecl(pe)
		}
	}
	if c.goPtrIsArrayLvalue(n) { // decay
		return c.goPtrRootLvalueDecl(n)
	}

	return nil, false
}

// goPtrIsIntConst reports whether n is an integer constant.
func (c *ctx) goPtrIsIntConst(n cc.ExpressionNode) bool {
	switch n.Value().(type) {
	case cc.Int64Value, cc.UInt64Value:
		return cc.IsIntegerType(n.Type())
	}
	return false
}

// goPtrRootLvalueDecl is goPtrRootDecl for the lvalue n.
func (c *ctx) goPtrRootLvalueDecl(n cc.ExpressionNode) (*cc.Declarator, bool) {
	switch x := unparen(n).(type) {
	case *cc.PrimaryExpression:
		if x.Case != cc.PrimaryExpressionIdent {
			return nil, false
		}

		d := c.declaratorOf(x)
		if d == nil || d.IsTypename() || d.Type().Kind() == cc.Function || d.IsThreadLocal() || c.goPtrVar(d) {
			return nil, false
		}

		if _, ok := c.isVLA(d.Type()); ok {
			return nil, false
		}

		if d.IsParam() && d.Type().Undecay().Kind() == cc.Array {
			return nil, false // a pointer, not an object
		}

		if c.f != nil && !c.isAddressable(x) {
			return nil, false // the parameter of an inlined function replaced by a non-addressable argument
		}

		return d, true
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionSelect:
			if x.Field().IsBitfield() {
				return nil, false
			}

			return c.goPtrRootLvalueDecl(x.PostfixExpression)
		case cc.PostfixExpressionIndex:
			// Only a constant index within the array: a pointer past a Go
			// object is not valid.
			base, index := c.goPtrIndexParts(x)
			at, ok := base.Type().Undecay().(*cc.ArrayType)
			if !ok || at.IsVLA() || at.Len() < 0 || !c.goPtrIsArrayLvalue(base) {
				return nil, false
			}

			if k, ok := c.goPtrIntConst(index); !ok || k >= at.Len() {
				return nil, false
			}

			return c.goPtrRootLvalueDecl(base)
		}
	}
	return nil, false
}

// goPtrAdditiveParts returns the pointer and the integer operand of n, a
// pointer plus or minus an integer, and whether the integer is subtracted.
func (c *ctx) goPtrAdditiveParts(n *cc.AdditiveExpression) (pe, ie cc.ExpressionNode, neg bool) {
	x, y := n.AdditiveExpression, n.MultiplicativeExpression
	if x == nil || y == nil {
		return nil, nil, false
	}

	switch n.Case {
	case cc.AdditiveExpressionAdd:
		switch {
		case x.Type().Kind() == cc.Ptr && cc.IsIntegerType(y.Type()):
			return x, y, false
		case cc.IsIntegerType(x.Type()) && y.Type().Kind() == cc.Ptr:
			return y, x, false
		}
	case cc.AdditiveExpressionSub:
		if x.Type().Kind() == cc.Ptr && cc.IsIntegerType(y.Type()) {
			return x, y, true
		}
	}
	return nil, nil, false
}

// isGoPtrVar reports whether n is an identifier, possibly parenthesized, of a
// converted variable.
func (c *ctx) isGoPtrVar(n cc.ExpressionNode) bool { return c.goPtrVar(c.goPtrIdentDecl(n)) }

// isGoPtrExpr reports whether n is a pointer value derived from a converted
// variable, so that it is an unsafe.Pointer in the generated code.
func (c *ctx) isGoPtrExpr(n cc.ExpressionNode) bool {
	if c.goPtr == nil || n == nil {
		return false
	}

	switch x := unparen(n).(type) {
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionIdent {
			return c.goPtrVar(c.declaratorOf(x))
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast && x.Type().Kind() == cc.Ptr {
			return c.isGoPtrExpr(x.CastExpression)
		}
	case *cc.AdditiveExpression:
		if pe, _, _ := c.goPtrAdditiveParts(x); pe != nil {
			return c.isGoPtrExpr(pe)
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionAddrof {
			return c.isGoPtrLvalue(x.CastExpression)
		}
	case *cc.ExpressionList:
		for x.ExpressionList != nil {
			x = x.ExpressionList
		}
		return c.isGoPtrExpr(x.AssignmentExpression)
	}
	if c.goPtrIsArrayLvalue(n) { // decay
		return c.isGoPtrLvalue(n)
	}

	return false
}

// isGoPtrLvalue reports whether n is an object located at an address derived
// from a converted variable, so that &n is an unsafe.Pointer in the generated
// code.
func (c *ctx) isGoPtrLvalue(n cc.ExpressionNode) bool {
	if c.goPtr == nil || n == nil || c.isVolatileOrAtomicExpr(n) {
		return false
	}

	switch x := unparen(n).(type) {
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionDeref {
			return c.isGoPtrExpr(x.CastExpression)
		}
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionPSelect:
			return !x.Field().IsBitfield() && c.isGoPtrExpr(x.PostfixExpression)
		case cc.PostfixExpressionSelect:
			return !x.Field().IsBitfield() && c.isGoPtrLvalue(x.PostfixExpression)
		case cc.PostfixExpressionIndex:
			base, _ := c.goPtrIndexParts(x)
			if c.goPtrIsArrayLvalue(base) {
				return c.isGoPtrLvalue(base)
			}

			return c.isGoPtrExpr(base)
		}
	}
	return false
}

// goPtrAdd returns base, an unsafe.Pointer, advanced by off bytes.
func goPtrAdd(base string, off int64) string {
	if off == 0 {
		return base
	}

	return unsafe("Add", fmt.Sprintf("%s, %d", base, off))
}

// goPtrStr returns the pointer value n as an unsafe.Pointer.
func (c *ctx) goPtrStr(w writer, n cc.ExpressionNode) string {
	return fmt.Sprintf("%s", c.expr(w, n, nil, exprUnsafePointer))
}

// goPtrExpr emits n, a C pointer value, as a Go unsafe.Pointer: the mode
// exprUnsafePointer. A value derived from a converted variable or the address
// of a Go variable is produced as such, anything else by converting the
// uintptr.
func (c *ctx) goPtrExpr(w writer, n cc.ExpressionNode, t cc.Type) (r *buf, rt cc.Type, rmode mode) {
	var b buf
	rt, rmode = t, exprUnsafePointer
	if rt == nil || rt.Kind() != cc.Ptr {
		rt = n.Type()
	}
	if rt.Kind() != cc.Ptr {
		rt = c.pvoid
	}
	switch x := unparen(n).(type) {
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionIdent && c.goPtrVar(c.declaratorOf(x)) {
			return c.primaryExpression(w, x, t, exprUnsafePointer)
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionAddrof {
			if s, ok := c.goPtrAddrOf(w, x.CastExpression); ok {
				b.w("%s", s)
				return &b, rt, rmode
			}
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast && x.Type().Kind() == cc.Ptr && (c.isGoPtrExpr(x.CastExpression) || c.goPtrIsRoot(x.CastExpression)) {
			r, _, _ = c.goPtrExpr(w, x.CastExpression, nil)
			return r, rt, rmode
		}
	case *cc.AdditiveExpression:
		if pe, ie, neg := c.goPtrAdditiveParts(x); pe != nil && (c.isGoPtrExpr(pe) || c.goPtrIsRoot(pe)) {
			p := c.goPtrStr(w, pe)
			b.w("%s", unsafe("Add", fmt.Sprintf("%s, %s", p, c.goPtrOffset(w, pe, ie, neg))))
			return &b, rt, rmode
		}
	case *cc.ExpressionList:
		for ; x.ExpressionList != nil; x = x.ExpressionList {
			w.w("%s%s;", sep(x.AssignmentExpression), c.discardStr2(x.AssignmentExpression, c.topExpr(w, x.AssignmentExpression, nil, exprVoid)))
		}
		r, _, _ = c.goPtrExpr(w, x.AssignmentExpression, nil)
		return r, rt, rmode
	}
	if c.goPtrIsArrayLvalue(n) { // decay
		if s, ok := c.goPtrAddrOf(w, n); ok {
			b.w("%s", s)
			return &b, rt, rmode
		}
	}

	if c.goPtrIsZeroConst(n) {
		b.w("(%snil)", tag(preserve))
		return &b, rt, rmode
	}

	e := c.checkVolatileExpr(w, n, rt, exprDefault)
	b.w("%s", unsafePointer(e))
	b.volatileOrAtomicHandled = e.volatileOrAtomicHandled
	return &b, rt, rmode
}

// goPtrOffset returns the byte offset of pe plus or minus ie elements, for
// unsafe.Add: a literal for a constant ie, since negating a typed unsigned
// constant does not compile, and the uintptr arithmetic of a pointer plus
// integer otherwise.
func (c *ctx) goPtrOffset(w writer, pe, ie cc.ExpressionNode, neg bool) string {
	sz := pe.Type().(*cc.PointerType).Elem().Undecay().Size()
	if sz > 0 {
		var k int64
		ok := false
		switch x := ie.Value().(type) {
		case cc.Int64Value:
			k, ok = int64(x), true
		case cc.UInt64Value:
			k, ok = int64(x), x <= 1<<62
		}
		if ok {
			if neg {
				k = -k
			}
			return fmt.Sprint(k * sz)
		}
	}

	off := fmt.Sprintf("(%s)%s", c.topExpr(w, ie, c.pvoid, exprDefault), c.elemSize(pe, "*"))
	if neg {
		off = "-" + off
	}
	return off
}

func (c *ctx) goPtrIsRoot(n cc.ExpressionNode) bool {
	_, ok := c.goPtrRootDecl(n)
	return ok
}

// goPtrAddrOf returns the address of the lvalue n as an unsafe.Pointer, if n
// is located at an address derived from a converted variable or is a Go
// variable or a part of one.
func (c *ctx) goPtrAddrOf(w writer, n cc.ExpressionNode) (string, bool) {
	if c.isGoPtrLvalue(n) {
		base, off := c.goPtrAddr(w, n)
		return goPtrAdd(base, off), true
	}

	if d, ok := c.goPtrRootLvalueDecl(n); ok {
		base, off := c.goPtrRootAddr(w, n, d)
		return goPtrAdd(base, off), true
	}

	return "", false
}

// goPtrAddr returns the address of the lvalue n, for which isGoPtrLvalue
// holds, as an unsafe.Pointer base and a constant offset.
func (c *ctx) goPtrAddr(w writer, n cc.ExpressionNode) (base string, off int64) {
	switch x := unparen(n).(type) {
	case *cc.UnaryExpression: // '*' CastExpression
		return c.goPtrStr(w, x.CastExpression), 0
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionPSelect:
			return c.goPtrStr(w, x.PostfixExpression), x.Field().Offset()
		case cc.PostfixExpressionSelect:
			base, off = c.goPtrAddr(w, x.PostfixExpression)
			return base, off + x.Field().Offset()
		case cc.PostfixExpressionIndex:
			b, index := c.goPtrIndexParts(x)
			switch {
			case c.goPtrIsArrayLvalue(b):
				base, off = c.goPtrAddr(w, b)
			default:
				base = c.goPtrStr(w, b)
			}
			if k, ok := c.goPtrIntConst(index); ok {
				return base, off + k*b.Type().(*cc.PointerType).Elem().Undecay().Size()
			}

			return unsafe("Add", fmt.Sprintf("%s, (%s)%s", goPtrAdd(base, off), c.topExpr(w, index, c.pvoid, exprDefault), c.mul(b))), 0
		}
	}
	c.err(errorf("%v: internal error: %q is not a Go pointer lvalue", pos(n), cc.NodeSource(n)))
	return "", 0
}

// goPtrRootAddr is goPtrAddr for the lvalue n rooted at the Go variable d. A
// variable pinned by another use is addressed through its frame slot.
func (c *ctx) goPtrRootAddr(w writer, n cc.ExpressionNode, d *cc.Declarator) (base string, off int64) {
	ident, off := c.goPtrRootParts(n)
	if c.f != nil && !c.isInlineParam(d) {
		if info := c.f.declInfos[d]; info != nil && info.pinned() {
			return unsafeFromUintptr(bpOff(info.bpOff + off)), 0
		}
	}

	return unsafeAddr(c.expr(w, ident, nil, exprLvalue)), off
}

// goPtrRootParts returns the identifier the lvalue n is rooted at and the
// constant offset of n within the variable, see goPtrRootLvalueDecl.
func (c *ctx) goPtrRootParts(n cc.ExpressionNode) (ident cc.ExpressionNode, off int64) {
	switch x := unparen(n).(type) {
	case *cc.PrimaryExpression:
		return x, 0
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionSelect:
			ident, off = c.goPtrRootParts(x.PostfixExpression)
			return ident, off + x.Field().Offset()
		case cc.PostfixExpressionIndex:
			b, index := c.goPtrIndexParts(x)
			ident, off = c.goPtrRootParts(b)
			k, _ := c.goPtrIntConst(index)
			return ident, off + k*b.Type().(*cc.PointerType).Elem().Undecay().Size()
		}
	}
	c.err(errorf("%v: internal error: %q is not a Go variable", pos(n), cc.NodeSource(n)))
	return n, 0
}

// isInlineParam reports whether d is a parameter of a function being inlined.
func (c *ctx) isInlineParam(d *cc.Declarator) bool {
	if c.f == nil {
		return false
	}

	for nfo := c.f.inlineInfo; nfo != nil; nfo = nfo.parent {
		for _, v := range nfo.params {
			if v.Declarator == d {
				return true
			}
		}
	}
	return false
}

// goPtrLvalue emits n in mode, if n is an object located at an address derived
// from a converted variable: a dereference of the address with the type of n.
func (c *ctx) goPtrLvalue(w writer, n cc.ExpressionNode, mode mode) (r *buf, rt cc.Type, rmode mode, ok bool) {
	if !c.isGoPtrLvalue(n) {
		return nil, nil, 0, false
	}

	var b buf
	base, off := c.goPtrAddr(w, n)
	addr := goPtrAdd(base, off)
	switch mode {
	case exprDefault, exprLvalue, exprSelect, exprIndex, exprVoid:
		if mode == exprVoid {
			mode = exprDefault
		}
		b.w("(*(*%s)(%s))", c.typ(n, n.Type().Undecay()), addr)
		return &b, n.Type(), mode, true
	case exprCall:
		b.w("(*(*%suintptr)(%s))", tag(preserve), addr)
		return &b, n.Type(), exprUintptr, true
	}

	c.err(errorf("%v: internal error: Go pointer lvalue %q in mode %v", pos(n), cc.NodeSource(n), mode))
	return &b, n.Type(), mode, true
}

// goPtrLvalueTemp emits the address of the lvalue n into a fresh variable and
// returns the dereference of that variable, for an lvalue that is used more
// than once.
func (c *ctx) goPtrLvalueTemp(w writer, n cc.ExpressionNode, t cc.Type) (deref string, ok bool) {
	if !c.isGoPtrLvalue(n) {
		return "", false
	}

	p := c.f.newAutovarTyp(n, goPtrType())
	base, off := c.goPtrAddr(w, n)
	w.w("\n%s = %s;", p, goPtrAdd(base, off))
	return fmt.Sprintf("(*(*%s)(%s))", c.typ(n, t), p), true
}

// goPtrCompare emits the comparison x op y when at least one operand is a Go
// pointer: Go pointers compare with each other and with nil, otherwise the Go
// pointer is converted to uintptr for the comparison, which is allowed, and
// compared with the other operand rendered as uintptr.
func (c *ctx) goPtrCompare(w writer, op string, x, y cc.ExpressionNode, ct cc.Type) (r *buf, ok bool) {
	gx, gy := c.isGoPtrExpr(x), c.isGoPtrExpr(y)
	if !gx && !gy {
		return nil, false
	}

	var b buf
	eq := op == "==" || op == "!="
	side := func(n cc.ExpressionNode, isGoPtr, otherIsGoPtr bool) string {
		switch {
		case isGoPtr && eq && (otherIsGoPtr || c.goPtrIsZeroConst(x) || c.goPtrIsZeroConst(y)):
			return c.goPtrStr(w, n)
		case isGoPtr:
			return fmt.Sprintf("%suintptr(%s)", tag(preserve), c.goPtrStr(w, n))
		case eq && c.goPtrIsZeroConst(n):
			return fmt.Sprintf("%snil", tag(preserve))
		default:
			return fmt.Sprintf("%s", c.checkVolatileExpr(w, n, ct, exprDefault))
		}
	}
	b.w("(%s %s %s)", side(x, gx, gy), op, side(y, gy, gx))
	return &b, true
}

// goPtrIncDec emits n, a converted variable, incremented or decremented by
// delta elements.
func (c *ctx) goPtrIncDec(n cc.ExpressionNode, delta int64) string {
	nm := c.goPtrName(n)
	sz := n.Type().(*cc.PointerType).Elem().Undecay().Size()
	return fmt.Sprintf("%s = %s", nm, unsafe("Add", fmt.Sprintf("%s, %d", nm, delta*sz)))
}

// goPtrName returns the Go name of n, an identifier of a converted variable.
func (c *ctx) goPtrName(n cc.ExpressionNode) string {
	d := c.goPtrIdentDecl(n)
	if c.pass == 2 {
		if nm := c.f.locals[d]; nm != "" {
			return nm
		}
	}
	return c.declaratorTag(d) + d.Name()
}

// goPtrWrapper emits a function with the original name and uintptr signature
// of d that calls the body emitted under fn.linkName. The parameters keep
// their names, which carry the function pointer annotation, see
// ccgoFuncParam.
func (c *ctx) goPtrWrapper(w writer, d *cc.Declarator, ft *cc.FunctionType, fn *goPtrFn) {
	params := c.normalizeParams(ft.Parameters())
	var names []string
	for i, v := range params {
		nm := c.f.locals[v.Declarator]
		if nm == "" {
			nm = fmt.Sprintf("%sp%d", tag(ccgo), i)
		}
		names = append(names, nm)
	}
	w.w("\n\nfunc %s%s(%stls *%s%sTLS", c.declaratorTag(d), d.Name(), tag(ccgo), c.task.tlsQualifier, tag(preserve))
	for i, v := range params {
		w.w(", %s %s", names[i], c.typ2(v, v.Type().Decay(), true))
	}
	if ft.IsVariadic() {
		w.w(", %s%s %suintptr", tag(ccgo), vaArgName, tag(preserve))
	}
	w.w(") ")
	if ft.Result().Kind() != cc.Void {
		w.w("%s ", c.typ2(nil, ft.Result(), true))
	}
	w.w("{\n")
	if ft.Result().Kind() != cc.Void {
		w.w("return ")
	}
	w.w("%s(%stls", fn.linkName, tag(ccgo))
	for i := range params {
		arg := names[i]
		if fn.converted[i] {
			arg = unsafePointer(arg)
		}
		w.w(", %s", arg)
	}
	if ft.IsVariadic() {
		w.w(", %s%s", tag(ccgo), vaArgName)
	}
	w.w(")\n}\n\n")
}
