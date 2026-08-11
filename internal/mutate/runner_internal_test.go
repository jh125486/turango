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
	"os/exec"
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
// doc comment) are left alone, and a symlink is preserved as a symlink
// rather than having its target content copied in as a plain file — the
// same contract copyTree already gives a full module copy.
func TestCopyDirFiles(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	writeFiles(t, map[string]string{
		filepath.Join(src, "a.go"):        "package p\n",
		filepath.Join(src, "a_test.go"):   "package p\n",
		filepath.Join(src, "sub", "b.go"): "package sub\n",
	})

	link := filepath.Join(src, "link.go")
	if err := os.Symlink(filepath.Join(src, "a.go"), link); err != nil {
		t.Fatalf("creating symlink fixture: %v", err)
	}

	if err := copyDirFiles(src, dst); err != nil {
		t.Fatalf("copyDirFiles() error = %v", err)
	}

	for _, want := range []string{"a.go", "a_test.go", "link.go"} {
		if _, err := os.Lstat(filepath.Join(dst, want)); err != nil {
			t.Errorf("copyDirFiles() did not copy %s: %v", want, err)
		}
	}

	if _, err := os.Stat(filepath.Join(dst, "sub")); err == nil {
		t.Error("copyDirFiles() copied a subdirectory, want top-level files only")
	}

	got, err := os.Readlink(filepath.Join(dst, "link.go"))
	if err != nil {
		t.Fatalf("copyDirFiles() did not preserve link.go as a symlink: %v", err)
	}

	if want := filepath.Join(src, "a.go"); got != want {
		t.Errorf("copyDirFiles() symlink target = %q, want %q", got, want)
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

// TestResolveClosure covers the fallback triggers closureDirs alone doesn't
// know about: a vendor/ directory, any local replace directive whose target
// is outside the module, and — see the "in-module replace" subtests below —
// one whose target is inside the module but outside the computed closure.
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

	// This is the regression test for the review's load-bearing finding: a
	// black-box "target_test" package (this repo's own default test shape —
	// see engine_test.go, report_test.go) is a *separate* *packages.Package
	// from the production "target" one, with its own Imports map. Before
	// resolveClosure accepted every variant and merged them (mergeVariants),
	// a same-module import that only appeared in that package was silently
	// absent from the closure — invisible to every existing fixture because
	// they all hand-built a single pkg.Imports that already had everything.
	t.Run("a black-box test package's own import is included", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		targetDir := filepath.Join(moduleDir, "target")
		helperDir := filepath.Join(moduleDir, "helper")

		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"):         "module example.com/mod\n\ngo 1.23\n",
			filepath.Join(targetDir, "target.go"):      "package target\n",
			filepath.Join(targetDir, "target_test.go"): "package target_test\n",
			filepath.Join(helperDir, "helper.go"):      "package helper\n",
		})

		mod := &packages.Module{Dir: moduleDir}

		helper := &packages.Package{
			PkgPath: "example.com/mod/helper",
			Module:  mod,
			GoFiles: []string{filepath.Join(helperDir, "helper.go")},
		}

		prod := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  mod,
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
		}

		blackbox := &packages.Package{
			PkgPath: "example.com/mod/target_test",
			Module:  mod,
			GoFiles: []string{filepath.Join(targetDir, "target_test.go")},
			Imports: map[string]*packages.Package{"example.com/mod/helper": helper},
		}

		dirs, ok := resolveClosure(prod, blackbox)
		if !ok {
			t.Fatal("resolveClosure() ok = false, want true")
		}

		want := map[string]bool{targetDir: true, helperDir: true}
		if !reflect.DeepEqual(dirs, want) {
			t.Errorf("resolveClosure() dirs = %v, want %v — the black-box test package's own import must not be dropped", dirs, want)
		}
	})

	// This covers the other go/packages test variant shape resolveClosure's
	// own doc comment calls out: a *whitebox* _test.go file — same package
	// clause as the production code (this repo's own convention is
	// black-box by default, but a handful of files, e.g. runner_internal_test.go
	// itself, are whitebox for a documented reason) — is not a second
	// *packages.Package with a plain PkgPath; go/packages instead produces
	// a distinct, bracketed "pkg [pkg.test]" variant that recompiles the
	// production files together with the internal test file(s). Its own
	// Imports map can hold an import used only by the internal test file,
	// exactly as the black-box case above, and mergeVariants must not drop
	// it either.
	t.Run("an internal test file's own import is included", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		targetDir := filepath.Join(moduleDir, "target")
		helperDir := filepath.Join(moduleDir, "helper")

		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"):         "module example.com/mod\n\ngo 1.23\n",
			filepath.Join(targetDir, "target.go"):      "package target\n",
			filepath.Join(targetDir, "target_test.go"): "package target\n",
			filepath.Join(helperDir, "helper.go"):      "package helper\n",
		})

		mod := &packages.Module{Dir: moduleDir}

		helper := &packages.Package{
			PkgPath: "example.com/mod/helper",
			Module:  mod,
			GoFiles: []string{filepath.Join(helperDir, "helper.go")},
		}

		prod := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  mod,
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
		}

		internalTest := &packages.Package{
			PkgPath: "example.com/mod/target [example.com/mod/target.test]",
			Module:  mod,
			GoFiles: []string{
				filepath.Join(targetDir, "target.go"),
				filepath.Join(targetDir, "target_test.go"),
			},
			Imports: map[string]*packages.Package{"example.com/mod/helper": helper},
		}

		dirs, ok := resolveClosure(prod, internalTest)
		if !ok {
			t.Fatal("resolveClosure() ok = false, want true")
		}

		want := map[string]bool{targetDir: true, helperDir: true}
		if !reflect.DeepEqual(dirs, want) {
			t.Errorf("resolveClosure() dirs = %v, want %v — the internal test variant's own import must not be dropped", dirs, want)
		}
	})

	// This is the regression test for the review's second finding: go.mod is
	// copied unmodified into a closure-only workspace, so an in-module
	// replace directive is only safe to leave untouched when its target is
	// itself part of the closure just computed.
	t.Run("an in-module replace target outside the closure falls back", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\n" +
				"replace example.com/mod/unrelated => ./unrelated\n",
		})

		if _, ok := resolveClosure(newTarget(t, moduleDir)); ok {
			t.Error("resolveClosure() ok = true, want false: the in-module replace target isn't in the closure")
		}
	})

	t.Run("an in-module replace target inside the closure resolves", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		targetDir := filepath.Join(moduleDir, "target")
		depDir := filepath.Join(moduleDir, "dep")

		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\n" +
				"replace example.com/mod/dep => ./dep\n",
			filepath.Join(targetDir, "target.go"): "package target\n",
			filepath.Join(depDir, "dep.go"):       "package dep\n",
		})

		mod := &packages.Module{Dir: moduleDir}

		dep := &packages.Package{
			PkgPath: "example.com/mod/dep",
			Module:  mod,
			GoFiles: []string{filepath.Join(depDir, "dep.go")},
		}

		target := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  mod,
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
			Imports: map[string]*packages.Package{"example.com/mod/dep": dep},
		}

		dirs, ok := resolveClosure(target)
		if !ok {
			t.Fatal("resolveClosure() ok = false, want true: the replace target is already inside the computed closure")
		}

		want := map[string]bool{targetDir: true, depDir: true}
		if !reflect.DeepEqual(dirs, want) {
			t.Errorf("resolveClosure() dirs = %v, want %v", dirs, want)
		}
	})
}

