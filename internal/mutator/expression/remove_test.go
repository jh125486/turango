package expression_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
	"github.com/jh125486/turango/internal/mutator/expression"
)

// parseExpr parses src as a Go expression by wrapping it in a minimal file, so
// the returned node carries positions from a real *token.FileSet and can be
// printed back out faithfully.
func parseExpr(t *testing.T, src string) (*token.FileSet, ast.Expr) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "src.go", "package p\n\nvar _ = "+src+"\n", 0)
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
	}

	decl, ok := file.Decls[0].(*ast.GenDecl)
	if !ok {
		t.Fatalf("parsing %q: first decl is %T, want *ast.GenDecl", src, file.Decls[0])
	}

	spec, ok := decl.Specs[0].(*ast.ValueSpec)
	if !ok {
		t.Fatalf("parsing %q: first spec is %T, want *ast.ValueSpec", src, decl.Specs[0])
	}

	return fset, spec.Values[0]
}

// render prints node with go/printer, which is how the engine regenerates a
// mutated file, so the assertions below check what a mutant would compile as.
func render(t *testing.T, fset *token.FileSet, node ast.Node) string {
	t.Helper()

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		t.Fatalf("printing node: %v", err)
	}

	return buf.String()
}

// TestApplies covers RemoveMutator.Applies: the table exercises the various
// expression shapes it must tell apart, and the "non-expression nodes"
// subtest covers the non-*ast.BinaryExpr nodes the engine's walk hands over,
// including the nil the walk uses to signal the end of a subtree.
func TestApplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{name: "logical and", src: "a && b", want: true},
		{name: "logical or", src: "a || b", want: true},
		{name: "nested logical and", src: "a && b && c", want: true},
		{name: "addition", src: "a + b", want: false},
		{name: "equality", src: "a == b", want: false},
		{name: "bitwise and", src: "a & b", want: false},
		{name: "bitwise or", src: "a | b", want: false},
		{name: "identifier", src: "a", want: false},
		{name: "call", src: "f(a)", want: false},
		{name: "unary not", src: "!a", want: false},
		{name: "parenthesized and", src: "(a && b)", want: false}, // *ast.ParenExpr wraps it
	}

	m := &expression.RemoveMutator{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, expr := parseExpr(t, tt.src)

			if got := m.Applies(expr); got != tt.want {
				t.Errorf("Applies(%s) = %v, want %v", tt.src, got, tt.want)
			}

			if tt.want {
				return
			}

			if got := m.Mutate(expr); got != nil {
				t.Errorf("Mutate(%s) = %v, want nil when Applies is false", tt.src, got)
			}
		})
	}

	t.Run("non-expression nodes", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, "src.go", "package p\n\nfunc f(a, b bool) bool { return a && b }\n", 0)
		if err != nil {
			t.Fatalf("parsing source: %v", err)
		}

		// The nil entry is not hypothetical: ast.Inspect calls its visitor with a
		// nil node to signal the end of a subtree, so the engine's walk will hand
		// one over.
		nonExprNodes := []struct {
			name string
			node ast.Node
		}{
			{name: "file", node: file},
			{name: "decl", node: file.Decls[0]},
			{name: "nil", node: nil},
		}

		for _, tc := range nonExprNodes {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				if m.Applies(tc.node) {
					t.Errorf("Applies(%T) = true, want false", tc.node)
				}

				if got := m.Mutate(tc.node); got != nil {
					t.Errorf("Mutate(%T) = %v, want nil", tc.node, got)
				}
			})
		}
	})
}

