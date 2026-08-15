package control_test

import (
	"go/ast"
	"testing"

	"github.com/jh125486/turango/internal/mutator/control"
)

// isCaseClause is the node matcher the Case operator keys off.
func isCaseClause(node ast.Node) bool { _, ok := node.(*ast.CaseClause); return ok }

func TestCase(t *testing.T) {
	t.Parallel()

	run(t, &control.Case{}, isCaseClause, "remove case body", []bodyCase{
		{
			name: "removes an expression switch case body",
			src: `package p

func f(i int) {
	switch i {
	case 1:
		g()
	default:
		h()
	}
}
`,
			applies: true,
			want: `package p

func f(i int) {
	switch i {
	case 1:

	default:
		h()
	}
}
`,
		},
		{
			name: "removes a type switch case body",
			src: `package p

func f(v any) {
	switch t := v.(type) {
	case int:
		g(t)
	}
}
`,
			applies: true,
			want: `package p

func f(v any) {
	switch t := v.(type) {
	case int:

	}
}
`,
		},
		{
			name: "skips an empty case body",
			src: `package p

func f(i int) {
	switch i {
	case 1:
	default:
		h()
	}
}
`,
			applies: false,
		},
	})
}

// TestCaseDefaultClause covers the default clause specifically: it is an
// *ast.CaseClause with a nil List and gets no special treatment.
func TestCaseDefaultClause(t *testing.T) {
	t.Parallel()

	isDefault := func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)

		return ok && clause.List == nil
	}

	run(t, &control.Case{}, isDefault, "remove case body", []bodyCase{
		{
			name: "removes the default body",
			src: `package p

func f(i int) {
	switch i {
	case 1:
		g()
	default:
		h()
	}
}
`,
			applies: true,
			want: `package p

func f(i int) {
	switch i {
	case 1:
		g()
	default:

	}
}
`,
		},
		{
			name: "skips an empty default body",
			src: `package p

func f(i int) {
	switch i {
	case 1:
		g()
	default:
	}
}
`,
			applies: false,
		},
	})
}

// TestCaseApplies locks in the hot-path pre-filter behaviour: Case must
// reject nodes it does not own. Unlike If and Else, a generic non-empty
// *ast.CaseClause is not foreign to Case, so it is excluded here.
func TestCaseApplies(t *testing.T) {
	t.Parallel()

	nodes := map[string]ast.Node{
		"ident":     ast.NewIdent("x"),
		"forStmt":   &ast.ForStmt{Body: &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}}},
		"blockStmt": &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}},
	}

	m := &control.Case{}

	for name, node := range nodes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if m.Applies(node) {
				t.Errorf("Applies(%s) = true, want false", name)
			}
			if got := m.Mutate(node); got != nil {
				t.Errorf("Mutate(%s) = %v, want nil", name, got)
			}
		})
	}
}
