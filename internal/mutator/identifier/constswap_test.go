// Package identifier_test exercises constSwap's exported surface —
// [identifier.ConstSwapName], and the [mutator.Mutator] / [mutator.TypedMutator]
// methods reachable through the registry — without depending on the
// unexported constSwap type itself. constSwap is never referenced directly:
// every bound instance in this file comes from [mutator.New] plus a
// [mutator.TypedMutator] assertion, the same path the engine uses.
package identifier_test

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
	"github.com/jh125486/turango/internal/mutator/identifier"
)

// fileSrc is one named source file, parsed and type-checked together with
// its siblings as a single package.
type fileSrc struct {
	name string
	src  string
}

// typeCheckFiles parses and type-checks files as a single, self-contained
// package. Unlike internal/mutator/operator/operator_test.go's parseFunc —
// whose own doc comment says snippets "are never type-checked, so they need
// only parse" — this operator's Applies/Mutate consult go/types information,
// so its tests need a real *types.Info and *types.Package to bind
// [constSwap.WithScope] against. go/types.Config.Check is used directly,
// rather than golang.org/x/tools/go/packages, so the tests stay fast and
// need no module or toolchain shell-out.
func typeCheckFiles(t *testing.T, files []fileSrc) (*token.FileSet, map[string]*ast.File, *types.Info, *types.Package) {
	t.Helper()

	fset := token.NewFileSet()

	parsed := make([]*ast.File, 0, len(files))
	byName := make(map[string]*ast.File, len(files))

	for _, f := range files {
		file, err := parser.ParseFile(fset, f.name, f.src, parser.AllErrors)
		if err != nil {
			t.Fatalf("parsing %q: %v", f.name, err)
		}

		parsed = append(parsed, file)
		byName[f.name] = file
	}

	info := &types.Info{
		Types:  make(map[ast.Expr]types.TypeAndValue),
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
	}

	conf := types.Config{Importer: importer.Default()}

	pkg, err := conf.Check("p", fset, parsed, info)
	if err != nil {
		t.Fatalf("type-checking: %v", err)
	}

	return fset, byName, info, pkg
}

// typeCheckFunc is typeCheckFiles for the common case of one, self-contained
// snippet.
func typeCheckFunc(t *testing.T, src string) (*token.FileSet, *ast.File, *types.Info, *types.Package) {
	t.Helper()

	const name = "snippet.go"

	fset, files, info, pkg := typeCheckFiles(t, []fileSrc{{name: name, src: src}})

	return fset, files[name], info, pkg
}

// render prints node back to source, mirroring
// internal/mutator/literal/literal_test.go's helper of the same name.
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

// boundMutator constructs the registered constswap operator via
// [mutator.New] plus a [mutator.TypedMutator] assertion — the same path the
// engine uses — and binds it to info/pkg. constSwap itself is unexported, so
// this is the only way a blackbox test can obtain a working instance.
func boundMutator(t *testing.T, info *types.Info, pkg *types.Package) mutator.Mutator {
	t.Helper()

	m, err := mutator.New(identifier.ConstSwapName)
	if err != nil {
		t.Fatalf("New(%q): %v", identifier.ConstSwapName, err)
	}

	tm, ok := m.(mutator.TypedMutator)
	if !ok {
		t.Fatalf("%T does not implement mutator.TypedMutator", m)
	}

	return tm.WithScope(info, pkg)
}

// TestRegisteredName pins the exact registry name, since it is the string a
// user types into -mutateoperators.
func TestRegisteredName(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(identifier.ConstSwapName)
	if err != nil {
		t.Fatalf("New(%q): %v", identifier.ConstSwapName, err)
	}

	if m.Name() != identifier.ConstSwapName {
		t.Errorf("Name() = %q, want %q", m.Name(), identifier.ConstSwapName)
	}
}

