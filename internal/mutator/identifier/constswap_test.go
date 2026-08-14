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
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// boundMutator constructs the operator registered under name via
// [mutator.New] plus a [mutator.TypedMutator] assertion — the same path the
// engine uses — and binds it to info/pkg. Both constSwap and localConstSwap
// are unexported, so this is the only way a blackbox test can obtain a
// working instance of either.
func boundMutator(t *testing.T, name string, info *types.Info, pkg *types.Package) mutator.Mutator {
	t.Helper()

	m, err := mutator.New(name)
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}

	tm, ok := m.(mutator.TypedMutator)
	if !ok {
		t.Fatalf("%T does not implement mutator.TypedMutator", m)
	}

	return tm.WithScope(info, pkg)
}

// findBinaryExpr returns the sole *ast.BinaryExpr in file whose printed
// source text equals want. Comparisons are rendered without whitespace
// beyond what go/printer emits for the expression itself, so want should be
// written exactly as it appears in the snippet's source (e.g. "n1 > maxVal").
func findBinaryExpr(t *testing.T, fset *token.FileSet, file *ast.File, want string) *ast.BinaryExpr {
	t.Helper()

	var found []*ast.BinaryExpr

	ast.Inspect(file, func(node ast.Node) bool {
		expr, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}

		if render(t, fset, expr) == want {
			found = append(found, expr)
		}

		return true
	})

	if len(found) != 1 {
		t.Fatalf("want exactly one BinaryExpr rendering as %q in snippet, got %d", want, len(found))
	}

	return found[0]
}

// registeredNames lists every registry name this file's operators are
// expected to be reachable under, shared between TestRegisteredName and
// TestUnboundInstanceIsInert so both cover v1 (ConstSwapName) and v2
// (LocalConstSwapName) identically.
var registeredNames = []string{identifier.ConstSwapName, identifier.LocalConstSwapName}

// TestRegisteredName pins the exact registry name of each operator this
// package registers, since it is the string a user types into
// -mutateoperators.
func TestRegisteredName(t *testing.T) {
	t.Parallel()

	for _, name := range registeredNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := mutator.New(name)
			if err != nil {
				t.Fatalf("New(%q): %v", name, err)
			}

			if m.Name() != name {
				t.Errorf("Name() = %q, want %q", m.Name(), name)
			}
		})
	}
}

