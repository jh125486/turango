// Whitebox: runner.go exports no identifiers at all — mutant, runner,
// classify, parseTestEvents, copyModule and the rest are all unexported —
// so every test here needs direct package access; there is no exported
// surface left for a blackbox file to cover.
package mutate

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/jh125486/turango/internal/mutator"
)

// The samples below are verbatim `go test -json` output captured from a real
// toolchain against a throwaway module, trimmed only of lines the classifier
// does not read. They are the contract this classifier is written against, so
// they are pasted rather than generated.

//nolint:gosec // false positive: "pass" here means a captured `go test -json` PASS action, not a credential
const passStream = `{"Time":"2026-08-05T16:03:00.089842-07:00","Action":"start","Package":"example.com/fixture/mathx"}
{"Time":"2026-08-05T16:03:00.566645-07:00","Action":"run","Package":"example.com/fixture/mathx","Test":"TestClamp"}
{"Time":"2026-08-05T16:03:00.569463-07:00","Action":"output","Package":"example.com/fixture/mathx","Test":"TestClamp","Output":"=== RUN   TestClamp\n"}
{"Time":"2026-08-05T16:03:00.569498-07:00","Action":"output","Package":"example.com/fixture/mathx","Test":"TestClamp","Output":"--- PASS: TestClamp (0.00s)\n"}
{"Time":"2026-08-05T16:03:00.569505-07:00","Action":"pass","Package":"example.com/fixture/mathx","Test":"TestClamp","Elapsed":0}
{"Time":"2026-08-05T16:03:00.569554-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"PASS\n"}
{"Time":"2026-08-05T16:03:00.569617-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"ok  \texample.com/fixture/mathx\t0.480s\n"}
{"Time":"2026-08-05T16:03:00.569815-07:00","Action":"pass","Package":"example.com/fixture/mathx","Elapsed":0.48}
`

const failStream = `{"Time":"2026-08-05T16:03:12.500158-07:00","Action":"start","Package":"example.com/fixture/mathx"}
{"Time":"2026-08-05T16:03:13.004879-07:00","Action":"run","Package":"example.com/fixture/mathx","Test":"TestClamp"}
{"Time":"2026-08-05T16:03:13.005033-07:00","Action":"run","Package":"example.com/fixture/mathx","Test":"TestSum"}
{"Time":"2026-08-05T16:03:13.005038-07:00","Action":"output","Package":"example.com/fixture/mathx","Test":"TestSum","Output":"    mathx_test.go:19: Sum = -6\n"}
{"Time":"2026-08-05T16:03:13.005053-07:00","Action":"output","Package":"example.com/fixture/mathx","Test":"TestSum","Output":"--- FAIL: TestSum (0.00s)\n"}
{"Time":"2026-08-05T16:03:13.005057-07:00","Action":"fail","Package":"example.com/fixture/mathx","Test":"TestSum","Elapsed":0}
{"Time":"2026-08-05T16:03:13.005078-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"FAIL\n"}
{"Time":"2026-08-05T16:03:13.005547-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"FAIL\texample.com/fixture/mathx\t0.505s\n"}
{"Time":"2026-08-05T16:03:13.005568-07:00","Action":"fail","Package":"example.com/fixture/mathx","Elapsed":0.505}
`

// buildFailStream is what a modern toolchain (Go 1.24+) emits: the compiler's
// diagnostics arrive as build-output/build-fail events on stdout, keyed by
// ImportPath rather than Package, and stderr stays empty.
const buildFailStream = `{"ImportPath":"example.com/fixture/mathx [example.com/fixture/mathx.test]","Action":"build-output","Output":"# example.com/fixture/mathx [example.com/fixture/mathx.test]\n"}
{"ImportPath":"example.com/fixture/mathx [example.com/fixture/mathx.test]","Action":"build-output","Output":"mathx/mathx.go:18:12: undefined: undefinedVar\n"}
{"ImportPath":"example.com/fixture/mathx [example.com/fixture/mathx.test]","Action":"build-fail"}
{"Time":"2026-08-05T16:03:13.147299-07:00","Action":"start","Package":"example.com/fixture/mathx"}
{"Time":"2026-08-05T16:03:13.147325-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"FAIL\texample.com/fixture/mathx [build failed]\n"}
{"Time":"2026-08-05T16:03:13.147329-07:00","Action":"fail","Package":"example.com/fixture/mathx","Elapsed":0,"FailedBuild":"example.com/fixture/mathx [example.com/fixture/mathx.test]"}
`

