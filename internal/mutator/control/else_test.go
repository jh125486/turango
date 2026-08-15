package control_test

import (
	"go/ast"
	"testing"

	"github.com/jh125486/turango/internal/mutator/control"
)

func TestElse(t *testing.T) {
	t.Parallel()

	run(t, &control.Else{}, isIfStmt, "remove else body", []bodyCase{
		{
			name: "removes the else body and keeps the if body",
			src: `package p

func f(c bool) {
	if c {
		g()
	} else {
		h()
	}
}
`,
			applies: true,
			want: `package p

func f(c bool) {
	if c {
		g()
	} else {

	}
}
`,
		},
		{
			name: "skips a nil else",
			src: `package p

func f(c bool) {
	if c {
		g()
	}
}
`,
			applies: false,
		},
		{
			name: "skips an else if chain",
			src: `package p

func f(c, d bool) {
	if c {
		g()
	} else if d {
		h()
	}
}
`,
			applies: false,
		},
		{
			name: "skips an else if chain that ends in a block",
			src: `package p

func f(c, d bool) {
	if c {
		g()
	} else if d {
		h()
	} else {
		i()
	}
}
`,
			applies: false,
		},
		{
			name: "skips an already-empty else body",
			src: `package p

func f(c bool) {
	if c {
		g()
	} else {
	}
}
`,
			applies: false,
		},
	})
}

// TestElseApplies locks in the hot-path pre-filter behaviour: Else must
// reject nodes it does not own, including other operators' node types.
func TestElseApplies(t *testing.T) {
	t.Parallel()

	nodes := map[string]ast.Node{
		"ident":      ast.NewIdent("x"),
		"forStmt":    &ast.ForStmt{Body: &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}}},
		"blockStmt":  &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}},
		"caseClause": &ast.CaseClause{Body: []ast.Stmt{&ast.EmptyStmt{}}},
	}

	m := &control.Else{}

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