// TestUnboundInstanceIsInert covers the zero-value contract: the
// registry-held instance has no type information — WithScope was never
// called on it — and must never report a match.
func TestUnboundInstanceIsInert(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(identifier.ConstSwapName)
	if err != nil {
		t.Fatalf("New(%q): %v", identifier.ConstSwapName, err)
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

// scopeCase describes one const-scoping scenario shared between TestApplies
// and TestMutate: a small package (one or more files), the identifier use to
// probe, and the expected outcome.
type scopeCase struct {
	name string

	files      []fileSrc
	targetFile string
	target     string

	wantApplies bool

	// skipMutate marks a case this suite never asserted anything about
	// Mutate() for — the different-files regression only ever checked
	// Applies().
	skipMutate bool

	// wantMutations is the expected Mutate() descriptions, in order; nil
	// means Mutate must return nil.
	wantMutations []string

	// checkRoundTrip, when true, additionally applies mutations[0] and
	// asserts the identifier renders as wantAppliedText, then reverts and
	// asserts it renders as it did before.
	checkRoundTrip  bool
	wantAppliedText string
}

func scopeCases() []scopeCase {
	const snippet = "snippet.go"

	sameBlock := []fileSrc{{name: snippet, src: `package p

const (
	A = 1
	B = 2
)

func F() int { return A }
func G() int { return B }
`}}

	differentTypes := []fileSrc{{name: snippet, src: `package p

const (
	N int    = 1
	S string = "x"
)

func F() int { return N }
`}}

	differentBlocks := []fileSrc{{name: snippet, src: `package p

const (
	A = 1
)

const (
	B = 2
)

func F() int { return A }
`}}

	differentFiles := []fileSrc{
		{name: "a.go", src: "package p\n\nconst A = 1\n\nfunc F() int { return A }\n"},
		{name: "b.go", src: "package p\n\nconst B = 2\n\nfunc G() int { return B }\n"},
	}

	noSibling := []fileSrc{{name: snippet, src: `package p

const A = 1

func F() int { return A }
`}}

	return []scopeCase{
		{
			// The core positive case: two same-type constants in one
			// const( ... ) block must offer to swap into each other, in
			// both directions.
			name:            "same block: A",
			files:           sameBlock,
			targetFile:      snippet,
			target:          "A",
			wantApplies:     true,
			wantMutations:   []string{"A -> B"},
			checkRoundTrip:  true,
			wantAppliedText: "B",
		},
		{
			name:          "same block: B",
			files:         sameBlock,
			targetFile:    snippet,
			target:        "B",
			wantApplies:   true,
			wantMutations: []string{"B -> A"},
		},
		{
			// The exact-type-match rule: two package-level constants in the
			// same block whose types differ must not be offered as swaps
			// for each other.
			name:        "different types",
			files:       differentTypes,
			targetFile:  snippet,
			target:      "N",
			wantApplies: false,
		},
		{
			// The scope-restriction regression test: two same-type
			// constants declared in different const( ... ) blocks of the
			// same file must not be offered as swaps for each other, even
			// though they satisfy every other condition. This is the
			// easiest way for the operator to silently regress — dropping
			// the block check would still pass every other case here.
			name:        "different blocks",
			files:       differentBlocks,
			targetFile:  snippet,
			target:      "A",
			wantApplies: false,
		},
		{
			// Extends the same regression coverage across files: two
			// same-type, standalone (non-block) package-level constants
			// declared in different files of the same package must not
			// swap into each other.
			name:        "different files",
			files:       differentFiles,
			targetFile:  "a.go",
			target:      "A",
			wantApplies: false,
			skipMutate:  true,
		},
		{
			// A constant with no same-type sibling at all.
			name:        "no sibling",
			files:       noSibling,
			targetFile:  snippet,
			target:      "A",
			wantApplies: false,
		},
	}
}

// TestApplies covers Applies() across every const-scoping scenario the
// operator distinguishes: same block (both directions), mismatched types,
// different blocks, different files, and no sibling at all.
func TestApplies(t *testing.T) {
	t.Parallel()

	for _, tc := range scopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, files, info, pkg := typeCheckFiles(t, tc.files)
			bound := boundMutator(t, info, pkg)
			ident := findIdent(t, files[tc.targetFile], info, tc.target)

			if got := bound.Applies(ident); got != tc.wantApplies {
				t.Errorf("Applies(%s) = %v, want %v", tc.target, got, tc.wantApplies)
			}
		})
	}
}

// TestMutate covers Mutate() across the same scenarios as TestApplies,
// except "different files", which this suite never asserted a Mutate()
// expectation for.
func TestMutate(t *testing.T) {
	t.Parallel()

	for _, tc := range scopeCases() {
		if tc.skipMutate {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset, files, info, pkg := typeCheckFiles(t, tc.files)
			bound := boundMutator(t, info, pkg)
			ident := findIdent(t, files[tc.targetFile], info, tc.target)

			muts := bound.Mutate(ident)

			if tc.wantMutations == nil {
				if muts != nil {
					t.Errorf("Mutate(%s) = %v, want nil", tc.target, muts)
				}

				return
			}

			if len(muts) != len(tc.wantMutations) {
				t.Fatalf("Mutate(%s) returned %d mutations, want %d", tc.target, len(muts), len(tc.wantMutations))
			}

			for i, want := range tc.wantMutations {
				if got := muts[i].Description; got != want {
					t.Errorf("Mutate(%s)[%d] description = %q, want %q", tc.target, i, got, want)
				}
			}

			if !tc.checkRoundTrip {
				return
			}

			before := render(t, fset, ident)

			muts[0].Apply()

			if got := render(t, fset, ident); got != tc.wantAppliedText {
				t.Errorf("after Apply: got %q, want %q", got, tc.wantAppliedText)
			}

			muts[0].Revert()

			if got := render(t, fset, ident); got != before {
				t.Errorf("after Revert: got %q, want %q", got, before)
			}
		})
	}
}