// legacyBuildFailStdout / legacyBuildFailStderr are the pre-1.24 shape: the
// compiler writes to stderr, outside the event stream entirely, and stdout only
// carries a package-level failure with no test ever having run.
const legacyBuildFailStdout = `{"Time":"2026-08-05T16:03:13.147299-07:00","Action":"start","Package":"example.com/fixture/mathx"}
{"Time":"2026-08-05T16:03:13.147325-07:00","Action":"output","Package":"example.com/fixture/mathx","Output":"FAIL\texample.com/fixture/mathx [build failed]\n"}
{"Time":"2026-08-05T16:03:13.147329-07:00","Action":"fail","Package":"example.com/fixture/mathx","Elapsed":0}
`

const legacyBuildFailStderr = `# example.com/fixture/mathx [example.com/fixture/mathx.test]
mathx/mathx.go:18:12: undefined: undefinedVar
`

// noTestFilesStream is a package with no tests at all: the package is skipped,
// nothing fails, and no test-level event ever appears.
const noTestFilesStream = `{"Time":"2026-08-05T16:03:13.147299-07:00","Action":"start","Package":"example.com/fixture/app"}
{"Time":"2026-08-05T16:03:13.147325-07:00","Action":"output","Package":"example.com/fixture/app","Output":"?   \texample.com/fixture/app\t[no test files]\n"}
{"Time":"2026-08-05T16:03:13.147329-07:00","Action":"skip","Package":"example.com/fixture/app","Elapsed":0}
`

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stdout     string
		stderr     string
		want       Status
		wantOutput string // substring the reconstructed output must contain
	}{
		"all tests pass": {
			stdout:     passStream,
			want:       Survived,
			wantOutput: "ok  \texample.com/fixture/mathx",
		},
		"a test fails": {
			stdout:     failStream,
			want:       Killed,
			wantOutput: "--- FAIL: TestSum",
		},
		"build failure, modern events": {
			stdout:     buildFailStream,
			want:       NotViable,
			wantOutput: "undefined: undefinedVar",
		},
		"build failure, diagnostics on stderr": {
			stdout:     legacyBuildFailStdout,
			stderr:     legacyBuildFailStderr,
			want:       NotViable,
			wantOutput: "undefined: undefinedVar",
		},
		"package has no test files": {
			stdout:     noTestFilesStream,
			want:       Survived,
			wantOutput: "[no test files]",
		},
		"nothing at all": {
			want: Survived,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, out := classify([]byte(tt.stdout), []byte(tt.stderr))
			if got != tt.want {
				t.Errorf("classify() = %v, want %v\noutput:\n%s", got, tt.want, out)
			}

			if tt.wantOutput != "" && !strings.Contains(out, tt.wantOutput) {
				t.Errorf("classify() output = %q, want it to contain %q", out, tt.wantOutput)
			}
		})
	}
}

// TestClassifyIgnoresTestOutputThatLooksLikeACompileError guards the one way
// the compile-error heuristic could misfire: a test that prints something
// shaped like a compiler diagnostic must still be classified by its events.
func TestClassifyIgnoresTestOutputThatLooksLikeACompileError(t *testing.T) {
	t.Parallel()

	stream := `{"Action":"run","Package":"p","Test":"TestX"}
{"Action":"output","Package":"p","Test":"TestX","Output":"    x_test.go:9:3: undefined: whatever\n"}
{"Action":"output","Package":"p","Test":"TestX","Output":"--- FAIL: TestX (0.00s)\n"}
{"Action":"fail","Package":"p","Test":"TestX","Elapsed":0}
{"Action":"fail","Package":"p","Elapsed":0.1}
`

	if got, _ := classify([]byte(stream), nil); got != Killed {
		t.Errorf("classify() = %v, want %v", got, Killed)
	}
}

