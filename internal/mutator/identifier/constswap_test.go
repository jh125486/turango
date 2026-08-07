package identifier

import (
	"bytes"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
)

// typeCheckFunc parses and type-checks src as a small, self-contained
// package. Unlike internal/mutator/operator/operator_test.go's parseFunc —
// whose own doc comment says snippets "are never type-checked, so they need
// only parse" — this operator's Applies/Mutate consult go/types information,
// so its tests need a real *types.Info and *types.Package to bind
// [constSwap.WithScope] against. go/types.Config.Check is used directly,
// rather than golang.org/x/tools/go/packages, so the tests stay fast and
// need no module or toolchain shell-out.
func typeCheckFunc(t *testing.T, src string) (*token.FileSet, *ast.File, *types.Info, *types.Package) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "snippet.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
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
		t.Fatalf("type-checking %q: %v", src, err)
	}

	return fset, file, info, pkg
}

// render prints node back to source, mirroring
// internal/mutator/operator/operator_test.go's helper of the same name.
func render(t *testing.T, fset *token.FileSet, node ast.Node) string {
	t.Helper()

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		t.Fatalf("printing node: %v", err)
	}

	return buf.String()
}

// findIdent returns the sole *ast.Ident named want that is a *use* (per
// info.Uses, not info.Defs) in file.
func findIdent(t *testing.T, file *ast.File, info *types.Info, want string) *ast.Ident {
	t.Helper()

	var found []*ast.Ident

	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || ident.Name != want {
			return true
		}

		if _, isUse := info.Uses[ident]; isUse {
			found = append(found, ident)
		}

		return true
	})

	if len(found) != 1 {
		t.Fatalf("want exactly one use of %q in snippet, got %d", want, len(found))
	}

	return found[0]
}

// TestRegisteredName pins the exact registry name, since it is the string a
// user types into -mutateoperators.
func TestRegisteredName(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(ConstSwapName)
	if err != nil {
		t.Fatalf("New(%q): %v", ConstSwapName, err)
	}

	if m.Name() != ConstSwapName {
		t.Errorf("Name() = %q, want %q", m.Name(), ConstSwapName)
	}
}

// TestUnboundInstanceIsInert covers the zero-value contract: the
// registry-held instance has no type information and must never report a
// match.
func TestUnboundInstanceIsInert(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(ConstSwapName)
	if err != nil {
		t.Fatalf("New(%q): %v", ConstSwapName, err)
	}

	_, file, _, _ := typeCheckFunc(t, "package p\n\nconst (\n\tA = 1\n\tB = 2\n)\n\nfunc F() int { return A }\n")

	ast.Inspect(file, func(node ast.Node) bool {
		if m.Applies(node) {
			t.Errorf("unbound instance Applies(%T) = true, want false", node)
		}

		if muts := m.Mutate(node); muts != nil {
			t.Errorf("unbound instance Mutate(%T) = %v, want nil", node, muts)
		}

		return true
	})
}

// TestSameBlockOffersBothDirections is the core positive case: two same-type
// constants in one const( ... ) block must offer to swap into each other, in
// both directions.
func TestSameBlockOffersBothDirections(t *testing.T) {
	t.Parallel()

	src := `package p

const (
	A = 1
	B = 2
)

func F() int { return A }
func G() int { return B }
`

	fset, file, info, pkg := typeCheckFunc(t, src)

	bound := (&constSwap{}).WithScope(info, pkg)

	useA := findIdent(t, file, info, "A")
	if !bound.Applies(useA) {
		t.Fatal("Applies(A) = false, want true")
	}

	mutsA := bound.Mutate(useA)
	if len(mutsA) != 1 {
		t.Fatalf("Mutate(A) returned %d mutations, want 1", len(mutsA))
	}

	if got, want := mutsA[0].Description, "A -> B"; got != want {
		t.Errorf("Mutate(A) description = %q, want %q", got, want)
	}

	before := render(t, fset, useA)
	mutsA[0].Apply()

	if got := render(t, fset, useA); got != "B" {
		t.Errorf("after Apply: got %q, want %q", got, "B")
	}

	mutsA[0].Revert()

	if got := render(t, fset, useA); got != before {
		t.Errorf("after Revert: got %q, want %q", got, before)
	}

	useB := findIdent(t, file, info, "B")
	if !bound.Applies(useB) {
		t.Fatal("Applies(B) = false, want true")
	}

	mutsB := bound.Mutate(useB)
	if len(mutsB) != 1 {
		t.Fatalf("Mutate(B) returned %d mutations, want 1", len(mutsB))
	}

	if got, want := mutsB[0].Description, "B -> A"; got != want {
		t.Errorf("Mutate(B) description = %q, want %q", got, want)
	}
}

