package statement_test

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
	"github.com/jh125486/turango/internal/mutator/statement"
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

// TestRegistered covers the operator's registration under RemoverName: that
// mutator.New resolves it and that the resolved instance reports the same
// name back.
func TestRegistered(t *testing.T) {
	t.Parallel()

	m, err := mutator.New(statement.RemoverName)
	if err != nil {
		t.Fatalf("New(%q) error = %v", statement.RemoverName, err)
	}

	if m.Name() != "statement/remover" {
		t.Errorf("Name() = %q, want %q", m.Name(), "statement/remover")
	}
}

// TestApplies covers Remover.Applies: the table exercises the statement-kind
// and assignment-token combinations the pre-filter must tell apart (folded in
// from what was a standalone token table, since every token maps directly to
// an Applies outcome through a one-statement block), and a "rejects
// non-containers" subtest covers the node kinds Applies must ignore,
// including the statement kinds the operator removes (those are only ever
// reached through their container).
func TestApplies(t *testing.T) {
	t.Parallel()

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
		{
			name: "block with a lone add-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx += 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone sub-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx -= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone mul-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx *= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone quo-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx /= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone rem-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx %= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone and-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx &= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone or-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx |= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone xor-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx ^= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone shl-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx <<= 1\n}\n",
			want: true,
		},
		{
			name: "block with a lone shr-assign",
			src:  "package p\n\nfunc f(x int) {\n\tx >>= 1\n}\n",
			want: true,
		},
	}

	m := &statement.Remover{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, file := parseSrc(t, tt.src)
			block := funcBody(t, file)

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

	t.Run("rejects non-containers", func(t *testing.T) {
		t.Parallel()

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

		for name, node := range nodes {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				if m.Applies(node) {
					t.Errorf("Applies(%T) = true, want false", node)
				}
				if got := m.Mutate(node); got != nil {
					t.Errorf("Mutate(%T) = %v, want nil", node, descriptions(got))
				}
			})
		}
	})
}

// TestMutate covers Remover.Mutate: the table pins the exact set of mutations
// produced for a block and a case clause — one per removable statement, in
// source order, and none for a short variable declaration, an if statement, a
// return, or a nested block's own statements — and further subtests cover
// properties that do not fit that table: that Mutate itself leaves the AST
// untouched, that each returned mutation's Apply/Revert round-trips the
// source, that a mutation can be cycled more than once, and that a long
// statement's description gets truncated.
func TestMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		node func(*testing.T, *ast.File) ast.Node
		want []string
	}{
		{name: "block", src: mixedSrc, node: func(t *testing.T, f *ast.File) ast.Node { return funcBody(t, f) }, want: []string{"remove statement: x++", "remove statement: foo()", "remove statement: x = 2", "remove statement: x += 3", "remove statement: y--"}},
		{name: "nested block", src: mixedSrc, node: func(t *testing.T, f *ast.File) ast.Node {
			return findNode(t, f, func(n ast.Node) bool { _, ok := n.(*ast.IfStmt); return ok }).(*ast.IfStmt).Body
		}, want: []string{"remove statement: bar()"}},
		{name: "case clause", src: switchSrc, node: func(t *testing.T, f *ast.File) ast.Node { return caseClause(t, f, 0) }, want: []string{"remove statement: foo()", "remove statement: x++"}},
		{name: "default clause with only a short var decl and a blank assign", src: switchSrc, node: func(t *testing.T, f *ast.File) ast.Node { return caseClause(t, f, 1) }, want: []string{"remove statement: _ = z"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			testStatementMutationCase(t, tt.src, tt.node, tt.want)
		})
	}

	t.Run("does not touch the AST", testStatementMutationUntouched)

	// "apply revert round trip" applies each mutation independently, compares
	// the printed mutant against the source with exactly that line removed,
	// then reverts and confirms the source is restored byte for byte. The two
	// shapes (block, case clause) each parse their own file and so run in
	// parallel; the per-mutation subtests within a shape do not, since they
	// apply and revert in sequence against that one shared AST — running them
	// concurrently would let one goroutine's render() observe another's
	// half-applied mutation.
	t.Run("apply revert round trip", testStatementMutationRoundTrip)

	// "apply revert is repeatable" confirms a mutation can be cycled more than
	// once, since the engine reuses one AST for a whole walk.
	t.Run("apply revert is repeatable", testStatementMutationRepeatable)

	// "truncates long statements" keeps a pathological line from swamping a
	// report row.
	t.Run("truncates long statements", testStatementMutationTruncation)
}