// TestMergeVariants covers the union mergeVariants builds across every
// go/packages variant of a target directory: GoFiles/CompiledGoFiles
// pooled, Imports maps unioned, Errors concatenated, and Module taken from
// the first variant that has one — the shape resolveClosure depends on to
// give closureDirs a single root that actually represents everything
// `go test` needs, not just the production package.
func TestMergeVariants(t *testing.T) {
	t.Parallel()

	mod := &packages.Module{Dir: "/mod"}

	prod := &packages.Package{
		PkgPath: "example.com/mod/target",
		Module:  mod,
		GoFiles: []string{"/mod/target/target.go"},
		Imports: map[string]*packages.Package{"example.com/mod/a": {PkgPath: "example.com/mod/a"}},
	}

	blackbox := &packages.Package{
		PkgPath: "example.com/mod/target_test",
		GoFiles: []string{"/mod/target/target_test.go"},
		Errors:  []packages.Error{{Msg: "boom"}},
		Imports: map[string]*packages.Package{"example.com/mod/b": {PkgPath: "example.com/mod/b"}},
	}

	merged := mergeVariants([]*packages.Package{nil, prod, blackbox})

	if merged.Module != mod {
		t.Errorf("mergeVariants() Module = %v, want %v", merged.Module, mod)
	}

	wantFiles := []string{"/mod/target/target.go", "/mod/target/target_test.go"}
	if !reflect.DeepEqual(merged.GoFiles, wantFiles) {
		t.Errorf("mergeVariants() GoFiles = %v, want %v", merged.GoFiles, wantFiles)
	}

	if len(merged.Errors) != 1 {
		t.Errorf("mergeVariants() Errors = %v, want 1 entry", merged.Errors)
	}

	for _, path := range []string{"example.com/mod/a", "example.com/mod/b"} {
		if _, ok := merged.Imports[path]; !ok {
			t.Errorf("mergeVariants() Imports missing %s", path)
		}
	}
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

// The tests below cover ROADMAP.md gap 6: gitRepoRoot, gitWorktreeClean,
// copyWorktree and workspaceFor. Unlike copyModule's tests, these need a
// real git repository, not just a plain directory — runGit builds one.

// runGit runs a git subcommand in dir, failing the test on error. It is used
// only to *build* fixtures (init, config, add, commit); the functions under
// test invoke git themselves and are not routed through this helper.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	//nolint:gosec // dir and args are this test's own fixture-building calls, not external input.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitFixtureModule builds a small, real git repository containing one Go
// module — go.mod plus one trivial package — and commits it, returning the
// repository root (which is also the module root here: a single-module
// repo, the common case this fixture is meant to stand in for).
func gitFixtureModule(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	runGit(t, root, "init", "--quiet")
	runGit(t, root, "config", "user.email", "turango-test@example.com")
	runGit(t, root, "config", "user.name", "turango test")

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/gitfixture\n\ngo 1.23\n",
		filepath.Join(root, "lib.go"): "package gitfixture\n\n// Marker is read back by TestCopyWorktree to confirm the checkout's content.\nconst Marker = \"committed\"\n",
	})

	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "--quiet", "-m", "initial")

	return root
}