func TestTruncateKeepsTail(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxOutputBytes) + "TAIL"

	got := truncate(long)
	if !strings.HasSuffix(got, "TAIL") {
		t.Error("truncate() dropped the tail")
	}

	if !strings.HasPrefix(got, "...(truncated)...") {
		t.Error("truncate() did not mark the output as truncated")
	}

	if short := truncate("short"); short != "short" {
		t.Errorf("truncate(%q) = %q, want it unchanged", "short", short)
	}
}

// TestRunSkipsNoOpMutation covers the skip path: a mutation whose printed
// source is identical to the original is not a mutant, so run must report
// neither a result nor an error — and must not touch the filesystem or shell
// out, which is why this runner has a deliberately unusable go binary path.
func TestRunSkipsNoOpMutation(t *testing.T) {
	t.Parallel()

	const src = `package p

func f(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var baseline bytes.Buffer
	if err := printer.Fprint(&baseline, fset, file); err != nil {
		t.Fatalf("Fprint() error = %v", err)
	}

	var applied, reverted bool

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	got, ok, equivalent, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "/nowhere/p.go",
		baseline:  baseline.Bytes(),
		moduleDir: "/nowhere",
		pkgDir:    "/nowhere",
		operator:  "test/noop",
		line:      4,
		mutation: mutator.Mutation{
			Description: "no-op",
			Apply:       func() { applied = true },
			Revert:      func() { reverted = true },
		},
	})
	if err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	if ok || equivalent {
		t.Errorf("run() = %+v, ok = %v, equivalent = %v, want both false for a no-op mutation", got, ok, equivalent)
	}

	if !applied || !reverted {
		t.Errorf("run() applied = %v, reverted = %v; want both true", applied, reverted)
	}
}

// TestMutantTestArgs pins the flag surface each scope produces, which is what
// decides how much of the module gets to catch a mutant.
func TestMutantTestArgs(t *testing.T) {
	t.Parallel()

	base := mutant{
		moduleDir: filepath.FromSlash("/m"),
		pkgDir:    filepath.FromSlash("/m/internal/mathx"),
	}

	tests := map[string]struct {
		scope    Scope
		covering []string
		want     []string
	}{
		"full runs the whole module": {
			scope: ScopeFull,
			want:  []string{"./..."},
		},
		"package runs only the mutated package": {
			scope: ScopePackage,
			want:  []string{"./internal/mathx"},
		},
		"impact runs only the covering tests": {
			scope:    ScopeImpact,
			covering: []string{"TestClamp", "TestDescribe"},
			want: []string{
				"-run=^(TestClamp|TestDescribe)$",
				"-coverpkg=./internal/mathx",
				"./internal/mathx",
			},
		},
		"impact with a single covering test": {
			scope:    ScopeImpact,
			covering: []string{"TestClamp"},
			want: []string{
				"-run=^(TestClamp)$",
				"-coverpkg=./internal/mathx",
				"./internal/mathx",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := base
			m.scope = tt.scope
			m.covering = tt.covering

			got, err := m.testArgs()
			if err != nil {
				t.Fatalf("testArgs() error = %v", err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("testArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunSkipsUncoveredMutant covers impact scope's payoff: a mutant on a line
// no test executes is decided by the coverage map alone. The unusable go binary
// is the assertion — reaching the toolchain at all would fail the test.
func TestRunSkipsUncoveredMutant(t *testing.T) {
	t.Parallel()

	const src = `package p

func f(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var baseline bytes.Buffer
	if err := printer.Fprint(&baseline, fset, file); err != nil {
		t.Fatalf("Fprint() error = %v", err)
	}

	// A mutation that genuinely changes the printed source, so the no-op
	// short-circuit cannot be what makes this pass.
	decl := file.Decls[0].(*ast.FuncDecl)
	stmt := decl.Body.List[0].(*ast.IfStmt)
	original := stmt.Cond

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	got, ok, equivalent, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "/nowhere/p.go",
		baseline:  baseline.Bytes(),
		moduleDir: "/nowhere",
		pkgDir:    "/nowhere",
		operator:  "control/if",
		line:      4,
		scope:     ScopeImpact,
		covering:  nil,
		node:      stmt,
		mutation: mutator.Mutation{
			Description: "if -> false",
			Apply:       func() { stmt.Cond = ast.NewIdent("false") },
			Revert:      func() { stmt.Cond = original },
		},
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if equivalent {
		t.Error("run() equivalent = true, want false: TCE is not active in this test")
	}

	if !ok {
		t.Fatal("run() ok = false, want a survived verdict for an uncovered line")
	}

	if got.Status != Survived {
		t.Errorf("run() status = %v, want %v", got.Status, Survived)
	}

	if !strings.Contains(got.Output, "no test") {
		t.Errorf("run() output = %q, want it to explain that no test covers the line", got.Output)
	}

	// Before/After: mutation.Node was left nil, so the fallback is m.node
	// (stmt, the whole if statement) — printed before Apply and again
	// after, a concrete assertion that the render captures the actual
	// condition swap, not just that the fields are non-empty.
	if !strings.Contains(got.Before, "a > b") {
		t.Errorf("run() Before = %q, want it to contain the original condition", got.Before)
	}

	if !strings.Contains(got.After, "false") || strings.Contains(got.After, "a > b") {
		t.Errorf("run() After = %q, want the swapped condition, not the original", got.After)
	}

	if stmt.Cond != original {
		t.Error("run() left the mutation applied to the shared AST")
	}
}

// TestRunReportsEmptyAfterForRemoval covers MutantResult.After's documented
// special case: when mutation.Node's own printed text is identical before
// and after Apply — because Apply repointed the containing list's slot
// rather than editing Node's fields — After must be empty, not a stale
// duplicate of Before. statement/remover is the concrete case this protects
// (see its own doc comment on the Mutation literal it returns), reproduced
// directly here rather than through the real operator so the test pins the
// runner's contract independent of that operator's own behavior.
func TestRunReportsEmptyAfterForRemoval(t *testing.T) {
	t.Parallel()

	const src = `package p

func f() int {
	x := 1
	return x
}
`

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var baseline bytes.Buffer
	if err := printer.Fprint(&baseline, fset, file); err != nil {
		t.Fatalf("Fprint() error = %v", err)
	}

	decl := file.Decls[0].(*ast.FuncDecl)
	list := &decl.Body.List
	stmt := (*list)[0]
	blank := &ast.EmptyStmt{Semicolon: stmt.Pos(), Implicit: true}

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	got, ok, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "/nowhere/p.go",
		baseline:  baseline.Bytes(),
		moduleDir: "/nowhere",
		pkgDir:    "/nowhere",
		operator:  "statement/remover",
		line:      4,
		scope:     ScopeImpact,
		covering:  nil,
		node:      decl.Body,
		mutation: mutator.Mutation{
			Description: "remove statement: x := 1",
			Apply:       func() { (*list)[0] = blank },
			Revert:      func() { (*list)[0] = stmt },
			Node:        stmt,
		},
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if !ok {
		t.Fatal("run() ok = false, want a survived verdict for an uncovered line")
	}

	if !strings.Contains(got.Before, "x := 1") {
		t.Errorf("run() Before = %q, want the removed statement's text", got.Before)
	}

	if got.After != "" {
		t.Errorf("run() After = %q, want empty: the node's own text does not change when it is removed, not edited", got.After)
	}
}

func TestPackagePattern(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		moduleDir, pkgDir, want string
	}{
		"root package":   {moduleDir: "/m", pkgDir: "/m", want: "."},
		"nested package": {moduleDir: "/m", pkgDir: "/m/internal/foo", want: "./internal/foo"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := packagePattern(filepath.FromSlash(tt.moduleDir), filepath.FromSlash(tt.pkgDir))
			if err != nil {
				t.Fatalf("packagePattern() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("packagePattern() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommonDir(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		paths []string
		want  string
	}{
		"single path":     {paths: []string{"/a/b/c"}, want: "/a/b/c"},
		"siblings":        {paths: []string{"/a/b/c", "/a/b/d"}, want: "/a/b"},
		"nested":          {paths: []string{"/a/b", "/a/b/c/d"}, want: "/a/b"},
		"only root":       {paths: []string{"/a", "/b"}, want: "/"},
		"nothing at all":  {paths: nil, want: ""},
		"three way split": {paths: []string{"/a/b/c", "/a/b/d/e", "/a/x"}, want: "/a"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := make([]string, 0, len(tt.paths))
			for _, p := range tt.paths {
				paths = append(paths, filepath.FromSlash(p))
			}

			if got := commonDir(paths); got != filepath.FromSlash(tt.want) {
				t.Errorf("commonDir(%v) = %q, want %q", tt.paths, got, tt.want)
			}
		})
	}
}

// TestCopyModuleWithLocalReplace exercises the workspace copy against the shape
// that actually breaks naive copiers: a module that replaces a dependency with
// a sibling checkout, embeds a non-Go asset, and vendors nothing in particular.
func TestCopyModuleWithLocalReplace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	main := filepath.Join(root, "main")
	lib := filepath.Join(root, "lib")

	writeFiles(t, map[string]string{
		filepath.Join(main, "go.mod"): `module example.com/main

go 1.23

require example.com/lib v0.0.0

replace example.com/lib => ../lib
`,
		filepath.Join(main, "assets", "banner.txt"):  "hello\n",
		filepath.Join(main, "vendor", "modules.txt"): "# example.com/lib v0.0.0\n",
		filepath.Join(main, ".git", "config"):        "[core]\n",
		filepath.Join(lib, "go.mod"):                 "module example.com/lib\n\ngo 1.23\n",
		filepath.Join(lib, "lib.go"):                 "package lib\n",
	})

	dst := t.TempDir()

	copied, err := copyModule(dst, main)
	if err != nil {
		t.Fatalf("copyModule() error = %v", err)
	}

	// Non-Go assets and vendor/ must survive at their original relative paths,
	// or //go:embed and -mod=vendor builds break in the copy.
	for _, rel := range []string{"go.mod", "assets/banner.txt", "vendor/modules.txt"} {
		if _, err := os.Stat(filepath.Join(copied, filepath.FromSlash(rel))); err != nil {
			t.Errorf("copied module is missing %s: %v", rel, err)
		}
	}

	if _, err := os.Stat(filepath.Join(copied, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was copied into the workspace; err = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(copied, "go.mod"))
	if err != nil {
		t.Fatalf("reading copied go.mod: %v", err)
	}

	target := replaceTarget(t, string(data))
	if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
		t.Fatalf("rewritten replace target %q is not a filesystem path", target)
	}

	resolved := filepath.Join(copied, filepath.FromSlash(target))
	//nolint:gosec // resolved is derived from this test's own fixture go.mod, not external input
	if _, err := os.Stat(filepath.Join(resolved, "go.mod")); err != nil {
		t.Errorf("replace target %q does not resolve to a copied module: %v", target, err)
	}
}

// replaceTarget pulls the right-hand side out of the single replace line in a
// go.mod.
func replaceTarget(t *testing.T, gomod string) string {
	t.Helper()

	for line := range strings.SplitSeq(gomod, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "replace" && fields[2] == "=>" {
			return fields[3]
		}
	}

	t.Fatalf("no replace directive found in:\n%s", gomod)

	return ""
}

// writeFiles creates every named file, and the directories holding them, with
// the given contents.
func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()

	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
}

// The tests below cover ROADMAP.md gap 5's not-yet-wired components
// (closureDirs, hasEmbedDirective, resolveClosure, copyClosure,
// copyDirFiles) in isolation. Nothing in the engine calls these yet — see
// closureDirs' own doc comment for why activating them is a deliberately
// separate step — but they're real, reachable code, tested the same as
// anything else in this file.

// TestHasEmbedDirective covers the fail-toward-safety rule: a real
// directive is a match, an unrelated mention of the string is (honestly, a
// false positive, but the safe direction) also a match, and an unreadable
// file is treated as a match too, since a false negative here is not
// survivable (a missing embedded asset fails the build) while a false
// positive only costs an unnecessary fallback to the full-module copy.
func TestHasEmbedDirective(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	withDirective := filepath.Join(dir, "embed.go")
	writeFiles(t, map[string]string{
		withDirective: "package p\n\n//go:embed data.txt\nvar data string\n",
	})

	withoutDirective := filepath.Join(dir, "plain.go")
	writeFiles(t, map[string]string{
		withoutDirective: "package p\n\nfunc F() {}\n",
	})

	tests := map[string]struct {
		files []string
		want  bool
	}{
		"real directive":        {files: []string{withoutDirective, withDirective}, want: true},
		"no directive":          {files: []string{withoutDirective}, want: false},
		"unreadable, fail safe": {files: []string{filepath.Join(dir, "does-not-exist.go")}, want: true},
		"no files at all":       {files: nil, want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := hasEmbedDirective(tt.files); got != tt.want {
				t.Errorf("hasEmbedDirective(%v) = %v, want %v", tt.files, got, tt.want)
			}
		})
	}
}