func testStatementMutationCase(t *testing.T, src string, node func(*testing.T, *ast.File) ast.Node, want []string) {
	t.Helper()
	_, file := parseSrc(t, src)
	target := node(t, file)
	m := &statement.Remover{}
	if !m.Applies(target) {
		t.Fatalf("Applies(%T) = false, want true", target)
	}
	if got := descriptions(m.Mutate(target)); !reflect.DeepEqual(got, want) {
		t.Errorf("Mutate() descriptions =\n%v\nwant\n%v", got, want)
	}
}

func testStatementMutationUntouched(t *testing.T) {
	t.Parallel()
	fset, file := parseSrc(t, mixedSrc)
	block := funcBody(t, file)
	before := append([]ast.Stmt(nil), block.List...)
	if mutations := (&statement.Remover{}).Mutate(block); len(mutations) == 0 {
		t.Fatal("Mutate() returned no mutations")
	}
	if !reflect.DeepEqual(block.List, before) {
		t.Error("Mutate() modified the statement list")
	}
	if got := render(t, fset, file); got != mixedSrc {
		t.Errorf("Mutate() changed the printed source:\n%s", got)
	}
}

func testStatementMutationRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		node      func(*testing.T, *ast.File) ast.Node
		blanks    []string
	}{
		{name: "block", src: mixedSrc, node: func(t *testing.T, f *ast.File) ast.Node { return funcBody(t, f) }, blanks: []string{"x++", "foo()", "x = 2", "x += 3", "y--"}},
		{name: "case clause", src: switchSrc, node: func(t *testing.T, f *ast.File) ast.Node { return caseClause(t, f, 0) }, blanks: []string{"foo()", "x++"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { t.Parallel(); testStatementMutationRoundTripCase(t, tc.src, tc.node, tc.blanks) })
	}
}

func testStatementMutationRoundTripCase(t *testing.T, src string, node func(*testing.T, *ast.File) ast.Node, blanks []string) {
	t.Helper()
	fset, file := parseSrc(t, src)
	mutations := (&statement.Remover{}).Mutate(node(t, file))
	if len(mutations) != len(blanks) {
		t.Fatalf("Mutate() returned %d mutations, want %d", len(mutations), len(blanks))
	}
	for i, mutation := range mutations {
		want := blankLine(t, src, blanks[i])
		mutation.Apply()
		if got := render(t, fset, file); got != want {
			t.Errorf("after Apply, source =\n%q\nwant\n%q", got, want)
		}
		mutation.Revert()
		if got := render(t, fset, file); got != src {
			t.Errorf("after Revert, source =\n%q\nwant\n%q", got, src)
		}
	}
}

func testStatementMutationRepeatable(t *testing.T) {
	t.Parallel()
	fset, file := parseSrc(t, mixedSrc)
	mutations := (&statement.Remover{}).Mutate(funcBody(t, file))
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

func testStatementMutationTruncation(t *testing.T) {
	t.Parallel()
	src := "package p\n\nfunc f() {\n\tfoo(\"" + strings.Repeat("a", 200) + "\")\n}\n"
	_, file := parseSrc(t, src)
	mutations := (&statement.Remover{}).Mutate(funcBody(t, file))
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}
	got := mutations[0].Description
	if !strings.HasSuffix(got, "...") {
		t.Errorf("Description = %q, want a truncated value ending in %q", got, "...")
	}
	const descriptionLimit = 60
	if want := len("remove statement: ") + descriptionLimit + len("..."); len(got) != want {
		t.Errorf("len(Description) = %d, want %d", len(got), want)
	}
}
