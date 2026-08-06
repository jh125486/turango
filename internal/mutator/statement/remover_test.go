package statement

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/jh125486/turango/internal/mutator"
)

// mixedSrc holds every removable statement kind alongside statements the
// operator must leave alone: a short variable declaration (deleting it would
// un-declare x), an if statement, and a return.
const mixedSrc = `package p

func f() {
	x := 1
	x++
	foo()
	x = 2
	x += 3
	y--
	if x > 0 {
		bar()
	}
	return
}
`

// switchSrc exercises *ast.CaseClause, whose statement list is Body rather than
// List. Its default clause mixes a short variable declaration (never removable)
// with a plain assignment (removable).
const switchSrc = `package p

func f(x int) {
	switch x {
	case 1:
		foo()
		x++
	default:
		z := 0
		_ = z
	}
}
`

// parseSrc parses src into a file, failing the test if it is not valid Go.
func parseSrc(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	return fset, file
}

// funcBody returns the body of the first function declaration in file.
func funcBody(t *testing.T, file *ast.File) *ast.BlockStmt {
	t.Helper()

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			return fn.Body
		}
	}

	t.Fatal("no function declaration in the fixture")

	return nil
}

// findNode returns the first node in file for which match reports true.
func findNode(t *testing.T, file *ast.File, match func(ast.Node) bool) ast.Node {
	t.Helper()

	var found ast.Node

	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil || n == nil {
			return false
		}
		if match(n) {
			found = n

			return false
		}

		return true
	})

	if found == nil {
		t.Fatal("no matching node in the fixture")
	}

	return found
}

// caseClause returns the index'th case clause of the first switch in file.
func caseClause(t *testing.T, file *ast.File, index int) *ast.CaseClause {
	t.Helper()

	sw, ok := findNode(t, file, func(n ast.Node) bool {
		_, ok := n.(*ast.SwitchStmt)

		return ok
	}).(*ast.SwitchStmt)
	if !ok {
		t.Fatal("first match was not a switch statement")
	}

	clause, ok := sw.Body.List[index].(*ast.CaseClause)
	if !ok {
		t.Fatalf("clause %d is a %T, want *ast.CaseClause", index, sw.Body.List[index])
	}

	return clause
}

// render prints file, which is how the engine materialises a mutant.
func render(t *testing.T, fset *token.FileSet, file *ast.File) string {
	t.Helper()

	var buf bytes.Buffer

	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("printer.Fprint() error = %v", err)
	}

	return buf.String()
}

// blankLine returns src with the unique line whose trimmed text is want
// replaced by an empty line, which is what go/printer emits for a statement
// swapped out for an *ast.EmptyStmt.
func blankLine(t *testing.T, src, want string) string {
	t.Helper()

	lines := strings.Split(src, "\n")

	found := -1

	for i, line := range lines {
		if strings.TrimSpace(line) != want {
			continue
		}
		if found >= 0 {
			t.Fatalf("line %q is not unique in the fixture", want)
		}

		found = i
	}

	if found < 0 {
		t.Fatalf("line %q is not in the fixture", want)
	}

	lines[found] = ""

	return strings.Join(lines, "\n")
}

func descriptions(mutations []mutator.Mutation) []string {
	got := make([]string, len(mutations))
	for i, m := range mutations {
		got[i] = m.Description
	}

	return got
}

func TestRemoverIsRegistered(t *testing.T) {
	m, err := mutator.New(RemoverName)
	if err != nil {
		t.Fatalf("New(%q) error = %v", RemoverName, err)
	}

	if m.Name() != "statement/remover" {
		t.Errorf("Name() = %q, want %q", m.Name(), "statement/remover")
	}
}

