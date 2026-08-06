package operator

import (
	"go/ast"
	"testing"
)

func TestIncDecMutate(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
		desc string
	}{
		{name: "inc to dec", src: "i++", want: "i--", desc: "++ -> --"},
		{name: "dec to inc", src: "i--", want: "i++", desc: "-- -> ++"},
	}

	var m incDec

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseFunc(t, tt.src)
			stmt := findNode[*ast.IncDecStmt](t, file)

			if !m.Applies(stmt) {
				t.Fatal("Applies() = false, want true")
			}

			before := render(t, fset, stmt)

			mutations := m.Mutate(stmt)

			if got := render(t, fset, stmt); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}

			if mutations[0].Description != tt.desc {
				t.Errorf("Description = %q, want %q", mutations[0].Description, tt.desc)
			}

			applyRoundTrip(t, fset, stmt, mutations[0], tt.want)
		})
	}
}

// TestIncDecInsideForPost covers the common shape, a loop's post statement,
// where the mutant flips a counting-up loop to counting down.
func TestIncDecInsideForPost(t *testing.T) {
	fset, file := parseFunc(t, "for i := 0; i < n; i++ {\n\t}")

	var m incDec

	stmt := findNode[*ast.IncDecStmt](t, file)
	loop := findNode[*ast.ForStmt](t, file)

	mutations := m.Mutate(stmt)
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}

	applyRoundTrip(t, fset, loop, mutations[0], "for i := 0; i < n; i-- {\n}")
}

func TestIncDecIgnoresOtherNodes(t *testing.T) {
	_, file := parseFunc(t, "a += 1")

	var m incDec

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