// TestUnboundInstanceIsInert covers the zero-value contract shared by both
// operators in this file: the registry-held instance has no type
// information — WithScope was never called on it — and must never report a
// match.
func TestUnboundInstanceIsInert(t *testing.T) {
	t.Parallel()

	for _, name := range registeredNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := mutator.New(name)
			if err != nil {
				t.Fatalf("New(%q): %v", name, err)
			}

			_, file, _, _ := typeCheckFunc(t, "package p\n\nconst (\n\tA = 1\n\tB = 2\n)\n\nfunc F() int { if A > B { return A }; return B }\n")

			ast.Inspect(file, func(node ast.Node) bool {
				if m.Applies(node) {
					t.Errorf("unbound instance Applies(%T) = true, want false", node)
				}

				if muts := m.Mutate(node); muts != nil {
					t.Errorf("unbound instance Mutate(%T) = %v, want nil", node, muts)
				}

				return true
			})
		})
	}
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

	blankIdent := []fileSrc{{name: snippet, src: `package p

const (
	_ = iota
	A
	B
)

func F() int { return A }
`}}

	threeSiblings := []fileSrc{{name: snippet, src: `package p

const (
	A = 1
	B = 2
	C = 3
)

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
		{
			// The blank identifier in a const( ... ) block, verified against
			// go/types' actual behaviour rather than assumed: unlike a blank
			// *variable* (where info.Defs records nil), go/types does record
			// a real *types.Const, named "_", for a blank const spec — so
			// yieldDeclConsts's info.Defs[name].(*types.Const) type assertion
			// succeeds for it, and it is offered as an ordinary same-group
			// sibling alongside B. Sorted by name, "_" (0x5F) sorts after
			// the uppercase letters, so B is offered before it.
			name:            "blank identifier in const block is an ordinary sibling",
			files:           blankIdent,
			targetFile:      snippet,
			target:          "A",
			wantApplies:     true,
			wantMutations:   []string{"A -> B", "A -> _"},
			checkRoundTrip:  true,
			wantAppliedText: "B",
		},
		{
			// Three same-type constants in one block: A has two siblings
			// (B, C), which is what exercises buildGroups' sort.Slice
			// comparator — a group of exactly one sibling (as in "same
			// block: A/B" above) never invokes it, since sort.Slice does not
			// call Less for a length-1 slice.
			name:          "same block: three siblings",
			files:         threeSiblings,
			targetFile:    snippet,
			target:        "A",
			wantApplies:   true,
			wantMutations: []string{"A -> B", "A -> C"},
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
			bound := boundMutator(t, identifier.ConstSwapName, info, pkg)
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
			bound := boundMutator(t, identifier.ConstSwapName, info, pkg)
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

// localScopeCase describes one local-variable-to-package-constant scoping
// scenario shared between TestLocalApplies and TestLocalMutate: a small
// package, the exact source text of the comparison expression to probe (fed
// to [findBinaryExpr]), and the expected outcome.
type localScopeCase struct {
	name string

	src        string
	comparison string

	wantApplies bool

	// wantMutations is the expected Mutate() descriptions, in order (operand
	// X's mutations, then operand Y's, per localConstSwap.Mutate); nil means
	// Mutate must return nil.
	wantMutations []string
}

func localScopeCases() []localScopeCase {
	return []localScopeCase{
		{
			// The core positive case, modelled directly on the strconv
			// ParseUint shape this operator exists to close (see
			// TestLocalMutateReproducesStrconvShape for the same assertion
			// against the actual frozen corpus fixture, not a synthetic
			// snippet): a local variable (maxVal) declared with an explicit
			// uint64 type, compared against another local (n1) that happens
			// to share the same type, with one package-level constant (an
			// untyped integer literal, exactly like the real maxUint64)
			// that fits both. Both operands are independently eligible,
			// since both are uint64-typed locals.
			name: "strconv shape: both operands eligible",
			src: `package p

const maxUint64 = (1<<64 - 1)

func F(bitSize int) uint64 {
	var maxVal uint64
	maxVal = 1<<uint(bitSize) - 1

	var n1 uint64

	if n1 > maxVal {
		return 0
	}

	return maxVal
}
`,
			comparison:    "n1 > maxVal",
			wantApplies:   true,
			wantMutations: []string{"n1 -> maxUint64", "maxVal -> maxUint64"},
		},
		{
			// Isolates the explicitly-typed-constant path: the candidate
			// constant has its own uint64 type, so the exact-match rule —
			// the same one constSwap's const-for-const swap uses — applies,
			// not the untyped-constant representability check.
			name: "explicitly typed candidate: exact type match",
			src: `package p

const Limit uint64 = 100

func F() bool {
	var x uint64

	return x > Limit
}
`,
			comparison:    "x > Limit",
			wantApplies:   true,
			wantMutations: []string{"x -> Limit"},
		},
		{
			// Isolates the untyped-constant representability path with a
			// single eligible operand: x is compared against a literal, not
			// an identifier, so only x itself is ever a candidate operand.
			name: "untyped candidate: representable",
			src: `package p

const Max = 1<<64 - 1

func F() bool {
	var x uint64

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// The representability guard this operator exists to hold
			// itself to, instead of the looser (and, per this file's own
			// investigation, actually wrong for negative values)
			// types.AssignableTo check: a negative untyped constant must
			// never be offered as a swap-in for an unsigned local, since
			// `var x uint64 = NegOne` is a real compile error ("constant -1
			// overflows uint64") that this operator can and should avoid
			// proposing in the first place.
			name: "negative untyped candidate: not representable in unsigned local",
			src: `package p

const NegOne = -1

func F() bool {
	var x uint64

	return x > 5
}
`,
			comparison:  "x > 5",
			wantApplies: false,
		},
		{
			// Type mismatch: the local is a string, the only package
			// constant is numeric — representable rejects non-integer
			// basic kinds outright, so no candidate is offered.
			name: "type mismatch: no candidates",
			src: `package p

const N = 5

func F() bool {
	var s string

	return s == "a"
}
`,
			comparison:  `s == "a"`,
			wantApplies: false,
		},
		{
			// The local-variable-only restriction: a package-level
			// variable of a matching type must never be offered a swap,
			// even though it satisfies the type check — only function-local
			// variables are in scope for this operator ("local variables",
			// not "variables").
			name: "package-level variable excluded",
			src: `package p

const Max = 1<<64 - 1

var globalX uint64

func F() bool {
	return globalX > 5
}
`,
			comparison:  "globalX > 5",
			wantApplies: false,
		},
		{
			// The comparison-operand restriction: an arithmetic operator is
			// not in comparisonOps, so Applies is false regardless of
			// whether the operands would otherwise be eligible — this is
			// the operator's primary defence against scanning every
			// identifier use in a function instead of just the ones at a
			// boundary.
			name: "non-comparison operator excluded",
			src: `package p

const Max = 1<<64 - 1

func F() uint64 {
	var x uint64

	return x + 5
}
`,
			comparison:  "x + 5",
			wantApplies: false,
		},
		{
			// No package-level constants at all: candidatesFor must return
			// empty rather than panicking on an empty consts slice.
			name: "no package constants: no candidates",
			src: `package p

func F() bool {
	var x uint64

	return x > 5
}
`,
			comparison:  "x > 5",
			wantApplies: false,
		},
		{
			// representable's signed-integer path: a plain "int" local is
			// types.Int, one of the two kinds representable's first switch
			// case (Int, Int64) handles via inSignedRange.
			name: "signed local: int",
			src: `package p

const Max = 100

func F() bool {
	var x int

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Int8 case.
			name: "signed local: int8",
			src: `package p

const Max = 100

func F() bool {
	var x int8

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Int16 case.
			name: "signed local: int16",
			src: `package p

const Max = 1000

func F() bool {
	var x int16

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Int32 case.
			name: "signed local: int32",
			src: `package p

const Max = 100000

func F() bool {
	var x int32

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Uint8 case (Uint/Uint64/Uintptr is already
			// covered by the strconv-shaped uint64 cases above).
			name: "unsigned local: uint8",
			src: `package p

const Max = 200

func F() bool {
	var x uint8

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Uint16 case.
			name: "unsigned local: uint16",
			src: `package p

const Max = 60000

func F() bool {
	var x uint16

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's Uint32 case.
			name: "unsigned local: uint32",
			src: `package p

const Max = 4000000000

func F() bool {
	var x uint32

	return x > 5
}
`,
			comparison:    "x > 5",
			wantApplies:   true,
			wantMutations: []string{"x -> Max"},
		},
		{
			// representable's early, kind-based rejection: F is an untyped
			// *float* constant, so representable must reject it before ever
			// consulting the variable's basic kind (val.Kind() != constant.Int
			// short-circuits ahead of the switch on basic.Kind()) — even
			// though x's own type (uint64) would otherwise be a representable
			// target for an integer constant.
			name: "untyped float candidate: not representable (non-integer)",
			src: `package p

const F = 1.5

func F2() bool {
	var x uint64

	return x > 5
}
`,
			comparison:  "x > 5",
			wantApplies: false,
		},
	}
}

// TestLocalApplies covers Applies() for localConstSwap across every scoping
// scenario in localScopeCases.
func TestLocalApplies(t *testing.T) {
	t.Parallel()

	for _, tc := range localScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset, file, info, pkg := typeCheckFunc(t, tc.src)
			bound := boundMutator(t, identifier.LocalConstSwapName, info, pkg)
			expr := findBinaryExpr(t, fset, file, tc.comparison)

			if got := bound.Applies(expr); got != tc.wantApplies {
				t.Errorf("Applies(%s) = %v, want %v", tc.comparison, got, tc.wantApplies)
			}
		})
	}
}

// TestLocalMutate covers Mutate() across the same scenarios as
// TestLocalApplies.
func TestLocalMutate(t *testing.T) {
	t.Parallel()

	for _, tc := range localScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset, file, info, pkg := typeCheckFunc(t, tc.src)
			bound := boundMutator(t, identifier.LocalConstSwapName, info, pkg)
			expr := findBinaryExpr(t, fset, file, tc.comparison)

			muts := bound.Mutate(expr)

			if tc.wantMutations == nil {
				if muts != nil {
					t.Errorf("Mutate(%s) = %v, want nil", tc.comparison, muts)
				}

				return
			}

			if len(muts) != len(tc.wantMutations) {
				t.Fatalf("Mutate(%s) returned %d mutations, want %d", tc.comparison, len(muts), len(tc.wantMutations))
			}

			for i, want := range tc.wantMutations {
				if got := muts[i].Description; got != want {
					t.Errorf("Mutate(%s)[%d] description = %q, want %q", tc.comparison, i, got, want)
				}
			}
		})
	}
}

