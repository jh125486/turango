package operator_test

import (
	"go/ast"
	"testing"
)

func TestAssignmentMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string // rendered statement after Apply; "" means no mutation offered
		desc string
	}{
		{name: "add to sub", src: "a += 1", want: "a -= 1", desc: "+= -> -="},
		{name: "sub to add", src: "a -= 1", want: "a += 1", desc: "-= -> +="},
		{name: "mul to quo", src: "a *= 2", want: "a /= 2", desc: "*= -> /="},
		// %= is one-directional: it swaps to /=, ...
		{name: "rem to quo", src: "a %= 2", want: "a /= 2", desc: "%= -> /="},
		// ... and /= must not swap back to %=; it keeps its own partner *=.
		{name: "quo to mul not rem", src: "a /= 2", want: "a *= 2", desc: "/= -> *="},
		{name: "and to or", src: "a &= 1", want: "a |= 1", desc: "&= -> |="},
		{name: "or to and", src: "a |= 1", want: "a &= 1", desc: "|= -> &="},
		{name: "xor to and one way", src: "a ^= 1", want: "a &= 1", desc: "^= -> &="},
		{name: "and not to xor one way", src: "a &^= 1", want: "a ^= 1", desc: "&^= -> ^="},
		{name: "shl to shr", src: "a <<= 1", want: "a >>= 1", desc: "<<= -> >>="},
		{name: "shr to shl", src: "a >>= 1", want: "a <<= 1", desc: ">>= -> <<="},
		{name: "define excluded", src: "a := 1"},
		{name: "assign excluded", src: "a = 1"},
		{name: "multi assign excluded", src: "a, b = b, a"},
	}

	m := newMutator(t, "operator/assignment")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset, file := parseFunc(t, tt.src)
			stmt := findNode[*ast.AssignStmt](t, file)

			eligible := tt.want != ""

			if got := m.Applies(stmt); got != eligible {
				t.Fatalf("Applies() = %v, want %v", got, eligible)
			}

			before := render(t, fset, stmt)

			mutations := m.Mutate(stmt)

			if got := render(t, fset, stmt); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if !eligible {
				if mutations != nil {
					t.Fatalf("Mutate() = %v, want nil", mutations)
				}

				return
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

func TestAssignmentApplies(t *testing.T) {
	t.Parallel()

	_, file := parseFunc(t, "a := b + c")

	m := newMutator(t, "operator/assignment")

	for _, expr := range findNodes[*ast.BinaryExpr](file) {
		if m.Applies(expr) {
			t.Error("Applies() = true for a binary expression")
		}

		if got := m.Mutate(expr); got != nil {
			t.Errorf("Mutate() = %v, want nil", got)
		}
	}
}