// TestGitRepoRoot covers both directions: a directory inside a real git
// repository resolves to that repository's root, and a plain directory
// (a corpus fixture's own module/, in spirit — deliberately not a git repo)
// reports ok==false rather than an error, since that is the ordinary,
// expected case this function's caller (copyWorktree) must fall back from
// silently.
func TestGitRepoRoot(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)

	got, ok := gitRepoRoot(t.Context(), repo)
	if !ok {
		t.Fatal("gitRepoRoot() ok = false, want true for a real git repo")
	}

	// Resolve both sides through EvalSymlinks: on macOS, t.TempDir() lives
	// under /tmp, which is itself a symlink to /private/tmp, and `git
	// rev-parse --show-toplevel` reports the resolved path.
	wantResolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s) error = %v", repo, err)
	}

	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s) error = %v", got, err)
	}

	if gotResolved != wantResolved {
		t.Errorf("gitRepoRoot() = %q, want %q", got, repo)
	}

	plain := t.TempDir()

	if _, ok := gitRepoRoot(t.Context(), plain); ok {
		t.Error("gitRepoRoot() ok = true for a plain, non-git directory, want false")
	}
}

// TestGitWorktreeClean covers all three states copyWorktree's precondition
// cares about: a freshly committed repo is clean, an uncommitted edit to a
// tracked file is not, and an untracked file is not either — `git worktree
// add` checks out HEAD, so any of these would silently be missing from a
// worktree-based mutant's workspace.
func TestGitWorktreeClean(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)

	if !gitWorktreeClean(t.Context(), repo) {
		t.Error("gitWorktreeClean() = false immediately after commit, want true")
	}

	if err := os.WriteFile(filepath.Join(repo, "lib.go"), []byte("package gitfixture\n\n// modified, uncommitted.\nconst Marker = \"dirty\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if gitWorktreeClean(t.Context(), repo) {
		t.Error("gitWorktreeClean() = true with an uncommitted edit to a tracked file, want false")
	}

	runGit(t, repo, "checkout", "--", "lib.go")

	if !gitWorktreeClean(t.Context(), repo) {
		t.Fatal("gitWorktreeClean() = false after reverting the edit, want true (test setup problem, not the function under test)")
	}

	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package gitfixture\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if gitWorktreeClean(t.Context(), repo) {
		t.Error("gitWorktreeClean() = true with an untracked file present, want false")
	}
}

