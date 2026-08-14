// Whitebox: the scenarios in this file need direct access to unexported
// pieces of constswap.go — yieldDeclConsts (an *ast.GenDecl shape go/parser
// itself never produces, since a const( ... ) block always yields
// *ast.ValueSpec specs, each of whose names always resolves through
// info.Defs to a real *types.Const — verified empirically: even the blank
// identifier does, see constswap_test.go's "blank identifier in const block
// is an ordinary sibling"), fileOf, and localConstSwap.candidatesFor (a
// synthetic *ast.Ident position no real parse ever produces) — that a
// blackbox test driven only through the registry and Applies/Mutate has no
// way to reach. constswap_test.go covers everything reachable through that
// exported surface; this file covers the handful of defensive branches that
// require constructing inputs no real parse/type-check pass would ever hand
// the operator.
package identifier

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// TestYieldDeclConstsSkipsNonValueSpec covers the spec-type guard inside
// yieldDeclConsts: every const( ... ) block go/parser produces holds only
// *ast.ValueSpec specs, so this shape is unreachable through real parsed
// source. It exists purely so this function does not assume its *ast.GenDecl
// argument came from go/parser specifically — a defensive guard worth
// locking in directly since no blackbox scenario can ever exercise it.
func TestYieldDeclConstsSkipsNonValueSpec(t *testing.T) {
	t.Parallel()

	gen := &ast.GenDecl{
		Tok: token.CONST,
		// An *ast.ImportSpec standing in for a spec kind that is not
		// *ast.ValueSpec — never produced by go/parser for a CONST decl, but
		// yieldDeclConsts must skip it rather than assume the type
		// assertion always succeeds.
		Specs: []ast.Spec{&ast.ImportSpec{}},
	}

	called := false

	yieldDeclConsts(nil, nil, gen, func(*ast.File, *ast.GenDecl, *types.Const) {
		called = true
	})

	if called {
		t.Error("yieldDeclConsts called yield for a non-ValueSpec spec, want skipped")
	}
}

// TestYieldDeclConstsSkipsUnresolvedName covers yieldDeclConsts' other guard:
// a ValueSpec name with no *types.Const behind it in info.Defs. Verified
// empirically (see constswap_test.go's blank-identifier case) that this does
// not happen for any name go/types actually resolves, including "_" — so,
// like TestYieldDeclConstsSkipsNonValueSpec above, this constructs the
// otherwise-unreachable input directly: an *ast.Ident absent from info.Defs
// entirely.
func TestYieldDeclConstsSkipsUnresolvedName(t *testing.T) {
	t.Parallel()

	name := ast.NewIdent("X")
	spec := &ast.ValueSpec{Names: []*ast.Ident{name}}
	gen := &ast.GenDecl{Tok: token.CONST, Specs: []ast.Spec{spec}}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}

	called := false

	yieldDeclConsts(info, nil, gen, func(*ast.File, *ast.GenDecl, *types.Const) {
		called = true
	})

	if called {
		t.Error("yieldDeclConsts called yield for a name absent from info.Defs, want skipped")
	}
}

// TestFileOfReturnsNilForUnknownPosition covers fileOf's final "not found"
// return: a position that falls outside every *ast.File reachable through
// info.Scopes.
func TestFileOfReturnsNilForUnknownPosition(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "snippet.go", "package p\n", 0)
	if err != nil {
		t.Fatalf("parsing snippet: %v", err)
	}

	info := &types.Info{Scopes: map[ast.Node]*types.Scope{file: types.NewScope(nil, file.Pos(), file.End(), "")}}

	// Far beyond the tiny snippet's own [Pos, End) range, and unrelated to
	// fset's own bookkeeping — any position not inside the one known file's
	// range demonstrates the "not found" path.
	unknown := token.Pos(1 << 30)

	if got := fileOf(info, unknown); got != nil {
		t.Errorf("fileOf(unknown) = %v, want nil", got)
	}
}

// typeCheckLocalVar type-checks a single-file package containing a function
// with a local variable declaration, returning the resulting *types.Info,
// *types.Package, the parsed *ast.File, and the *types.Var the local
// declaration resolves to. It is a minimal, single-purpose stand-in for
// constswap_test.go's typeCheckFunc (unavailable here: that helper lives in
// package identifier_test, not this whitebox file's package identifier).
func typeCheckLocalVar(t *testing.T, src, varName string) (*types.Info, *types.Package, *ast.File, *types.Var) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parsing snippet: %v", err)
	}

	info := &types.Info{
		Types:  make(map[ast.Expr]types.TypeAndValue),
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
	}

	conf := types.Config{Importer: importer.Default()}

	pkg, err := conf.Check("p", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("type-checking snippet: %v", err)
	}

	var v *types.Var

	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || ident.Name != varName {
			return true
		}

		if obj, isUse := info.Uses[ident].(*types.Var); isUse {
			v = obj
		}

		return true
	})

	if v == nil {
		t.Fatalf("no use of local variable %q found in snippet", varName)
	}

	return info, pkg, file, v
}

// TestCandidatesForReturnsNilWhenFileUnresolvable covers candidatesFor's
// file-not-found guard: fileOf returning nil for a local variable's
// identifier. This cannot happen for any *ast.Ident go/parser itself
// produced — its position always falls inside the file info.Scopes recorded
// it under — so this test constructs a synthetic *ast.Ident, positioned
// outside every known file, and registers it in info.Uses pointing at a real
// local *types.Var obtained from an actually type-checked snippet. That
// mirrors exactly what candidatesFor sees: a real, eligible local variable
// object, reached through an identifier whose position resolves to no known
// file — the "should not happen" case candidatesFor's own doc comment
// describes, handled by returning nil rather than panicking.
func TestCandidatesForReturnsNilWhenFileUnresolvable(t *testing.T) {
	t.Parallel()

	const src = `package p

const Max = 100

func F() bool {
	var x int

	return x > 5
}
`

	info, pkg, _, v := typeCheckLocalVar(t, src, "x")

	bound := (&localConstSwap{}).WithScope(info, pkg)

	l, ok := bound.(*localConstSwap)
	if !ok {
		t.Fatalf("WithScope returned %T, want *localConstSwap", bound)
	}

	// A synthetic identifier, never produced by go/parser, whose position is
	// far outside every file info.Scopes actually knows about.
	fake := &ast.Ident{NamePos: token.Pos(1 << 30), Name: "x"}
	info.Uses[fake] = v

	if got := l.candidatesFor(fake); got != nil {
		t.Errorf("candidatesFor(fake) = %v, want nil", got)
	}
}
