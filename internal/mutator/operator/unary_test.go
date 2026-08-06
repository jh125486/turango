package operator

import (
	"go/ast"
	"testing"
)

// TestUnaryStripsInEachTriggerPosition covers the three positions the operator
// is restricted to: a binary operand, an if condition, and an assignment's
// right-hand side. In each case the mutation is offered while visiting the
// container, not the unary expression itself.
func TestUnaryStripsInEachTriggerPosition(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		target func(t *testing.T, file *ast.File) ast.Node
		want   string
		desc   string
	}{
		{
			name: "binary left operand",
			src:  "_ = !a && b",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.BinaryExpr](t, file)
			},
			want: "a && b",
			desc: "strip unary ! from binary operand",
		},
		{
			name: "binary right operand",
			src:  "_ = a + -b",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.BinaryExpr](t, file)
			},
			want: "a + b",
			desc: "strip unary - from binary operand",
		},
		{
			name: "if condition",
			src:  "if !ok {\n\t}",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.IfStmt](t, file)
			},
			want: "if ok {\n}",
			desc: "strip unary ! from if condition",
		},
		{
			name: "assignment right-hand side",
			src:  "a = -b",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.AssignStmt](t, file)
			},
			want: "a = b",
			desc: "strip unary - from assignment right-hand side",
		},
		{
			name: "short variable declaration right-hand side",
			src:  "a := ^b",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.AssignStmt](t, file)
			},
			want: "a := b",
			desc: "strip unary ^ from assignment right-hand side",
		},
	}

	var m unary

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseFunc(t, tt.src)
			target := tt.target(t, file)

			if !m.Applies(target) {
				t.Fatal("Applies() = false, want true")
			}

			before := render(t, fset, target)

			mutations := m.Mutate(target)

			if got := render(t, fset, target); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}

			if mutations[0].Description != tt.desc {
				t.Errorf("Description = %q, want %q", mutations[0].Description, tt.desc)
			}

			applyRoundTrip(t, fset, target, mutations[0], tt.want)
		})
	}
}

// TestUnaryStripsEachSlotIndependently checks that a container holding two
// unary expressions offers two separately applicable mutations, not one
// combined edit.
func TestUnaryStripsEachSlotIndependently(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		target func(t *testing.T, file *ast.File) ast.Node
		wants  []string
	}{
		{
			name: "both binary operands",
			src:  "_ = !a && !b",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.BinaryExpr](t, file)
			},
			wants: []string{"a && !b", "!a && b"},
		},
		{
			name: "every assignment right-hand side",
			src:  "a, b = -x, !y",
			target: func(t *testing.T, file *ast.File) ast.Node {
				return findNode[*ast.AssignStmt](t, file)
			},
			wants: []string{"a, b = x, !y", "a, b = -x, y"},
		},
	}

	var m unary

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseFunc(t, tt.src)
			target := tt.target(t, file)

			mutations := m.Mutate(target)
			if len(mutations) != len(tt.wants) {
				t.Fatalf("Mutate() returned %d mutations, want %d", len(mutations), len(tt.wants))
			}

			for i, mutation := range mutations {
				applyRoundTrip(t, fset, target, mutation, tt.wants[i])
			}
		})
	}
}

// TestUnaryRevertRestoresTheOriginalPointer pins the contract that Revert
// restores the exact prior node, not an equivalent rebuilt one, so an AST can
// be reused across a whole walk.
func TestUnaryRevertRestoresTheOriginalPointer(t *testing.T) {
	_, file := parseFunc(t, "if !ok {\n\t}")

	stmt := findNode[*ast.IfStmt](t, file)
	original := stmt.Cond

	var m unary

	mutations := m.Mutate(stmt)
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}

	mutations[0].Apply()

	if stmt.Cond == original {
		t.Fatal("Apply did not replace the condition")
	}

	mutations[0].Revert()

	if stmt.Cond != original {
		t.Error("Revert did not restore the original *ast.UnaryExpr pointer")
	}
}

// TestUnaryDoesNotApplyOutsideTriggerPositions covers the deliberate
// restriction: a unary expression anywhere else — a call argument, a return
// value, a composite literal element — is left alone, and the unary expression
// itself is never the matched node.
func TestUnaryDoesNotApplyOutsideTriggerPositions(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{name: "call argument", src: "g(!a)"},
		{name: "index expression", src: "_ = xs[-i]"},
		{name: "composite literal element", src: "_ = []bool{!a}"},
	}

	var m unary

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, file := parseFunc(t, tt.src)

			// The unary expression itself must never match: this operator
			// matches containers, not the leaf.
			for _, expr := range findNodes[*ast.UnaryExpr](file) {
				if m.Applies(expr) {
					t.Error("Applies() = true for the *ast.UnaryExpr itself")
				}

				if got := m.Mutate(expr); got != nil {
					t.Errorf("Mutate(*ast.UnaryExpr) = %v, want nil", got)
				}
			}

			// Nor may any node in the snippet offer a mutation, since none of
			// the three trigger positions holds a unary expression here.
			ast.Inspect(file, func(node ast.Node) bool {
				if node == nil {
					return false
				}

				if m.Applies(node) {
					t.Errorf("Applies() = true for %T", node)
				}

				if got := m.Mutate(node); got != nil {
					t.Errorf("Mutate(%T) = %v, want nil", node, got)
				}

				return true
			})
		})
	}
}