// TestCopyDirFiles covers the shallow-copy contract: files directly in src
// land in dst, subdirectories (a different Go package, per closureDirs' own
// doc comment) are left alone.
func TestCopyDirFiles(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	writeFiles(t, map[string]string{
		filepath.Join(src, "a.go"):        "package p\n",
		filepath.Join(src, "a_test.go"):   "package p\n",
		filepath.Join(src, "sub", "b.go"): "package sub\n",
	})

	if err := copyDirFiles(src, dst); err != nil {
		t.Fatalf("copyDirFiles() error = %v", err)
	}

	for _, want := range []string{"a.go", "a_test.go"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("copyDirFiles() did not copy %s: %v", want, err)
		}
	}

	if _, err := os.Stat(filepath.Join(dst, "sub")); err == nil {
		t.Error("copyDirFiles() copied a subdirectory, want top-level files only")
	}
}

// TestClosureDirs covers the forward-closure walk: same-module packages
// (transitively) are included, a different module's packages are excluded
// entirely (their subtrees never walked), and an embed directive anywhere
// in the closure — even several imports deep — makes the whole result
// unsafe. Every *packages.Package below is hand-built: closureDirs only
// reads the fields it documents needing (Module, GoFiles, Imports, Errors),
// so there is no need to drive a real go/packages.Load or real toolchain to
// exercise it.
func TestClosureDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "mod")
	targetDir := filepath.Join(moduleDir, "target")
	depDir := filepath.Join(moduleDir, "dep")

	writeFiles(t, map[string]string{
		filepath.Join(targetDir, "target.go"): "package target\n",
		filepath.Join(depDir, "dep.go"):       "package dep\n",
	})

	mod := &packages.Module{Dir: moduleDir}

	dep := &packages.Package{
		PkgPath: "example.com/mod/dep",
		Module:  mod,
		GoFiles: []string{filepath.Join(depDir, "dep.go")},
	}

	stdlib := &packages.Package{
		PkgPath: "fmt",
		Module:  nil, // stdlib packages report no module
		GoFiles: []string{"/goroot/src/fmt/print.go"},
	}

	target := &packages.Package{
		PkgPath: "example.com/mod/target",
		Module:  mod,
		GoFiles: []string{filepath.Join(targetDir, "target.go")},
		Imports: map[string]*packages.Package{
			"example.com/mod/dep": dep,
			"fmt":                 stdlib,
		},
	}

	dirs, ok := closureDirs(target)
	if !ok {
		t.Fatal("closureDirs() ok = false, want true for a clean same-module closure")
	}

	want := map[string]bool{targetDir: true, depDir: true}
	if !reflect.DeepEqual(dirs, want) {
		t.Errorf("closureDirs() = %v, want %v", dirs, want)
	}

	t.Run("embed anywhere in the closure is unsafe", func(t *testing.T) {
		t.Parallel()

		embedDepDir := filepath.Join(dir, "embed-mod", "dep")
		writeFiles(t, map[string]string{
			filepath.Join(embedDepDir, "dep.go"): "package dep\n\n//go:embed data.txt\nvar data string\n",
		})

		embedMod := &packages.Module{Dir: filepath.Join(dir, "embed-mod")}

		embedDep := &packages.Package{
			PkgPath: "example.com/mod/dep",
			Module:  embedMod,
			GoFiles: []string{filepath.Join(embedDepDir, "dep.go")},
		}

		embedTarget := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  embedMod,
			GoFiles: []string{filepath.Join(embedMod.Dir, "target", "target.go")},
			Imports: map[string]*packages.Package{"example.com/mod/dep": embedDep},
		}

		writeFiles(t, map[string]string{
			filepath.Join(embedMod.Dir, "target", "target.go"): "package target\n",
		})

		if _, ok := closureDirs(embedTarget); ok {
			t.Error("closureDirs() ok = true, want false: an import several layers deep has a //go:embed directive")
		}
	})

	t.Run("a package with load errors is unsafe", func(t *testing.T) {
		t.Parallel()

		broken := &packages.Package{
			PkgPath: "example.com/mod/broken",
			Module:  mod,
			Errors:  []packages.Error{{Msg: "syntax error"}},
		}

		withBroken := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  mod,
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
			Imports: map[string]*packages.Package{"example.com/mod/broken": broken},
		}

		if _, ok := closureDirs(withBroken); ok {
			t.Error("closureDirs() ok = true, want false: an imported package failed to load")
		}
	})

	t.Run("no module info is unsafe", func(t *testing.T) {
		t.Parallel()

		if _, ok := closureDirs(&packages.Package{PkgPath: "example.com/mod/target"}); ok {
			t.Error("closureDirs() ok = true, want false: pkg has no Module at all")
		}
	})
}

