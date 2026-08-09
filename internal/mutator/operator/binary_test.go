package operator_test

import (
	"go/ast"
	"testing"
)

func TestBinaryMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string // the expression, assigned to _ in the snippet
		want string // rendered expression after Apply
		desc string
	}{
		{name: "add to sub", src: "a + b", want: "a - b", desc: "+ -> -"},
		{name: "sub to add", src: "a - b", want: "a + b", desc: "- -> +"},
		{name: "mul to quo", src: "a * b", want: "a / b", desc: "* -> /"},
		// % is one-directional, mirroring the assignment table's %= -> /=, ...
		{name: "rem to quo", src: "a % b", want: "a / b", desc: "% -> /"},
		// ... and / must not swap back to %; it keeps its own partner *.
		{name: "quo to mul not rem", src: "a / b", want: "a * b", desc: "/ -> *"},
		{name: "and to or", src: "a & b", want: "a | b", desc: "& -> |"},
		{name: "or to and", src: "a | b", want: "a & b", desc: "| -> &"},
		{name: "xor to and one way", src: "a ^ b", want: "a & b", desc: "^ -> &"},
		{name: "and not to xor one way", src: "a &^ b", want: "a ^ b", desc: "&^ -> ^"},
		{name: "shl to shr", src: "a << b", want: "a >> b", desc: "<< -> >>"},
		{name: "shr to shl", src: "a >> b", want: "a << b", desc: ">> -> <<"},
		{name: "land to lor", src: "a && b", want: "a || b", desc: "&& -> ||"},
		{name: "lor to land", src: "a || b", want: "a && b", desc: "|| -> &&"},
		{name: "eql to neq", src: "a == b", want: "a != b", desc: "== -> !="},
		{name: "neq to eql", src: "a != b", want: "a == b", desc: "!= -> =="},
		{name: "lss to geq", src: "a < b", want: "a >= b", desc: "< -> >="},
		{name: "geq to lss", src: "a >= b", want: "a < b", desc: ">= -> <"},
		{name: "gtr to leq", src: "a > b", want: "a <= b", desc: "> -> <="},
		{name: "leq to gtr", src: "a <= b", want: "a > b", desc: "<= -> >"},
	}

	m := newMutator(t, "operator/binary")

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

// TestBinaryMutatesEachOperandIndependently checks that a nested expression
// yields one mutation per binary node rather than one for the whole tree.
func TestBinaryMutatesEachOperandIndependently(t *testing.T) {
	t.Parallel()

	fset, file := parseFunc(t, "_ = a+b == c")

	m := newMutator(t, "operator/binary")

	exprs := findNodes[*ast.BinaryExpr](file)
	if len(exprs) != 2 {
		t.Fatalf("want 2 binary expressions, got %d", len(exprs))
	}

	// Walk order is outermost first: the == node, then its a+b operand.
	wants := []string{"a+b != c", "a-b == c"}

	root := exprs[0]

	for i, expr := range exprs {
		mutations := m.Mutate(expr)
		if len(mutations) != 1 {
			t.Fatalf("expr %d: Mutate() returned %d mutations, want 1", i, len(mutations))
		}

		applyRoundTrip(t, fset, root, mutations[0], wants[i])
	}
}

func TestBinaryIgnoresOtherNodes(t *testing.T) {
	t.Parallel()

	_, file := parseFunc(t, "a += 1")

	m := newMutator(t, "operator/binary")

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
