package control_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
)

// bodyCase describes one operator scenario: parse src, walk to the first node
// the operator targets, and check what the operator makes of it.
type bodyCase struct {
	name string
	src  string
	// applies is the expected Applies result for the target node.
	applies bool
	// want is the printed file after the mutation is applied. It is only
	// consulted when applies is true.
	want string
}

// firstNode returns the first node in file for which match reports true,
// walking in source order.
func firstNode(t *testing.T, file *ast.File, match func(ast.Node) bool) ast.Node {
	t.Helper()

	var found ast.Node

	ast.Inspect(file, func(node ast.Node) bool {
		if found != nil || node == nil {
			return false
		}
		if match(node) {
			found = node

			return false
		}

		return true
	})

	if found == nil {
		t.Fatal("no target node found in source")
	}

	return found
}

// render prints file, which is how the mutation engine materialises a mutant.
func render(t *testing.T, fset *token.FileSet, file *ast.File) string {
	t.Helper()

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("printer.Fprint() error = %v", err)
	}

	return buf.String()
}

// isIfStmt is the node matcher the If and Else operators key off.
func isIfStmt(node ast.Node) bool { _, ok := node.(*ast.IfStmt); return ok }

// run exercises the full engine contract for each case: Applies as a
// pre-filter, Mutate as a non-mutating query, then Apply and Revert.
func run(t *testing.T, m mutator.Mutator, match func(ast.Node) bool, wantDescription string, cases []bodyCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()

			file, err := parser.ParseFile(fset, "src.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile() error = %v", err)
			}

			node := firstNode(t, file, match)

			if got := m.Applies(node); got != tc.applies {
				t.Fatalf("Applies() = %v, want %v", got, tc.applies)
			}

			before := render(t, fset, file)
			mutations := m.Mutate(node)

			if !tc.applies {
				if mutations != nil {
					t.Fatalf("Mutate() = %v, want nil when Applies is false", mutations)
				}

				return
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}
			if got := mutations[0].Description; got != wantDescription {
				t.Errorf("Description = %q, want %q", got, wantDescription)
			}
			if got := render(t, fset, file); got != before {
				t.Errorf("Mutate() modified the AST before Apply:\ngot:\n%s\nwant:\n%s", got, before)
			}

			mutations[0].Apply()

			if got := render(t, fset, file); got != tc.want {
				t.Errorf("after Apply:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}

			mutations[0].Revert()

			if got := render(t, fset, file); got != before {
				t.Errorf("after Revert:\ngot:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

// TestRegistration checks the operators are reachable by the exact names the
// -mutateoperators flag accepts, and that each reports the name it was
// registered under.
func TestRegistration(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"control/if", "control/else", "control/case"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := mutator.New(name)
			if err != nil {
				t.Fatalf("mutator.New(%q) error = %v", name, err)
			}
			if got := m.Name(); got != name {
				t.Errorf("Name() = %q, want %q", got, name)
			}
		})
	}
}
