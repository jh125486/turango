package operator_test

import (
	"go/ast"
	"testing"
)

func TestBoundaryMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string // the expression, assigned to _ in the snippet
		want string // rendered expression after Apply
		desc string
	}{
		{name: "lss to leq", src: "a < b", want: "a <= b", desc: "< -> <="},
		{name: "leq to lss", src: "a <= b", want: "a < b", desc: "<= -> <"},
		{name: "gtr to geq", src: "a > b", want: "a >= b", desc: "> -> >="},
		{name: "geq to gtr", src: "a >= b", want: "a > b", desc: ">= -> >"},
	}

	m := newMutator(t, "operator/boundary")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset, file := parseFunc(t, "_ = "+tt.src)
			expr := findNode[*ast.BinaryExpr](t, file)

			if !m.Applies(expr) {
				t.Fatal("Applies() = false, want true")
			}

			before := render(t, fset, expr)

			mutations := m.Mutate(expr)

			if got := render(t, fset, expr); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}

			if mutations[0].Description != tt.desc {
				t.Errorf("Description = %q, want %q", mutations[0].Description, tt.desc)
			}

			applyRoundTrip(t, fset, expr, mutations[0], tt.want)
		})
	}
}

// TestBoundaryIgnoresNegationOperators checks that boundary does not claim
// the operators binary's negation swap already covers (==, !=, &&, ||, +,
// etc.) — the two operators must partition the relational tokens, not
// overlap.
func TestBoundaryIgnoresNegationOperators(t *testing.T) {
	t.Parallel()

	m := newMutator(t, "operator/boundary")

	for _, src := range []string{"a == b", "a != b", "a && b", "a || b", "a + b"} {
		t.Run(src, func(t *testing.T) {
			t.Parallel()

			_, file := parseFunc(t, "_ = "+src)
			expr := findNode[*ast.BinaryExpr](t, file)

			if m.Applies(expr) {
				t.Errorf("Applies(%q) = true, want false", src)
			}

			if got := m.Mutate(expr); got != nil {
				t.Errorf("Mutate(%q) = %v, want nil", src, got)
			}
		})
	}
}

func TestBoundaryIgnoresOtherNodes(t *testing.T) {
	t.Parallel()

	_, file := parseFunc(t, "a += 1")

	m := newMutator(t, "operator/boundary")

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