// TestCopyWorktree covers copyWorktree's own contract directly: a clean git
// repo produces a real, usable checkout at the returned root, whose cleanup
// actually removes the worktree (not just leaves it registered); a dirty
// repo and a non-git plain directory both report ok==false rather than
// attempting anything.
func TestCopyWorktree(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)
	dst := t.TempDir()

	root, cleanup, ok := copyWorktree(t.Context(), dst, repo)
	if !ok {
		t.Fatal("copyWorktree() ok = false for a clean git repo, want true")
	}

	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading checked-out go.mod: %v", err)
	}

	if !strings.Contains(string(data), "example.com/gitfixture") {
		t.Errorf("checked-out go.mod = %q, want it to name the fixture module", data)
	}

	cleanup()

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("worktree directory still exists after cleanup(): stat err = %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}

	if strings.Contains(string(out), filepath.Base(dst)) {
		t.Errorf("git worktree list still references the removed worktree:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package gitfixture\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, _, ok := copyWorktree(t.Context(), t.TempDir(), repo); ok {
		t.Error("copyWorktree() ok = true for a dirty git repo, want false")
	}

	if _, _, ok := copyWorktree(t.Context(), t.TempDir(), t.TempDir()); ok {
		t.Error("copyWorktree() ok = true for a plain, non-git directory, want false")
	}
}

// TestWorkspaceForFallsBackWithoutGit proves the zero-cost claim: with
// r.workspace left at its zero value ([WorkspaceCopy]), workspaceFor must
// behave exactly like calling copyModule directly, even when pointed at a
// real git repository -- it should never invoke git at all when worktree
// mode was never requested.
func TestWorkspaceForFallsBackWithoutGit(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)

	r := &runner{}

	root, cleanup, err := r.workspaceFor(t.Context(), t.TempDir(), repo, nil)
	if err != nil {
		t.Fatalf("workspaceFor() error = %v", err)
	}

	t.Cleanup(cleanup)

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("workspaceFor() did not produce a usable module copy: %v", err)
	}

	// A plain filesystem copy leaves .git behind untouched inside the
	// module tree (copyModule skips only the directory named .git at its
	// own root -- see its doc comment) -- the point here is narrower: this
	// confirms no *worktree* administrative entry was registered, i.e.
	// copyWorktree was never called.
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}

	if strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("git worktree list reports more than just the main worktree, want exactly one:\n%s", out)
	}
}

// TestWorkspaceForUsesWorktreeWhenRequested confirms the opt-in path
// actually takes effect: with r.workspace set to [WorkspaceWorktree] and a
// clean git repo, workspaceFor's cleanup must remove the worktree, not just
// hand back the outer temp directory to the caller's own defer as
// copyModule's callers already rely on.
func TestWorkspaceForUsesWorktreeWhenRequested(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)

	r := &runner{workspace: WorkspaceWorktree}
	tmp := t.TempDir()

	root, cleanup, err := r.workspaceFor(t.Context(), tmp, repo, nil)
	if err != nil {
		t.Fatalf("workspaceFor() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("workspaceFor() did not produce a usable module copy: %v", err)
	}

	if !strings.HasPrefix(root, tmp) {
		t.Errorf("workspaceFor() root = %q, want it rooted under tmp = %q", root, tmp)
	}

	cleanup()

	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}

	if strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("git worktree list still references a removed worktree, want only the main one:\n%s", out)
	}
}

// TestWorkspaceForPrefersClosureOverWorktree covers ROADMAP.md gap 5's
// activation: a non-nil dirs must win even when r.workspace requests a
// worktree and the module is a real, clean git repo — a worktree always
// checks out the whole repository at HEAD, which is never a narrower
// alternative to a pre-resolved closure copy (see workspaceFor's own doc
// comment). Proven the same way TestWorkspaceForFallsBackWithoutGit proves
// the copy-vs-worktree precedence: by asserting git never registered a
// worktree at all.
func TestWorkspaceForPrefersClosureOverWorktree(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)

	r := &runner{workspace: WorkspaceWorktree}

	root, cleanup, err := r.workspaceFor(t.Context(), t.TempDir(), repo, map[string]bool{repo: true})
	if err != nil {
		t.Fatalf("workspaceFor() error = %v", err)
	}

	t.Cleanup(cleanup)

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("workspaceFor() did not produce a usable module copy: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "lib.go")); err != nil {
		t.Errorf("workspaceFor() closure copy is missing lib.go: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}

	if strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("workspaceFor() used a git worktree despite a non-nil closure, want the closure copy only:\n%s", out)
	}
}