// TestResolveClosure covers the two fallback triggers closureDirs alone
// doesn't know about: a vendor/ directory, and any local replace directive.
// Either one means "give up, let the caller fall back to copyModule" — see
// resolveClosure's own doc comment for why re-scoping either to a partial
// closure is out of scope for v1.
func TestResolveClosure(t *testing.T) {
	t.Parallel()

	newTarget := func(t *testing.T, moduleDir string) *packages.Package {
		t.Helper()

		targetDir := filepath.Join(moduleDir, "target")
		writeFiles(t, map[string]string{filepath.Join(targetDir, "target.go"): "package target\n"})

		return &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  &packages.Module{Dir: moduleDir},
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
		}
	}

	t.Run("clean module resolves", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n"})

		dirs, ok := resolveClosure(newTarget(t, moduleDir))
		if !ok {
			t.Fatal("resolveClosure() ok = false, want true for a module with no vendor dir or replace directives")
		}

		if want := map[string]bool{filepath.Join(moduleDir, "target"): true}; !reflect.DeepEqual(dirs, want) {
			t.Errorf("resolveClosure() dirs = %v, want %v", dirs, want)
		}
	})

	t.Run("a vendor directory falls back", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"):                "module example.com/mod\n\ngo 1.23\n",
			filepath.Join(moduleDir, "vendor", "modules.txt"): "",
		})

		if _, ok := resolveClosure(newTarget(t, moduleDir)); ok {
			t.Error("resolveClosure() ok = true, want false: a vendor/ directory is present")
		}
	})

	t.Run("a local replace directive falls back", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		sibling := t.TempDir()
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\nreplace example.com/sibling => " +
				filepath.ToSlash(sibling) + "\n",
		})

		if _, ok := resolveClosure(newTarget(t, moduleDir)); ok {
			t.Error("resolveClosure() ok = true, want false: go.mod has a local replace directive")
		}
	})
}

