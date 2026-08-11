// Whitebox: suppress.go exports no identifiers — suppressions,
// scanSuppressions, parseDirective and anchored are all unexported — so
// testing them, and testing the mutateFile cascade that consults them,
// requires direct package access. Blackbox coverage of the same cascade
// behaviour through the exported Run lives in engine_test.go's
// TestRunSuppressesCompoundStatement.
package mutate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/mutator"
)

// parseSuppressSrc parses src with comments retained, which is the only mode
// suppression works in.
func parseSuppressSrc(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	return fset, file
}

// firstNode returns the first node of type T in file, in walk order.
func firstNode[T ast.Node](t *testing.T, file *ast.File) T {
	t.Helper()

	var (
		found T
		ok    bool
	)

	ast.Inspect(file, func(n ast.Node) bool {
		if ok || n == nil {
			return false
		}

		if typed, is := n.(T); is {
			found, ok = typed, true

			return false
		}

		return true
	})

	if !ok {
		t.Fatalf("no %T in the fixture", found)
	}

	return found
}

// TestParseDirective pins exactly which comment texts are directives. The
// negative half matters more than the positive: anything that silently disables
// mutation testing without the author meaning to is a hole in the tool.
func TestParseDirective(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		text       string
		wantReason string
		want       bool
	}{
		"bare":                     {text: "//nomutant", want: true},
		"spaced":                   {text: "// nomutant", want: true},
		"trailing space":           {text: "//  nomutant  ", want: true},
		"with reason":              {text: "//nomutant:generated code", wantReason: "generated code", want: true},
		"spaced reason":            {text: "// nomutant: known flaky under -race", wantReason: "known flaky under -race", want: true},
		"reason containing colons": {text: "// nomutant: see issue: #12", wantReason: "see issue: #12", want: true},
		"empty reason":             {text: "// nomutant:", want: true},

		"block comment":          {text: "/* nomutant */"},
		"block comment reason":   {text: "/* nomutant: nope */"},
		"prose starting with it": {text: "// nomutant directives go here"},
		"longer word":            {text: "// nomutants everywhere"},
		"other directive":        {text: "//nolint:all"},
		"lint ignore":            {text: "//lint:ignore SA1000 because"},
		"ordinary comment":       {text: "// Total sums the positive values in vs."},
		"go directive":           {text: "//go:generate stringer -type=Status"},
		"empty":                  {text: "//"},
		"suffix match":           {text: "// notnomutant"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reason, ok := parseDirective(tt.text)
			if ok != tt.want {
				t.Fatalf("parseDirective(%q) ok = %v, want %v", tt.text, ok, tt.want)
			}

			if reason != tt.wantReason {
				t.Errorf("parseDirective(%q) reason = %q, want %q", tt.text, reason, tt.wantReason)
			}
		})
	}
}

// TestScanSuppressions covers the file-level scan: every directive is indexed by
// its own line, and nothing else in the file is.
func TestScanSuppressions(t *testing.T) {
	t.Parallel()

	const src = `package p

// Doc comment, not a directive.
func f(i int) int {
	// nomutant
	i++
	i-- // nomutant: measured elsewhere
	/* nomutant */
	i++
	//nolint:all
	return i
}
`

	fset, file := parseSuppressSrc(t, src)

	got := scanSuppressions(fset, file)

	want := suppressions{
		5: "",
		7: "measured elsewhere",
	}

	if !reflect.DeepEqual(map[int]string(got), map[int]string(want)) {
		t.Errorf("scanSuppressions() = %v, want %v", got, want)
	}
}

// TestScanSuppressionsWithoutDirectives keeps the common case allocation-free
// and, more importantly, keeps anchored's fast path honest.
func TestScanSuppressionsWithoutDirectives(t *testing.T) {
	t.Parallel()

	fset, file := parseSuppressSrc(t, "package p\n\n// f increments i.\nfunc f(i int) int {\n\ti++\n\treturn i\n}\n")

	if got := scanSuppressions(fset, file); len(got) != 0 {
		t.Errorf("scanSuppressions() = %v, want empty", got)
	}
}

