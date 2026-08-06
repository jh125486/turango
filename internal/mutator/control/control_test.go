package control_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
	"github.com/jh125486/turango/internal/mutator/control"
)

// bodyCase describes one operator scenario: parse src, walk to the first node
// the operator targets, and check what the operator makes of it.
type bodyCase struct {
	name string
	src  string
	// applies is the expected Applies result for the target node.
	applies bool
	// want is the printed file after the mutation is applied. It is only
	// consulted when applies is true.
	want string
}

// firstNode returns the first node in file for which match reports true,
// walking in source order.
func firstNode(t *testing.T, file *ast.File, match func(ast.Node) bool) ast.Node {
	t.Helper()

	var found ast.Node

	ast.Inspect(file, func(node ast.Node) bool {
		if found != nil || node == nil {
			return false
		}
		if match(node) {
			found = node

			return false
		}

		return true
	})

	if found == nil {
		t.Fatal("no target node found in source")
	}

	return found
}

// render prints file, which is how the mutation engine materialises a mutant.
func render(t *testing.T, fset *token.FileSet, file *ast.File) string {
	t.Helper()

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("printer.Fprint() error = %v", err)
	}

	return buf.String()
}

// isIfStmt and isCaseClause are the node matchers the operators key off.
func isIfStmt(node ast.Node) bool { _, ok := node.(*ast.IfStmt); return ok }

func isCaseClause(node ast.Node) bool { _, ok := node.(*ast.CaseClause); return ok }

// run exercises the full engine contract for each case: Applies as a
// pre-filter, Mutate as a non-mutating query, then Apply and Revert.
func run(t *testing.T, m mutator.Mutator, match func(ast.Node) bool, wantDescription string, cases []bodyCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()

			file, err := parser.ParseFile(fset, "src.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile() error = %v", err)
			}

			node := firstNode(t, file, match)

			if got := m.Applies(node); got != tc.applies {
				t.Fatalf("Applies() = %v, want %v", got, tc.applies)
			}

			before := render(t, fset, file)
			mutations := m.Mutate(node)

			if !tc.applies {
				if mutations != nil {
					t.Fatalf("Mutate() = %v, want nil when Applies is false", mutations)
				}

				return
			}

			if len(mutations) != 1 {
				t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
			}
			if got := mutations[0].Description; got != wantDescription {
				t.Errorf("Description = %q, want %q", got, wantDescription)
			}
			if got := render(t, fset, file); got != before {
				t.Errorf("Mutate() modified the AST before Apply:\ngot:\n%s\nwant:\n%s", got, before)
			}

			mutations[0].Apply()

			if got := render(t, fset, file); got != tc.want {
				t.Errorf("after Apply:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}

			mutations[0].Revert()

			if got := render(t, fset, file); got != before {
				t.Errorf("after Revert:\ngot:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestIf(t *testing.T) {
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

func TestElse(t *testing.T) {
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

func TestCase(t *testing.T) {
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

// TestAppliesRejectsForeignNodes locks in the hot-path pre-filter behaviour:
// every operator must reject nodes it does not own, including the other
// operators' node types.
func TestAppliesRejectsForeignNodes(t *testing.T) {
	nodes := map[string]ast.Node{
		"ident":     ast.NewIdent("x"),
		"forStmt":   &ast.ForStmt{Body: &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}}},
		"blockStmt": &ast.BlockStmt{List: []ast.Stmt{&ast.EmptyStmt{}}},
		"caseClause": &ast.CaseClause{
			Body: []ast.Stmt{&ast.EmptyStmt{}},
		},
	}

	mutators := []mutator.Mutator{&control.If{}, &control.Else{}}

	for _, m := range mutators {
		for name, node := range nodes {
			t.Run(m.Name()+"/"+name, func(t *testing.T) {
				if m.Applies(node) {
					t.Errorf("Applies(%s) = true, want false", name)
				}
				if got := m.Mutate(node); got != nil {
					t.Errorf("Mutate(%s) = %v, want nil", name, got)
				}
			})
		}
	}

	caseMutator := &control.Case{}

	for _, name := range []string{"ident", "forStmt", "blockStmt"} {
		t.Run(caseMutator.Name()+"/"+name, func(t *testing.T) {
			if caseMutator.Applies(nodes[name]) {
				t.Errorf("Applies(%s) = true, want false", name)
			}
			if got := caseMutator.Mutate(nodes[name]); got != nil {
				t.Errorf("Mutate(%s) = %v, want nil", name, got)
			}
		})
	}
}

// TestRegistration checks the operators are reachable by the exact names the
// -mutateoperators flag accepts, and that each reports the name it was
// registered under.
func TestRegistration(t *testing.T) {
	for _, name := range []string{"control/if", "control/else", "control/case"} {
		t.Run(name, func(t *testing.T) {
			m, err := mutator.New(name)
			if err != nil {
				t.Fatalf("mutator.New(%q) error = %v", name, err)
			}
			if got := m.Name(); got != name {
				t.Errorf("Name() = %q, want %q", got, name)
			}
		})
	}
}

// TestRevertRestoresTheOriginalSlice checks Revert restores the exact slice
// header, not a rebuilt equivalent: the engine reuses the AST across the whole
// walk, so identity matters.
func TestRevertRestoresTheOriginalSlice(t *testing.T) {
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
