package literal_test

import (
	"go/ast"
	"testing"

	"github.com/jh125486/turango/internal/mutator/literal"
)

// findBoolIdent returns the *ast.Ident named want (there are several other
// identifiers in scope even in a minimal snippet -- the package name, the
// function name, the blank identifier -- so this filters by name rather
// than assuming the boolean literal is the only *ast.Ident present).
func findBoolIdent(t *testing.T, file *ast.File, want string) *ast.Ident {
	t.Helper()

	for _, ident := range findNodes[*ast.Ident](file) {
		if ident.Name == want {
			return ident
		}
	}

	t.Fatalf("no identifier named %q in snippet", want)

	return nil
}

func TestBooleanMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string
		desc string
	}{
		{name: "true to false", src: "true", want: "false", desc: "true -> false"},
		{name: "false to true", src: "false", want: "true", desc: "false -> true"},
	}

	var m literal.BooleanMutator

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset, file := parseFunc(t, "_ = "+tt.src)
			ident := findBoolIdent(t, file, tt.src)

			if !m.Applies(ident) {
				t.Fatal("Applies() = false, want true")
			}

			before := render(t, fset, ident)

			mutations := m.Mutate(ident)

			if got := render(t, fset, ident); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}

			if mutations[0].Description != tt.desc {
				t.Errorf("Description = %q, want %q", mutations[0].Description, tt.desc)
			}

			applyRoundTrip(t, fset, ident, mutations[0], tt.want)
		})
	}
}

func TestBooleanIgnoresOtherIdents(t *testing.T) {
	t.Parallel()

	_, file := parseFunc(t, "x := 1")

	var m literal.BooleanMutator

	// findNode wants exactly one *ast.Ident; "x := 1" has two (x, and the
	// blank identifier is not present here, so just x). Use findNodes and
	// check the one that isn't a declaration target incidentally matching.
	idents := findNodes[*ast.Ident](file)
	if len(idents) == 0 {
		t.Fatal("expected at least one identifier in snippet")
	}

	for _, ident := range idents {
		if m.Applies(ident) {
			t.Errorf("Applies(%q) = true, want false", ident.Name)
		}

		if got := m.Mutate(ident); got != nil {
			t.Errorf("Mutate(%q) = %v, want nil", ident.Name, got)
		}
	}
}

func TestBooleanIgnoresOtherNodes(t *testing.T) {
	t.Parallel()

	_, file := parseFunc(t, "a := 1")

	var m literal.BooleanMutator

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