// TestConstSwapMutateIgnoresIneligibleNodes calls Mutate() directly — without
// going through Applies() first, unlike every scopeCases scenario above — on
// nodes constUseIdent rejects for two different reasons: a node that is not
// an *ast.Ident at all, and an *ast.Ident that is a use of something other
// than a package-level constant (here, a local variable). Both are real,
// reachable inputs: the engine's own contract calls Applies() before Mutate()
// on every node it walks, but Mutate() does not assume that discipline itself
// (unlike, e.g., control/if.go's Mutate, which re-checks its own Applies())
// and must return nil rather than panic for either shape.
func TestConstSwapMutateIgnoresIneligibleNodes(t *testing.T) {
	t.Parallel()

	const src = `package p

const A = 1

func F() int {
	x := 2

	return x + A
}
`

	_, file, info, pkg := typeCheckFunc(t, src)
	bound := boundMutator(t, identifier.ConstSwapName, info, pkg)

	t.Run("non-ident node", func(t *testing.T) {
		t.Parallel()

		var lit *ast.BasicLit

		ast.Inspect(file, func(node ast.Node) bool {
			if l, ok := node.(*ast.BasicLit); ok {
				lit = l
			}

			return true
		})

		if lit == nil {
			t.Fatal("no *ast.BasicLit found in snippet")
		}

		if bound.Applies(lit) {
			t.Error("Applies(BasicLit) = true, want false")
		}

		if got := bound.Mutate(lit); got != nil {
			t.Errorf("Mutate(BasicLit) = %v, want nil", got)
		}
	})

	t.Run("ident use of a non-const object", func(t *testing.T) {
		t.Parallel()

		xIdent := findIdent(t, file, info, "x")

		if bound.Applies(xIdent) {
			t.Error("Applies(x) = true, want false")
		}

		if got := bound.Mutate(xIdent); got != nil {
			t.Errorf("Mutate(x) = %v, want nil", got)
		}
	})
}

