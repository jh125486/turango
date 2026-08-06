package literal

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
)

// parseFunc parses body as the body of a function in a throwaway file. The
// snippets are never type-checked, so they need only parse.
func parseFunc(t *testing.T, body string) (*token.FileSet, *ast.File) {
	t.Helper()

	src := "package p\n\nfunc f() {\n\t" + body + "\n}\n"

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
	}

	return fset, file
}

// render prints node back to source so a mutation can be asserted against a
// string rather than by walking the tree.
func render(t *testing.T, fset *token.FileSet, node ast.Node) string {
	t.Helper()

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		t.Fatalf("printing node: %v", err)
	}

	return buf.String()
}

// findNodes returns every node of type T in file, in walk order.
func findNodes[T ast.Node](file *ast.File) []T {
	var found []T

	ast.Inspect(file, func(node ast.Node) bool {
		if typed, ok := node.(T); ok {
			found = append(found, typed)
		}

		return true
	})

	return found
}

// findNode returns the sole node of type T in file, failing if there is not
// exactly one.
func findNode[T ast.Node](t *testing.T, file *ast.File) T {
	t.Helper()

	found := findNodes[T](file)
	if len(found) != 1 {
		var zero T
		t.Fatalf("want exactly one %T in snippet, got %d", zero, len(found))
	}

	return found[0]
}

// applyRoundTrip asserts that applying m changes target's rendering to want
// and that reverting restores it exactly.
func applyRoundTrip(t *testing.T, fset *token.FileSet, target ast.Node, m mutator.Mutation, want string) {
	t.Helper()

	before := render(t, fset, target)

	m.Apply()

	if got := render(t, fset, target); got != want {
		t.Errorf("after Apply: got %q, want %q", got, want)
	}

	m.Revert()

	if got := render(t, fset, target); got != before {
		t.Errorf("after Revert: got %q, want %q", got, before)
	}
}

func TestRegisteredNames(t *testing.T) {
	for _, name := range []string{NumberName, BooleanName} {
		t.Run(name, func(t *testing.T) {
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