// TestMutate covers RemoveMutator.Mutate: the table exercises the AST shapes
// and operand combinations the operator rewrites, and two further subtests
// pin behavior that does not fit the table's per-case description/left/right
// assertions — that the two returned mutations are independently
// applicable/revertible in either order, and that the replacement node is an
// *ast.Ident rather than an *ast.BasicLit.
func TestMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantDescs []string
		wantLeft  string
		wantRight string
	}{
		{
			name: "and",
			src:  "a && b",
			wantDescs: []string{
				"replace left operand of && with true",
				"replace right operand of && with true",
			},
			wantLeft:  "true && b",
			wantRight: "a && true",
		},
		{
			name: "or",
			src:  "a || b",
			wantDescs: []string{
				"replace left operand of || with false",
				"replace right operand of || with false",
			},
			wantLeft:  "false || b",
			wantRight: "a || false",
		},
		{
			// && is left-associative, so the outermost node is (a && b) && c and
			// its left operand is replaced wholesale — the mutator never walks
			// into the nested BinaryExpr itself.
			name: "nested and",
			src:  "a && b && c",
			wantDescs: []string{
				"replace left operand of && with true",
				"replace right operand of && with true",
			},
			wantLeft:  "true && c",
			wantRight: "a && b && true",
		},
		{
			// && binds tighter than ||, so the outermost node is a || (b && c).
			name: "mixed precedence",
			src:  "a || b && c",
			wantDescs: []string{
				"replace left operand of || with false",
				"replace right operand of || with false",
			},
			wantLeft:  "false || b && c",
			wantRight: "a || false",
		},
		{
			name: "call operands",
			src:  "f(x) && g(y)",
			wantDescs: []string{
				"replace left operand of && with true",
				"replace right operand of && with true",
			},
			wantLeft:  "true && g(y)",
			wantRight: "f(x) && true",
		},
	}

	m := &expression.RemoveMutator{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset, expr := parseExpr(t, tt.src)

			binary, ok := expr.(*ast.BinaryExpr)
			if !ok {
				t.Fatalf("parsed %s as %T, want *ast.BinaryExpr", tt.src, expr)
			}

			originalX, originalY := binary.X, binary.Y
			before := render(t, fset, expr)

			mutations := m.Mutate(expr)
			if len(mutations) != 2 {
				t.Fatalf("Mutate(%s) returned %d mutations, want 2", tt.src, len(mutations))
			}

			if got := render(t, fset, expr); got != before {
				t.Errorf("Mutate mutated the AST: got %q, want %q untouched", got, before)
			}

			for i, want := range []string{tt.wantLeft, tt.wantRight} {
				if got := mutations[i].Description; got != tt.wantDescs[i] {
					t.Errorf("mutations[%d].Description = %q, want %q", i, got, tt.wantDescs[i])
				}

				mutations[i].Apply()

				if got := render(t, fset, expr); got != want {
					t.Errorf("after mutations[%d].Apply(): %q, want %q", i, got, want)
				}

				mutations[i].Revert()

				if got := render(t, fset, expr); got != before {
					t.Errorf("after mutations[%d].Revert(): %q, want %q", i, got, before)
				}
			}

			// Revert must restore the exact prior ast.Expr values, not merely
			// equivalent ones, since the engine reuses this AST for the rest of
			// the walk.
			if binary.X != originalX {
				t.Errorf("X = %p after revert, want the original %p", binary.X, originalX)
			}

			if binary.Y != originalY {
				t.Errorf("Y = %p after revert, want the original %p", binary.Y, originalY)
			}
		})
	}

	// TestMutationsAreIndependent (folded in): applies the right-operand
	// mutation first to prove neither mutation depends on the other having
	// run.
	t.Run("mutations are independent", func(t *testing.T) {
		t.Parallel()

		fset, expr := parseExpr(t, "a && b")
		before := render(t, fset, expr)

		mutations := (&expression.RemoveMutator{}).Mutate(expr)
		if len(mutations) != 2 {
			t.Fatalf("Mutate returned %d mutations, want 2", len(mutations))
		}

		for _, i := range []int{1, 0} {
			mutations[i].Apply()
			mutations[i].Revert()
		}

		if got := render(t, fset, expr); got != before {
			t.Errorf("after out-of-order apply/revert: %q, want %q", got, before)
		}
	})

	// TestReplacementIsAnIdent (folded in): pins the boolean literal's node
	// type: true and false are predeclared identifiers in Go, not
	// *ast.BasicLit values.
	t.Run("replacement is an ident", func(t *testing.T) {
		t.Parallel()

		_, expr := parseExpr(t, "a || b")

		binary, ok := expr.(*ast.BinaryExpr)
		if !ok {
			t.Fatalf("parsed as %T, want *ast.BinaryExpr", expr)
		}

		mutations := (&expression.RemoveMutator{}).Mutate(expr)
		mutations[0].Apply()

		ident, ok := binary.X.(*ast.Ident)
		if !ok {
			t.Fatalf("X = %T after Apply, want *ast.Ident", binary.X)
		}

		if ident.Name != "false" {
			t.Errorf("X.Name = %q, want %q", ident.Name, "false")
		}

		if !ident.NamePos.IsValid() {
			t.Error("X.NamePos is invalid, want the original operand's position")
		}
	})
}

// TestRegistered covers the operator's registration under Name: that
// mutator.New resolves it and that the resolved instance reports the same
// name back.
func TestRegistered(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(expression.Name)
	if err != nil {
		t.Fatalf("mutator.New(%q) error = %v", expression.Name, err)
	}

	if got := m.Name(); got != "expression/remove" {
		t.Errorf("Name() = %q, want %q", got, "expression/remove")
	}
}