// TestLocalMutateReproducesStrconvShape is the required acceptance proof for
// the v2 extension: it type-checks the actual frozen
// corpus/stdlib-strconv-parseuint/module fixture — the pre-fix copy of Go's
// real strconv package, golang/go#21278 — and asserts that Mutate(), called
// on the exact comparison the historical bug lives at (`n1 > maxVal` in
// ParseUint, corpus/stdlib-strconv-parseuint/module/atoi.go), offers "maxVal
// -> maxUint64" as one of its candidates. That is precisely the historical
// mistake: ParseUint checked the overflowed sum against the package-wide
// maxUint64 instead of the bitSize-scoped maxVal.
//
// This is a whitebox-style, in-process type-check (go/types.Config.Check
// directly, not golang.org/x/tools/go/packages or a real `go test -mutate`
// run), matching this file's existing typeCheckFiles/typeCheckFunc
// convention rather than the internal/corpus package's real end-to-end
// harness — this test only needs to prove the operator *offers* the
// mutation, not that a full mutation-testing run against strconv's test
// suite kills or survives it, and the in-process form runs in milliseconds
// instead of shelling out to the real Go toolchain.
//
// atoi.go references Quote (quote.go) and Itoa (itoa.go), both in the same
// frozen module, so all four non-test .go files in the module directory are
// read and type-checked together as one package — a single-file check
// against atoi.go alone fails with "undefined: Quote"/"undefined: Itoa".
func TestLocalMutateReproducesStrconvShape(t *testing.T) {
	t.Parallel()

	const moduleDir = "../../../corpus/stdlib-strconv-parseuint/module"

	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", moduleDir, err)
	}

	var files []fileSrc

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := os.ReadFile(filepath.Join(moduleDir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}

		files = append(files, fileSrc{name: name, src: string(src)})
	}

	if len(files) == 0 {
		t.Fatalf("no non-test .go files found under %s", moduleDir)
	}

	fset, byName, info, pkg := typeCheckFiles(t, files)
	bound := boundMutator(t, identifier.LocalConstSwapName, info, pkg)

	atoiFile, ok := byName["atoi.go"]
	if !ok {
		t.Fatalf("%s has no atoi.go", moduleDir)
	}

	expr := findBinaryExpr(t, fset, atoiFile, "n1 > maxVal")

	if !bound.Applies(expr) {
		t.Fatalf("Applies(n1 > maxVal) = false, want true")
	}

	muts := bound.Mutate(expr)

	var descriptions []string

	for _, m := range muts {
		descriptions = append(descriptions, m.Description)
		if m.Description != "maxVal -> maxUint64" {
			continue
		}

		// Confirm the mutation actually rewrites the right identifier: apply
		// it and check the rendered source, then revert and check it's back
		// to the original — the same round-trip TestMutate performs for v1.
		ident, ok := m.Node.(*ast.Ident)
		if !ok {
			t.Fatalf("Mutation.Node = %T, want *ast.Ident", m.Node)
		}

		before := render(t, fset, ident)

		m.Apply()

		if got := render(t, fset, ident); got != "maxUint64" {
			t.Errorf("after Apply: got %q, want %q", got, "maxUint64")
		}

		m.Revert()

		if got := render(t, fset, ident); got != before {
			t.Errorf("after Revert: got %q, want %q", got, before)
		}
	}

	if !slices.Contains(descriptions, "maxVal -> maxUint64") {
		t.Fatalf("Mutate(n1 > maxVal) descriptions = %v, want to contain %q", descriptions, "maxVal -> maxUint64")
	}
}