// TestCopyClosure covers the actual copy: go.mod/go.sum land at the new
// module root, every directory in dirs lands at its module-relative offset
// preserved, and nothing outside dirs is copied.
func TestCopyClosure(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	targetDir := filepath.Join(moduleDir, "target")
	depDir := filepath.Join(moduleDir, "internal", "dep")
	irrelevantDir := filepath.Join(moduleDir, "irrelevant")

	writeFiles(t, map[string]string{
		filepath.Join(moduleDir, "go.mod"):        "module example.com/mod\n\ngo 1.23\n",
		filepath.Join(moduleDir, "go.sum"):        "",
		filepath.Join(targetDir, "target.go"):     "package target\n",
		filepath.Join(depDir, "dep.go"):           "package dep\n",
		filepath.Join(irrelevantDir, "unused.go"): "package irrelevant\n",
	})

	dst := t.TempDir()

	newModuleDir, err := copyClosure(dst, moduleDir, map[string]bool{targetDir: true, depDir: true})
	if err != nil {
		t.Fatalf("copyClosure() error = %v", err)
	}

	for _, want := range []string{
		filepath.Join(newModuleDir, "go.mod"),
		filepath.Join(newModuleDir, "go.sum"),
		filepath.Join(newModuleDir, "target", "target.go"),
		filepath.Join(newModuleDir, "internal", "dep", "dep.go"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("copyClosure() did not produce %s: %v", want, err)
		}
	}

	if _, err := os.Stat(filepath.Join(newModuleDir, "irrelevant")); err == nil {
		t.Error("copyClosure() copied a directory outside the closure")
	}
}