// TestApplies covers the pre-filter: containers holding a removable statement
// match, everything else does not.
func TestApplies(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "block with removable statements",
			src:  mixedSrc,
			want: true,
		},
		{
			name: "block with only a short var decl and a return",
			src:  "package p\n\nfunc f() {\n\ty := 2\n\treturn\n}\n",
			want: false,
		},
		{
			name: "block with only control flow",
			src:  "package p\n\nfunc f(x int) {\n\tif x > 0 {\n\t}\n\tfor {\n\t}\n}\n",
			want: false,
		},
		{
			name: "empty block",
			src:  "package p\n\nfunc f() {\n}\n",
			want: false,
		},
		{
			name: "block with a lone assignment",
			src:  "package p\n\nfunc f(x int) {\n\tx = 1\n}\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, file := parseSrc(t, tt.src)
			block := funcBody(t, file)

			m := &Remover{}

			if got := m.Applies(block); got != tt.want {
				t.Errorf("Applies() = %v, want %v", got, tt.want)
			}

			if !tt.want {
				if got := m.Mutate(block); got != nil {
					t.Errorf("Mutate() = %v, want nil when Applies is false", descriptions(got))
				}
			}
		})
	}
}

// TestAppliesRejectsNonContainers guards the hot path: the operator must ignore
// every node that is not a block or a case clause, including the statement
// kinds it removes (those are only ever reached through their container).
func TestAppliesRejectsNonContainers(t *testing.T) {
	_, file := parseSrc(t, mixedSrc)
	block := funcBody(t, file)

	nodes := map[string]ast.Node{
		"ident":       ast.NewIdent("x"),
		"file":        file,
		"func decl":   file.Decls[0],
		"inc dec":     block.List[1],
		"expr stmt":   block.List[2],
		"assign stmt": block.List[3],
		"if stmt":     block.List[6],
		"return stmt": block.List[7],
	}

	m := &Remover{}

	for name, node := range nodes {
		t.Run(name, func(t *testing.T) {
			if m.Applies(node) {
				t.Errorf("Applies(%T) = true, want false", node)
			}
			if got := m.Mutate(node); got != nil {
				t.Errorf("Mutate(%T) = %v, want nil", node, descriptions(got))
			}
		})
	}
}

