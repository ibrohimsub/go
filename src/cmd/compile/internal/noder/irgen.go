// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"os"

	"cmd/compile/internal/base"
	"cmd/compile/internal/dwarfgen"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
	"cmd/internal/src"
)

// checkFiles configures and runs the types2 checker on the given
// parsed source files and then returns the result.
func checkFiles(noders []*noder) (posMap, *types2.Package, *types2.Info) {
	if base.SyntaxErrors() != 0 {
		base.ErrorExit()
	}

	// setup and syntax error reporting
	var m posMap
	files := make([]*syntax.File, len(noders))
	for i, p := range noders {
		m.join(&p.posMap)
		files[i] = p.file
	}

	// typechecking
	importer := gcimports{
		packages: make(map[string]*types2.Package),
	}
	conf := types2.Config{
		GoVersion:             base.Flag.Lang,
		IgnoreLabels:          true, // parser already checked via syntax.CheckBranches mode
		CompilerErrorMessages: true, // use error strings matching existing compiler errors
		AllowTypeLists:        true, // remove this line once all tests use type set syntax
		Error: func(err error) {
			terr := err.(types2.Error)
			base.ErrorfAt(m.makeXPos(terr.Pos), "%s", terr.Msg)
		},
		Importer: &importer,
		Sizes:    &gcSizes{},
	}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Implicits:  make(map[syntax.Node]types2.Object),
		Scopes:     make(map[syntax.Node]*types2.Scope),
		Inferred:   make(map[syntax.Expr]types2.Inferred),
		// expand as needed
	}

	pkg := types2.NewPackage(base.Ctxt.Pkgpath, "")
	importer.check = types2.NewChecker(&conf, pkg, info)
	err := importer.check.Files(files)

	base.ExitIfErrors()
	if err != nil {
		base.FatalfAt(src.NoXPos, "conf.Check error: %v", err)
	}

	return m, pkg, info
}

// check2 type checks a Go package using types2, and then generates IR
// using the results.
func check2(noders []*noder) {
	m, pkg, info := checkFiles(noders)

	if base.Flag.G < 2 {
		os.Exit(0)
	}

	g := irgen{
		target: typecheck.Target,
		self:   pkg,
		info:   info,
		posMap: m,
		objs:   make(map[types2.Object]*ir.Name),
		typs:   make(map[types2.Type]*types.Type),
	}
	g.generate(noders)

	if base.Flag.G < 3 {
		os.Exit(0)
	}
}

// gfInfo is information gathered on a generic function.
type gfInfo struct {
	tparams      []*types.Type
	derivedTypes []*types.Type
	// Nodes in generic function that requires a subdictionary. Includes
	// method and function calls (OCALL), function values (OFUNCINST), method
	// values/expressions (OXDOT).
	subDictCalls []ir.Node
}

// instInfo is information gathered on an gcshape (or fully concrete)
// instantiation of a function.
type instInfo struct {
	fun       *ir.Func // The instantiated function (with body)
	dictParam *ir.Name // The node inside fun that refers to the dictionary param

	gf     *ir.Name // The associated generic function
	gfInfo *gfInfo

	startSubDict int // Start of dict entries for subdictionaries
	dictLen      int // Total number of entries in dictionary

	// Map from nodes in instantiated fun (OCALL, OCALLMETHOD, OFUNCINST, and
	// OMETHEXPR) to the associated dictionary entry for a sub-dictionary
	dictEntryMap map[ir.Node]int
}

type irgen struct {
	target *ir.Package
	self   *types2.Package
	info   *types2.Info

	posMap
	objs   map[types2.Object]*ir.Name
	typs   map[types2.Type]*types.Type
	marker dwarfgen.ScopeMarker

	// Fully-instantiated generic types whose methods should be instantiated
	instTypeList []*types.Type

	dnum int // for generating unique dictionary variables

	// Map from generic function to information about its type params, derived
	// types, and subdictionaries.
	gfInfoMap map[*types.Sym]*gfInfo

	// Map from a name of function that been instantiated to information about
	// its instantiated function, associated generic function/method, and the
	// mapping from IR nodes to dictionary entries.
	instInfoMap map[*types.Sym]*instInfo
}

func (g *irgen) generate(noders []*noder) {
	types.LocalPkg.Name = g.self.Name()
	types.LocalPkg.Height = g.self.Height()
	typecheck.TypecheckAllowed = true

	// Prevent size calculations until we set the underlying type
	// for all package-block defined types.
	types.DeferCheckSize()

	// At this point, types2 has already handled name resolution and
	// type checking. We just need to map from its object and type
	// representations to those currently used by the rest of the
	// compiler. This happens mostly in 3 passes.

	// 1. Process all import declarations. We use the compiler's own
	// importer for this, rather than types2's gcimporter-derived one,
	// to handle extensions and inline function bodies correctly.
	//
	// Also, we need to do this in a separate pass, because mappings are
	// instantiated on demand. If we interleaved processing import
	// declarations with other declarations, it's likely we'd end up
	// wanting to map an object/type from another source file, but not
	// yet have the import data it relies on.
	declLists := make([][]syntax.Decl, len(noders))
Outer:
	for i, p := range noders {
		g.pragmaFlags(p.file.Pragma, ir.GoBuildPragma)
		for j, decl := range p.file.DeclList {
			switch decl := decl.(type) {
			case *syntax.ImportDecl:
				g.importDecl(p, decl)
			default:
				declLists[i] = p.file.DeclList[j:]
				continue Outer // no more ImportDecls
			}
		}
	}

	// 2. Process all package-block type declarations. As with imports,
	// we need to make sure all types are properly instantiated before
	// trying to map any expressions that utilize them. In particular,
	// we need to make sure type pragmas are already known (see comment
	// in irgen.typeDecl).
	//
	// We could perhaps instead defer processing of package-block
	// variable initializers and function bodies, like noder does, but
	// special-casing just package-block type declarations minimizes the
	// differences between processing package-block and function-scoped
	// declarations.
	for _, declList := range declLists {
		for _, decl := range declList {
			switch decl := decl.(type) {
			case *syntax.TypeDecl:
				g.typeDecl((*ir.Nodes)(&g.target.Decls), decl)
			}
		}
	}
	types.ResumeCheckSize()

	// 3. Process all remaining declarations.
	for _, declList := range declLists {
		g.target.Decls = append(g.target.Decls, g.decls(declList)...)
	}

	if base.Flag.W > 1 {
		for _, n := range g.target.Decls {
			s := fmt.Sprintf("\nafter noder2 %v", n)
			ir.Dump(s, n)
		}
	}

	typecheck.DeclareUniverse()

	for _, p := range noders {
		// Process linkname and cgo pragmas.
		p.processPragmas()

		// Double check for any type-checking inconsistencies. This can be
		// removed once we're confident in IR generation results.
		syntax.Crawl(p.file, func(n syntax.Node) bool {
			g.validate(n)
			return false
		})
	}

	// Create any needed stencils of generic functions
	g.stencil()

	// Remove all generic functions from g.target.Decl, since they have been
	// used for stenciling, but don't compile. Generic functions will already
	// have been marked for export as appropriate.
	j := 0
	for i, decl := range g.target.Decls {
		if decl.Op() != ir.ODCLFUNC || !decl.Type().HasTParam() {
			g.target.Decls[j] = g.target.Decls[i]
			j++
		}
	}
	g.target.Decls = g.target.Decls[:j]
}

func (g *irgen) unhandled(what string, p poser) {
	base.FatalfAt(g.pos(p), "unhandled %s: %T", what, p)
	panic("unreachable")
}
