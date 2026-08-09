package control_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/jh125486/turango/internal/mutator/control"
)

func TestIf(t *testing.T) {
	t.Parallel()

	run(t, &control.If{}, isIfStmt, "remove if body", []bodyCase{
		{
			name: "removes the body",
			src: `package p

func f(c bool) {
	if c {
		g()
	}
}
`,
			applies: true,
			want: `package p

func f(c bool) {
	if c {

	}
}
`,
		},
		{
			name: "removes a multi-statement body but keeps the else branch",
			src: `package p

func f(c bool) {
	if c {
		g()
		h()
	} else {
		i()
	}
}
`,
			applies: true,
			want: `package p

func f(c bool) {
	if c {

	} else {
		i()
	}
}
`,
		},
		{
			name: "skips an already-empty body",
			src: `package p

func f(c bool) {
	if c {
	}
}
`,
			applies: false,
		},
	})
}

// TestIfRejectsForeignNodes locks in the hot-path pre-filter behaviour: If
// must reject nodes it does not own, including other operators' node types.
func TestIfRejectsForeignNodes(t *testing.T) {
	t.Parallel()

	nodes := map[string]ast.Node{
		"ident":      ast.NewIdent("x"),
		"forStmt":    &ast.ForStmt{Body: &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}}},
		"blockStmt":  &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}},
		"caseClause": &ast.CaseClause{Body: []ast.Stmt{&ast.EmptyStmt{}}},
	}

	m := &control.If{}

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

// TestRevertRestoresTheOriginalSlice checks Revert restores the exact slice
// header, not a rebuilt equivalent: the engine reuses the AST across the whole
// walk, so identity matters.
func TestRevertRestoresTheOriginalSlice(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	src := `package p

func f(c bool) {
	if c {
		g()
		h()
	}
}
`

	file, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile() error = %v", err)
	}

	stmt, ok := firstNode(t, file, isIfStmt).(*ast.IfStmt)
	if !ok {
		t.Fatal("target node is not an *ast.IfStmt")
	}

	original := stmt.Body.List

	mutations := (&control.If{}).Mutate(stmt)
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}

	mutations[0].Apply()

	if len(stmt.Body.List) != 0 {
		t.Fatalf("after Apply, body has %d statements, want 0", len(stmt.Body.List))
	}

	mutations[0].Revert()

	if len(stmt.Body.List) != len(original) {
		t.Fatalf("after Revert, body has %d statements, want %d", len(stmt.Body.List), len(original))
	}

	for i := range original {
		if stmt.Body.List[i] != original[i] {
			t.Errorf("after Revert, statement %d is not the original node", i)
		}
	}
}