// TestAnchored is the statement-anchoring rule in full: leading and trailing
// lines match, interior lines do not, and non-directive comments never match.
func TestAnchored(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		src        string
		node       func(*testing.T, *ast.File) ast.Node
		wantReason string
		want       bool
	}{
		"leading comment": {
			src:  "package p\n\nfunc f(i int) {\n\t// nomutant\n\ti++\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			want: true,
		},
		"trailing comment": {
			src:  "package p\n\nfunc f(i int) {\n\ti++ // nomutant\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			want: true,
		},
		"leading comment with a reason": {
			src:        "package p\n\nfunc f(i int) {\n\t// nomutant: telemetry only\n\ti++\n}\n",
			node:       func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			wantReason: "telemetry only",
			want:       true,
		},
		"trailing comment with a reason": {
			src:        "package p\n\nfunc f(i int) {\n\ti++ //nomutant:telemetry only\n}\n",
			node:       func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			wantReason: "telemetry only",
			want:       true,
		},
		"blank line between comment and statement": {
			src:  "package p\n\nfunc f(i int) {\n\t// nomutant\n\n\ti++\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			want: false,
		},
		"comment above the previous statement": {
			src:  "package p\n\nfunc f(i int) {\n\t// nomutant\n\ti--\n\ti += 2\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.AssignStmt](t, file) },
			want: false,
		},
		"block comment is not a directive": {
			src:  "package p\n\nfunc f(i int) {\n\t/* nomutant */\n\ti++\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			want: false,
		},
		"nolint is not a directive": {
			src:  "package p\n\nfunc f(i int) {\n\t//nolint:all\n\ti++\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IncDecStmt](t, file) },
			want: false,
		},
		"doc comment is not a directive": {
			src:  "package p\n\n// f increments i.\nfunc f(i int) {\n\ti++\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.FuncDecl](t, file) },
			want: false,
		},
		"compound statement anchored at its own line": {
			src:  "package p\n\nfunc f(i int) {\n\t// nomutant: hot path\n\tif i > 0 {\n\t\ti++\n\t}\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IfStmt](t, file) },
			// The reason is reported for the compound statement itself; every
			// node inside it is skipped by the walk, never re-checked here.
			wantReason: "hot path",
			want:       true,
		},
		"compound statement anchored at its closing brace": {
			src:  "package p\n\nfunc f(i int) {\n\tif i > 0 {\n\t\ti++\n\t} // nomutant\n}\n",
			node: func(t *testing.T, file *ast.File) ast.Node { return firstNode[*ast.IfStmt](t, file) },
			want: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fset, file := parseSuppressSrc(t, tt.src)

			reason, ok := scanSuppressions(fset, file).anchored(fset, tt.node(t, file))
			if ok != tt.want {
				t.Fatalf("anchored() ok = %v, want %v", ok, tt.want)
			}

			if reason != tt.wantReason {
				t.Errorf("anchored() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// TestAnchoredMultiLineCondition pins the documented first-line/last-line rule
// against the shape that tempts a looser implementation: a condition split over
// several lines. The directive must sit above the line the statement starts on;
// above an interior continuation line it suppresses nothing, and a naive
// "any line the statement spans" check would wrongly accept it.
func TestAnchoredMultiLineCondition(t *testing.T) {
	t.Parallel()

	const above = `package p

func f(a, b bool) bool {
	// nomutant
	if a &&
		b {
		return true
	}
	return false
}
`

	const interior = `package p

func f(a, b bool) bool {
	if a &&
		// nomutant
		b {
		return true
	}
	return false
}
`

	t.Run("above the first line suppresses", func(t *testing.T) {
		t.Parallel()

		fset, file := parseSuppressSrc(t, above)

		if _, ok := scanSuppressions(fset, file).anchored(fset, firstNode[*ast.IfStmt](t, file)); !ok {
			t.Error("anchored() = false for a directive above the statement's first line")
		}
	})

	t.Run("above an interior line does not suppress", func(t *testing.T) {
		t.Parallel()

		fset, file := parseSuppressSrc(t, interior)

		suppressed := scanSuppressions(fset, file)

		if len(suppressed) != 1 {
			t.Fatalf("scanSuppressions() = %v, want the directive to be scanned", suppressed)
		}

		if _, ok := suppressed.anchored(fset, firstNode[*ast.IfStmt](t, file)); ok {
			t.Error("anchored() = true for a directive on an interior continuation line")
		}

		// The condition itself ends on the line below the directive, not on it,
		// so the expression operators stay live too.
		if _, ok := suppressed.anchored(fset, firstNode[*ast.BinaryExpr](t, file)); ok {
			t.Error("anchored() = true for the condition of a non-suppressed if")
		}
	})
}

// TestAnchoredIgnoresTheFileNode covers the whole-file edge case: an *ast.File's
// Pos is the package keyword, so a directive written above the package clause
// would otherwise suppress every node in the file at once. Whole-file
// suppression is not a feature, so it is inert.
func TestAnchoredIgnoresTheFileNode(t *testing.T) {
	t.Parallel()

	const src = `// nomutant
package p

func f(i int) {
	i++
}
`

	fset, file := parseSuppressSrc(t, src)

	suppressed := scanSuppressions(fset, file)

	if _, ok := suppressed[1]; !ok {
		t.Fatalf("scanSuppressions() = %v, want the directive on line 1", suppressed)
	}

	if _, ok := suppressed.anchored(fset, file); ok {
		t.Error("anchored(*ast.File) = true: a comment above the package clause suppressed the whole file")
	}

	// Nothing else lands on it either: the func declaration is two lines below.
	if _, ok := suppressed.anchored(fset, firstNode[*ast.FuncDecl](t, file)); ok {
		t.Error("anchored(*ast.FuncDecl) = true for a directive above the package clause")
	}

	if _, ok := suppressed.anchored(fset, firstNode[*ast.IncDecStmt](t, file)); ok {
		t.Error("anchored(*ast.IncDecStmt) = true for a directive above the package clause")
	}
}

// TestAnchoredIgnoresCommentNodes covers the other self-referential case: the
// walk reaches comment groups through nodes' Doc fields, and a directive that
// matched itself would record a suppression for the comment rather than for any
// code.
func TestAnchoredIgnoresCommentNodes(t *testing.T) {
	t.Parallel()

	const src = `package p

// nomutant
func f(i int) {
	i++
}
`

	fset, file := parseSuppressSrc(t, src)

	suppressed := scanSuppressions(fset, file)

	group := firstNode[*ast.CommentGroup](t, file)

	if _, ok := suppressed.anchored(fset, group); ok {
		t.Error("anchored(*ast.CommentGroup) = true, want the directive to describe code, not itself")
	}

	if _, ok := suppressed.anchored(fset, group.List[0]); ok {
		t.Error("anchored(*ast.Comment) = true, want the directive to describe code, not itself")
	}

	// The declaration it precedes is suppressed, which is the point.
	if _, ok := suppressed.anchored(fset, firstNode[*ast.FuncDecl](t, file)); !ok {
		t.Error("anchored(*ast.FuncDecl) = false for a directive on the line above it")
	}
}

// cascadeSrc has every mutable node nested inside one compound statement: the
// range loop's body holds the only if statement, the only comparison, the
// only removable statement, and the only literal in the file. The outer
// block's own statements are a zero-value variable declaration (deliberately
// `var total int`, not `total := 0` -- the latter has a literal `0` that
// literal/number would match) and a return, neither of which any operator
// touches, so a directive on the loop must leave the walk with nothing at all
// to do.
//
// Duplicated in engine_test.go's identical fixture for
// TestRunSuppressesCompoundStatement, a blackbox test that only calls the
// exported Run and so cannot share this whitebox copy.
const cascadeSrc = `package guard

// Total sums the positive values in vs.
func Total(vs []int) int {
	var total int
	%sfor _, v := range vs {
		if v > 0 {
			total += v
		}
	}
	return total
}
`

// suppressedLine is the line cascadeSrc's range statement sits on once the
// directive has been substituted in.
const suppressedLine = 7

// cascadeModule writes a one-package module holding cascadeSrc, with the
// directive line substituted in, and returns the module root and the source
// file's path.
//
// writeFiles is defined in runner_internal_test.go; both files compile as
// part of the same whitebox package mutate, so it needs no local copy here.
func cascadeModule(t *testing.T, directiveLine string) (root, file string) {
	t.Helper()

	root = t.TempDir()
	file = filepath.Join(root, "guard", "guard.go")

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/cascade\n\ngo 1.23\n",
		file:                          fmtSrc(cascadeSrc, directiveLine),
		filepath.Join(root, "guard", "guard_test.go"): `package guard

import "testing"

func TestTotal(t *testing.T) {
	if got := Total([]int{1, -2, 3}); got != 4 {
		t.Fatalf("Total() = %d", got)
	}
}
`,
	})

	return root, file
}

// fmtSrc substitutes the directive line into cascadeSrc, keeping the template's
// tab indentation intact.
func fmtSrc(template, directiveLine string) string {
	if directiveLine != "" {
		directiveLine += "\n\t"
	}

	return strings.Replace(template, "%s", directiveLine, 1)
}

// TestMutateFileCascade proves the cascade through the engine's real walk,
// without a toolchain: the runner's go binary does not exist, so reaching a
// single mutant is a hard error. A directive on the range statement must
// therefore produce no error, no mutants and exactly one suppression — and the
// same file without the directive must fail, which is what makes the first half
// meaningful rather than vacuous.
func TestMutateFileCascade(t *testing.T) {
	t.Parallel()

	run := &runner{goBin: "/nonexistent/go", testTimeout: time.Second}

	t.Run("suppressed compound statement is never descended into", func(t *testing.T) {
		t.Parallel()

		root, path := cascadeModule(t, "// nomutant: hand-verified")

		sink := newCollector()

		if err := mutateFile(t.Context(), run, &fileJob{moduleDir: root, path: path, mutators: mutator.All()}, sink, nil); err != nil {
			t.Fatalf("mutateFile() error = %v, want the walk to stop at the suppressed loop", err)
		}

		result := sink.close()

		if len(result.Mutants) != 0 {
			t.Errorf("mutateFile() produced %d mutants inside a suppressed statement", len(result.Mutants))
		}

		want := []SuppressionResult{{File: path, Line: suppressedLine, Reason: "hand-verified"}}
		if !reflect.DeepEqual(result.Suppressions, want) {
			t.Errorf("Suppressions = %+v, want %+v", result.Suppressions, want)
		}

		if result.SuppressedCount() != 1 {
			t.Errorf("SuppressedCount() = %d, want 1", result.SuppressedCount())
		}
	})

	t.Run("without the directive the same nodes are mutated", func(t *testing.T) {
		t.Parallel()

		root, path := cascadeModule(t, "")

		sink := newCollector()

		if err := mutateFile(t.Context(), run, &fileJob{moduleDir: root, path: path, mutators: mutator.All()}, sink, nil); err == nil {
			t.Fatal("mutateFile() error = nil: the fixture produced no mutants even unsuppressed")
		}

		result := sink.close()

		if len(result.Suppressions) != 0 {
			t.Errorf("Suppressions = %+v, want none for a file with no directives", result.Suppressions)
		}
	})
}