// TestMutateDescriptions pins the exact set of mutations produced for a block:
// one per removable statement, in source order, and none for the short variable
// declaration, the if statement, the return, or the if body's own statement
// (which belongs to the nested block, not this one).
func TestMutateDescriptions(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		clause func(*testing.T, *ast.File) ast.Node
		want   []string
	}{
		{
			name: "block",
			src:  mixedSrc,
			clause: func(t *testing.T, file *ast.File) ast.Node {
				return funcBody(t, file)
			},
			want: []string{
				"remove statement: x++",
				"remove statement: foo()",
				"remove statement: x = 2",
				"remove statement: x += 3",
				"remove statement: y--",
			},
		},
		{
			name: "nested block",
			src:  mixedSrc,
			clause: func(t *testing.T, file *ast.File) ast.Node {
				return findNode(t, file, func(n ast.Node) bool {
					ifStmt, ok := n.(*ast.IfStmt)
					if !ok {
						return false
					}

					return ifStmt.Body != nil
				}).(*ast.IfStmt).Body
			},
			want: []string{"remove statement: bar()"},
		},
		{
			name: "case clause",
			src:  switchSrc,
			clause: func(t *testing.T, file *ast.File) ast.Node {
				return caseClause(t, file, 0)
			},
			want: []string{
				"remove statement: foo()",
				"remove statement: x++",
			},
		},
		{
			name: "default clause with only a short var decl and a blank assign",
			src:  switchSrc,
			clause: func(t *testing.T, file *ast.File) ast.Node {
				return caseClause(t, file, 1)
			},
			// _ = z is a plain assignment, so it is removable; z := 0 is not.
			want: []string{"remove statement: _ = z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, file := parseSrc(t, tt.src)
			node := tt.clause(t, file)

			m := &Remover{}

			if !m.Applies(node) {
				t.Fatalf("Applies(%T) = false, want true", node)
			}

			got := descriptions(m.Mutate(node))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Mutate() descriptions =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

// TestMutateDoesNotTouchTheAST locks in the contract that only Apply edits the
// tree.
func TestMutateDoesNotTouchTheAST(t *testing.T) {
	fset, file := parseSrc(t, mixedSrc)
	block := funcBody(t, file)

	before := append([]ast.Stmt(nil), block.List...)

	if mutations := (&Remover{}).Mutate(block); len(mutations) == 0 {
		t.Fatal("Mutate() returned no mutations")
	}

	if !reflect.DeepEqual(block.List, before) {
		t.Error("Mutate() modified the statement list")
	}

	if got := render(t, fset, file); got != mixedSrc {
		t.Errorf("Mutate() changed the printed source:\n%s", got)
	}
}

// TestApplyRevertRoundTrip applies each mutation independently, compares the
// printed mutant against the source with exactly that line removed, then reverts
// and confirms the source is restored byte for byte.
func TestApplyRevertRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		node   func(*testing.T, *ast.File) ast.Node
		blanks []string
	}{
		{
			name: "block",
			src:  mixedSrc,
			node: func(t *testing.T, file *ast.File) ast.Node {
				return funcBody(t, file)
			},
			blanks: []string{"x++", "foo()", "x = 2", "x += 3", "y--"},
		},
		{
			name: "case clause",
			src:  switchSrc,
			node: func(t *testing.T, file *ast.File) ast.Node {
				return caseClause(t, file, 0)
			},
			blanks: []string{"foo()", "x++"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseSrc(t, tt.src)

			mutations := (&Remover{}).Mutate(tt.node(t, file))
			if len(mutations) != len(tt.blanks) {
				t.Fatalf("Mutate() returned %d mutations, want %d", len(mutations), len(tt.blanks))
			}

			for i, m := range mutations {
				t.Run(tt.blanks[i], func(t *testing.T) {
					want := blankLine(t, tt.src, tt.blanks[i])

					m.Apply()

					if got := render(t, fset, file); got != want {
						t.Errorf("after Apply, source =\n%q\nwant\n%q", got, want)
					}

					m.Revert()

					if got := render(t, fset, file); got != tt.src {
						t.Errorf("after Revert, source =\n%q\nwant\n%q", got, tt.src)
					}
				})
			}
		})
	}
}

// TestApplyRevertIsRepeatable confirms a mutation can be cycled more than once,
// since the engine reuses one AST for a whole walk.
func TestApplyRevertIsRepeatable(t *testing.T) {
	fset, file := parseSrc(t, mixedSrc)

	mutations := (&Remover{}).Mutate(funcBody(t, file))
	want := blankLine(t, mixedSrc, "foo()")

	for range 3 {
		mutations[1].Apply()

		if got := render(t, fset, file); got != want {
			t.Fatalf("after Apply, source =\n%q\nwant\n%q", got, want)
		}

		mutations[1].Revert()

		if got := render(t, fset, file); got != mixedSrc {
			t.Fatalf("after Revert, source =\n%q\nwant\n%q", got, mixedSrc)
		}
	}
}

// TestRemovableAssignmentTokens checks the token filter directly: every
// assignment operator is removable except :=.
func TestRemovableAssignmentTokens(t *testing.T) {
	tests := []struct {
		tok  token.Token
		want bool
	}{
		{token.ASSIGN, true},
		{token.ADD_ASSIGN, true},
		{token.SUB_ASSIGN, true},
		{token.MUL_ASSIGN, true},
		{token.QUO_ASSIGN, true},
		{token.REM_ASSIGN, true},
		{token.AND_ASSIGN, true},
		{token.OR_ASSIGN, true},
		{token.XOR_ASSIGN, true},
		{token.SHL_ASSIGN, true},
		{token.SHR_ASSIGN, true},
		{token.DEFINE, false},
	}

	for _, tt := range tests {
		t.Run(tt.tok.String(), func(t *testing.T) {
			stmt := &ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent("x")},
				Tok: tt.tok,
				Rhs: []ast.Expr{ast.NewIdent("y")},
			}

			if got := removable(stmt); got != tt.want {
				t.Errorf("removable(%s) = %v, want %v", tt.tok, got, tt.want)
			}
		})
	}
}

// TestDescribeTruncatesLongStatements keeps a pathological line from swamping a
// report row.
func TestDescribeTruncatesLongStatements(t *testing.T) {
	src := "package p\n\nfunc f() {\n\tfoo(\"" + strings.Repeat("a", 200) + "\")\n}\n"

	_, file := parseSrc(t, src)

	mutations := (&Remover{}).Mutate(funcBody(t, file))
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}

	got := mutations[0].Description

	if !strings.HasSuffix(got, "...") {
		t.Errorf("Description = %q, want a truncated value ending in %q", got, "...")
	}

	if want := len("remove statement: ") + descriptionLimit + len("..."); len(got) != want {
		t.Errorf("len(Description) = %d, want %d", len(got), want)
	}
}