// TestDifferentTypesNoSwap covers the exact-type-match rule: two package-level
// constants in the same block whose types differ must not be offered as swaps
// for each other.
func TestDifferentTypesNoSwap(t *testing.T) {
	t.Parallel()

	src := `package p

const (
	N int    = 1
	S string = "x"
)

func F() int { return N }
`

	_, file, info, pkg := typeCheckFunc(t, src)

	bound := (&constSwap{}).WithScope(info, pkg)

	useN := findIdent(t, file, info, "N")
	if bound.Applies(useN) {
		t.Error("Applies(N) = true, want false: S has a different type")
	}

	if muts := bound.Mutate(useN); muts != nil {
		t.Errorf("Mutate(N) = %v, want nil", muts)
	}
}

// TestDifferentBlocksNoSwap is the scope-restriction regression test: two
// same-type constants declared in different const( ... ) blocks of the same
// file must not be offered as swaps for each other, even though they satisfy
// every other condition. This is the easiest way for the operator to
// silently regress — dropping the block check would still pass every other
// test here.
func TestDifferentBlocksNoSwap(t *testing.T) {
	t.Parallel()

	src := `package p

const (
	A = 1
)

const (
	B = 2
)

func F() int { return A }
`

	_, file, info, pkg := typeCheckFunc(t, src)

	bound := (&constSwap{}).WithScope(info, pkg)

	useA := findIdent(t, file, info, "A")
	if bound.Applies(useA) {
		t.Error("Applies(A) = true, want false: A and B are in different blocks")
	}

	if muts := bound.Mutate(useA); muts != nil {
		t.Errorf("Mutate(A) = %v, want nil", muts)
	}
}

// TestDifferentFilesNoSwap extends the same regression coverage across files:
// two same-type, standalone (non-block) package-level constants declared in
// different files of the same package must not swap into each other.
func TestDifferentFilesNoSwap(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	src1 := "package p\n\nconst A = 1\n\nfunc F() int { return A }\n"
	src2 := "package p\n\nconst B = 2\n\nfunc G() int { return B }\n"

	file1, err := parser.ParseFile(fset, "a.go", src1, parser.AllErrors)
	if err != nil {
		t.Fatalf("parsing a.go: %v", err)
	}

	file2, err := parser.ParseFile(fset, "b.go", src2, parser.AllErrors)
	if err != nil {
		t.Fatalf("parsing b.go: %v", err)
	}

	info := &types.Info{
		Types:  make(map[ast.Expr]types.TypeAndValue),
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
	}

	conf := types.Config{Importer: importer.Default()}

	pkg, err := conf.Check("p", fset, []*ast.File{file1, file2}, info)
	if err != nil {
		t.Fatalf("type-checking: %v", err)
	}

	bound := (&constSwap{}).WithScope(info, pkg)

	useA := findIdent(t, file1, info, "A")
	if bound.Applies(useA) {
		t.Error("Applies(A) = true, want false: A and B are declared in different files")
	}
}

// TestNoSiblingApplesFalse covers a constant with no same-type sibling at
// all: Applies must report false.
func TestNoSiblingApplesFalse(t *testing.T) {
	t.Parallel()

	src := `package p

const A = 1

func F() int { return A }
`

	_, file, info, pkg := typeCheckFunc(t, src)

	bound := (&constSwap{}).WithScope(info, pkg)

	useA := findIdent(t, file, info, "A")
	if bound.Applies(useA) {
		t.Error("Applies(A) = true, want false: A has no sibling constant")
	}

	if muts := bound.Mutate(useA); muts != nil {
		t.Errorf("Mutate(A) = %v, want nil", muts)
	}
}
