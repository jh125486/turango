// Whitebox: runner.go exports no identifiers at all — mutant, runner,
// classify, parseTestEvents, copyModule and the rest are all unexported —
// so every test here needs direct package access; there is no exported
// surface left for a blackbox file to cover.
package mutate

import (
	"bytes"
	"fmt"
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

	// longLen is a fixed literal, deliberately not expressed in terms of
	// maxOutputBytes: the assertion below pins that constant's actual
	// configured value (16<<10), which a size built from the constant
	// itself could never catch drifting.
	const longLen = 20000

	long := strings.Repeat("x", longLen) + "TAIL"

	got := truncate(long)
	if !strings.HasSuffix(got, "TAIL") {
		t.Error("truncate() dropped the tail")
	}

	if !strings.HasPrefix(got, "...(truncated)...") {
		t.Error("truncate() did not mark the output as truncated")
	}

	const prefix = "...(truncated)...\n"
	if want := prefix + long[len(long)-16384:]; got != want {
		t.Errorf("truncate() kept %d bytes of tail, want exactly 16384 (maxOutputBytes)", len(got)-len(prefix))
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

// TestMutantCacheKey pins the compound cache key's assembly: every
// field mutant.cacheKey draws from (scope, TCE, fingerprint, mutant ID) plus
// the caller-supplied toolchain string must actually vary the resulting
// cacheKey, or two genuinely different mutants/runs could collide on the
// same cache entry — the identical "the restriction is enforced, not just
// documented" concern TestMutantTestArgs's own table already covers for the
// scope-to-flags mapping.
func TestMutantCacheKey(t *testing.T) {
	t.Parallel()

	base := mutant{
		scope:            ScopeFull,
		tceEnabled:       false,
		cacheFingerprint: "fp1",
		id:               "mutant1",
	}

	baseKey := base.cacheKey("go1.26 darwin/arm64")

	tests := map[string]struct {
		m    mutant
		tool string
	}{
		"scope":       {m: mutant{scope: ScopePackage, cacheFingerprint: "fp1", id: "mutant1"}, tool: "go1.26 darwin/arm64"},
		"tce":         {m: mutant{scope: ScopeFull, tceEnabled: true, cacheFingerprint: "fp1", id: "mutant1"}, tool: "go1.26 darwin/arm64"},
		"fingerprint": {m: mutant{scope: ScopeFull, cacheFingerprint: "fp2", id: "mutant1"}, tool: "go1.26 darwin/arm64"},
		"mutant id":   {m: mutant{scope: ScopeFull, cacheFingerprint: "fp1", id: "mutant2"}, tool: "go1.26 darwin/arm64"},
		"toolchain":   {m: base, tool: "go1.25 linux/amd64"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tt.m.cacheKey(tt.tool); got == baseKey {
				t.Errorf("cacheKey() unchanged when only %s varied: both = %+v", name, got)
			}
		})
	}

	if got := base.cacheKey("go1.26 darwin/arm64"); got != baseKey {
		t.Errorf("cacheKey() = %+v on identical inputs, want equal to %+v", got, baseKey)
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
		// Relative paths with no shared first segment: the common-prefix
		// loop empties parts entirely (distinct from "nothing at all"
		// above, which never even enters the loop), exercising commonDir's
		// own separate "if len(parts) == 0" branch.
		"no common root, relative paths": {paths: []string{"a/b", "c/d"}, want: ""},
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

// TestParseReplacesEdgeCases covers parseReplaces' own branches that no
// existing test (all of which go through a real, well-formed go.mod with a
// local replace) ever exercises: no go.mod at all, a go.mod that cannot
// even be read as a file (a non-ENOENT os.ReadFile error), a go.mod that
// fails to parse, and a replace directive whose right-hand side is a real
// module version rather than a filesystem path.
func TestParseReplacesEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("no go.mod at all", func(t *testing.T) {
		t.Parallel()

		out, err := parseReplaces(t.TempDir())
		if err != nil {
			t.Fatalf("parseReplaces() error = %v, want nil for a missing go.mod", err)
		}

		if out != nil {
			t.Errorf("parseReplaces() = %v, want nil", out)
		}
	})

	t.Run("moduleDir is a file, not a directory", func(t *testing.T) {
		t.Parallel()

		// filepath.Join(moduleDir, "go.mod") then names a path through a
		// regular file, which os.ReadFile fails to open with an error that
		// is not os.IsNotExist.
		notADir := filepath.Join(t.TempDir(), "notadir")
		writeFiles(t, map[string]string{notADir: "not a directory\n"})

		if _, err := parseReplaces(notADir); err == nil {
			t.Fatal("parseReplaces() error = nil, want a non-nil, non-IsNotExist read error")
		}
	})

	t.Run("malformed go.mod", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "this is not valid go.mod syntax\n"})

		if _, err := parseReplaces(moduleDir); err == nil {
			t.Fatal("parseReplaces() error = nil, want a non-nil error from modfile.Parse")
		}
	})

	t.Run("a non-local replace target is excluded", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\n" +
				"replace example.com/x => example.com/y v1.0.0\n",
		})

		out, err := parseReplaces(moduleDir)
		if err != nil {
			t.Fatalf("parseReplaces() error = %v", err)
		}

		if len(out) != 0 {
			t.Errorf("parseReplaces() = %v, want empty: the replace target is a module version, not a filesystem path", out)
		}
	})
}

// TestLocalReplacesError and TestInModuleReplaceTargetsError cover each
// function's own error propagation from parseReplaces — neither is ever
// exercised through resolveClosure/copyModule's own tests, which always use
// a well-formed go.mod.
func TestLocalReplacesError(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "not valid go.mod\n"})

	if _, err := localReplaces(moduleDir); err == nil {
		t.Fatal("localReplaces() error = nil, want a non-nil error from a malformed go.mod")
	}
}

func TestInModuleReplaceTargetsError(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "not valid go.mod\n"})

	if _, err := inModuleReplaceTargets(moduleDir); err == nil {
		t.Fatal("inModuleReplaceTargets() error = nil, want a non-nil error from a malformed go.mod")
	}
}

// TestPlaceRootsDedupeAndFallback covers two placeRoots branches
// TestCopyModuleWithLocalReplace's happy path never reaches: a duplicate
// root is only placed once, and roots with no usable common ancestor fall
// back to the flat, uniquely-named slot.
func TestPlaceRootsDedupeAndFallback(t *testing.T) {
	t.Parallel()

	t.Run("duplicate roots are deduped", func(t *testing.T) {
		t.Parallel()

		root := filepath.FromSlash("/a/b/c")

		dests := placeRoots("/dst", []string{root, root})
		if len(dests) != 1 {
			t.Fatalf("placeRoots() = %v, want exactly one entry for a duplicated root", dests)
		}
	})

	t.Run("no common ancestor falls back to a flat name", func(t *testing.T) {
		t.Parallel()

		// Relative, single-segment roots share no common ancestor at all:
		// commonDir returns "", forcing every root onto the fallback name.
		roots := []string{filepath.FromSlash("a"), filepath.FromSlash("b")}

		dests := placeRoots("/dst", roots)
		if len(dests) != 2 {
			t.Fatalf("placeRoots() = %v, want two distinct entries", dests)
		}

		for i, root := range roots {
			dest, ok := dests[root]
			if !ok {
				t.Fatalf("placeRoots() missing an entry for %q", root)
			}

			wantPrefix := filepath.FromSlash(fmt.Sprintf("/dst/_mod%d_", i))
			if !strings.HasPrefix(dest, wantPrefix) {
				t.Errorf("placeRoots()[%q] = %q, want it under the flat fallback name %q*", root, dest, wantPrefix)
			}
		}
	})
}

// TestRewriteReplacesErrors covers rewriteReplaces' own error and branch
// paths: a missing go.mod, a malformed one, a replace target absent from
// dests (silently skipped), a Rel failure between an absolute moduleDir and
// a relative dest, a rewritten path that needs "./" prepended because Rel's
// own result has no dot-prefix, an invalid old module path rejected by
// AddReplace, and a read-only go.mod that cannot be written back.
func TestRewriteReplacesErrors(t *testing.T) {
	t.Parallel()

	t.Run("go.mod does not exist", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()

		err := rewriteReplaces(moduleDir, []localReplace{{oldPath: "example.com/x", target: "/elsewhere"}}, map[string]string{"/elsewhere": "/dst/elsewhere"})
		if err == nil {
			t.Fatal("rewriteReplaces() error = nil, want a non-nil error for a missing go.mod")
		}
	})

	t.Run("malformed go.mod", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "not valid go.mod\n"})

		err := rewriteReplaces(moduleDir, []localReplace{{oldPath: "example.com/x", target: "/elsewhere"}}, map[string]string{"/elsewhere": "/dst/elsewhere"})
		if err == nil {
			t.Fatal("rewriteReplaces() error = nil, want a non-nil error from modfile.Parse")
		}
	})

	t.Run("replace target absent from dests is skipped", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		const original = "module example.com/mod\n\ngo 1.23\n\nreplace example.com/x => ../x\n"
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): original})

		// dests has no entry for "/elsewhere": the replace loop's own "if
		// !ok { continue }" branch, leaving go.mod's existing directive
		// untouched by AddReplace.
		if err := rewriteReplaces(moduleDir, []localReplace{{oldPath: "example.com/x", oldVersion: "", target: "/elsewhere"}}, map[string]string{}); err != nil {
			t.Fatalf("rewriteReplaces() error = %v, want nil: an absent dests entry should be silently skipped", err)
		}

		data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
		if err != nil {
			t.Fatalf("reading go.mod: %v", err)
		}

		if !strings.Contains(string(data), "../x") {
			t.Errorf("go.mod = %q, want the original replace target untouched", data)
		}
	})

	t.Run("filepath.Rel fails between an absolute moduleDir and a relative dest", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir() // absolute
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\nreplace example.com/x => ../x\n",
		})

		replaces := []localReplace{{oldPath: "example.com/x", target: "/elsewhere"}}
		dests := map[string]string{"/elsewhere": "relative/dest"} // relative: Rel(moduleDir, ...) fails

		if err := rewriteReplaces(moduleDir, replaces, dests); err == nil {
			t.Fatal("rewriteReplaces() error = nil, want a non-nil error from filepath.Rel")
		}
	})

	t.Run("a Rel result with no dot-prefix gets one prepended", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n\nreplace example.com/x => ../x\n",
		})

		// dest is a subdirectory of moduleDir itself, so
		// filepath.Rel(moduleDir, dest) yields "vendored/x" with no leading
		// "./" or "../" — rewriteReplaces must add the "./" itself.
		dest := filepath.Join(moduleDir, "vendored", "x")
		replaces := []localReplace{{oldPath: "example.com/x", target: "/elsewhere"}}
		dests := map[string]string{"/elsewhere": dest}

		if err := rewriteReplaces(moduleDir, replaces, dests); err != nil {
			t.Fatalf("rewriteReplaces() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
		if err != nil {
			t.Fatalf("reading go.mod: %v", err)
		}

		if !strings.Contains(string(data), "./vendored/x") {
			t.Errorf("go.mod = %q, want the rewritten replace to start with \"./\"", data)
		}
	})

	// No test here for mod.AddReplace's or mod.Format's own error returns
	// (rewriteReplaces' two lines calling them): as of golang.org/x/mod
	// v0.38.0 (this module's pinned version), neither
	// (*modfile.File).AddReplace's underlying addReplace (it neither
	// validates oldPath/newPath — AutoQuote only quotes, never rejects —
	// nor can the line-update path it takes fail) nor (*modfile.File).Format
	// (`return Format(f.Syntax), nil` — literally always nil) ever actually
	// returns a non-nil error. This makes rewriteReplaces' own
	// "if err := mod.AddReplace(...); err != nil" and
	// "if err != nil { ... }" (after mod.Format()) branches provably
	// unreachable with the dependency version currently in go.mod — not a
	// test gap, two genuine dead branches in production code. Reported in
	// the task summary rather than deleted, per this task's own
	// instructions not to edit runner.go.

	t.Run("go.mod cannot be written back", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		goModPath := filepath.Join(moduleDir, "go.mod")
		writeFiles(t, map[string]string{
			goModPath: "module example.com/mod\n\ngo 1.23\n\nreplace example.com/x => ../x\n",
		})

		if err := os.Chmod(goModPath, 0o400); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}

		t.Cleanup(func() { _ = os.Chmod(goModPath, 0o600) })

		dest := filepath.Join(t.TempDir(), "x")
		replaces := []localReplace{{oldPath: "example.com/x", target: "/elsewhere"}}
		dests := map[string]string{"/elsewhere": dest}

		if err := rewriteReplaces(moduleDir, replaces, dests); err == nil {
			t.Skip("rewriteReplaces() error = nil: running as a user that can write a read-only file (e.g. root); skipping")
		}
	})
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

// TestCopyModuleErrors covers copyModule's own two error-propagation
// points: localReplaces failing to parse a malformed go.mod, and copyTree
// failing on a moduleDir that does not exist.
func TestCopyModuleErrors(t *testing.T) {
	t.Parallel()

	t.Run("malformed go.mod", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "this is not a valid go.mod\n"})

		if _, err := copyModule(t.TempDir(), moduleDir); err == nil {
			t.Fatal("copyModule() error = nil, want a non-nil error from a malformed go.mod")
		}
	})

	t.Run("moduleDir does not exist", func(t *testing.T) {
		t.Parallel()

		missing := filepath.Join(t.TempDir(), "does-not-exist")

		if _, err := copyModule(t.TempDir(), missing); err == nil {
			t.Fatal("copyModule() error = nil, want a non-nil error copying a nonexistent moduleDir")
		}
	})
}

// The tests below cover the dependency-closure workspace copy's
// components (closureDirs, hasEmbedDirective, resolveClosure, copyClosure,
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

// TestClosureWalkVisitGuards covers visit's own leading guard clause
// directly against a hand-built closureWalk, rather than trying to coax
// closureDirs' map-based recursion into hitting each condition through
// iteration order (which Go does not guarantee): a nil package, an
// already-seen package, and a walk already marked unsafe by an earlier
// sibling must each return immediately without touching w.dirs.
func TestClosureWalkVisitGuards(t *testing.T) {
	t.Parallel()

	t.Run("nil package is ignored", func(t *testing.T) {
		t.Parallel()

		w := &closureWalk{dirs: map[string]bool{}, seen: map[string]bool{}, safe: true}
		w.visit(nil)

		if len(w.dirs) != 0 || !w.safe {
			t.Errorf("visit(nil) mutated the walk: dirs = %v, safe = %v", w.dirs, w.safe)
		}
	})

	t.Run("already-seen package is not reprocessed", func(t *testing.T) {
		t.Parallel()

		w := &closureWalk{
			moduleDir: "/mod",
			dirs:      map[string]bool{},
			seen:      map[string]bool{"example.com/mod/target": true},
			safe:      true,
		}

		w.visit(&packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  &packages.Module{Dir: "/mod"},
			GoFiles: []string{"/mod/target/target.go"},
		})

		if len(w.dirs) != 0 {
			t.Errorf("visit() reprocessed an already-seen package: dirs = %v, want empty", w.dirs)
		}
	})

	t.Run("already-unsafe walk short-circuits", func(t *testing.T) {
		t.Parallel()

		w := &closureWalk{
			moduleDir: "/mod",
			dirs:      map[string]bool{},
			seen:      map[string]bool{},
			safe:      false, // a sibling import already tripped this
		}

		w.visit(&packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  &packages.Module{Dir: "/mod"},
			GoFiles: []string{"/mod/target/target.go"},
		})

		if len(w.dirs) != 0 || len(w.seen) != 0 {
			t.Errorf("visit() kept processing after safe was already false: dirs = %v, seen = %v", w.dirs, w.seen)
		}
	})
}

// TestClosureDirsFallsBackToCompiledGoFiles covers the GoFiles-empty
// fallback: a package go/packages loaded without GoFiles set (e.g. a
// non-default build config) still has its directory determined from
// CompiledGoFiles instead.
func TestClosureDirsFallsBackToCompiledGoFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "mod")
	targetDir := filepath.Join(moduleDir, "target")

	writeFiles(t, map[string]string{filepath.Join(targetDir, "target.go"): "package target\n"})

	target := &packages.Package{
		PkgPath:         "example.com/mod/target",
		Module:          &packages.Module{Dir: moduleDir},
		CompiledGoFiles: []string{filepath.Join(targetDir, "target.go")},
	}

	dirs, ok := closureDirs(target)
	if !ok {
		t.Fatal("closureDirs() ok = false, want true: CompiledGoFiles alone should be enough to locate the package")
	}

	if want := map[string]bool{targetDir: true}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("closureDirs() = %v, want %v", dirs, want)
	}
}

// TestClosureDirsNoFilesIsUnsafe covers the case neither GoFiles nor
// CompiledGoFiles can answer: closureDirs cannot even determine the
// package's own directory, so the whole result must be unsafe.
func TestClosureDirsNoFilesIsUnsafe(t *testing.T) {
	t.Parallel()

	target := &packages.Package{
		PkgPath: "example.com/mod/target",
		Module:  &packages.Module{Dir: "/mod"},
	}

	if _, ok := closureDirs(target); ok {
		t.Error("closureDirs() ok = true, want false: neither GoFiles nor CompiledGoFiles is set")
	}
}

// TestResolveClosureNoModuleInfo covers the earliest resolveClosure guard:
// when none of the supplied package variants carry module information at
// all, there is nothing to resolve a closure against.
func TestResolveClosureNoModuleInfo(t *testing.T) {
	t.Parallel()

	if _, ok := resolveClosure(&packages.Package{PkgPath: "example.com/mod/target"}); ok {
		t.Error("resolveClosure() ok = true, want false: no package variant has Module info")
	}
}

// TestResolveClosureClosureDirsFails covers resolveClosure's own "if !ok"
// branch after calling closureDirs — distinct from the vendor/replace
// early-outs above, which never even reach closureDirs: a module with
// neither a vendor/ directory nor any replace directive, but whose target
// package has a //go:embed directive, must still fall back.
func TestResolveClosureClosureDirsFails(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	targetDir := filepath.Join(moduleDir, "target")

	writeFiles(t, map[string]string{
		filepath.Join(moduleDir, "go.mod"):    "module example.com/mod\n\ngo 1.23\n",
		filepath.Join(targetDir, "target.go"): "package target\n\n//go:embed data.txt\nvar data string\n",
	})

	target := &packages.Package{
		PkgPath: "example.com/mod/target",
		Module:  &packages.Module{Dir: moduleDir},
		GoFiles: []string{filepath.Join(targetDir, "target.go")},
	}

	if _, ok := resolveClosure(target); ok {
		t.Error("resolveClosure() ok = true, want false: the target package itself has a //go:embed directive")
	}
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

// TestCopyClosureErrors covers copyClosure's three error-propagation
// points: copying go.mod itself failing, filepath.Rel failing on a dirs
// entry that isn't absolute, and copyDirFiles failing on a dirs entry that
// does not exist.
func TestCopyClosureErrors(t *testing.T) {
	t.Parallel()

	t.Run("copying go.mod fails", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n"})

		dst := t.TempDir()
		// Pre-create the copy's destination go.mod as a directory, so
		// copyFile's os.OpenFile(..., O_TRUNC) collides with an existing
		// directory instead of writing a regular file.
		if err := os.MkdirAll(filepath.Join(dst, "mod", "go.mod"), 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}

		if _, err := copyClosure(dst, moduleDir, nil); err == nil {
			t.Fatal("copyClosure() error = nil, want a non-nil error copying go.mod over an existing directory")
		}
	})

	t.Run("filepath.Rel fails on a non-absolute dirs entry", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir() // absolute

		if _, err := copyClosure(t.TempDir(), moduleDir, map[string]bool{"relative/dir": true}); err == nil {
			t.Fatal("copyClosure() error = nil, want a non-nil error locating a relative dirs entry within an absolute moduleDir")
		}
	})

	t.Run("stating go.mod fails with a non-IsNotExist error", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(moduleDir, "go.mod"): "module example.com/mod\n\ngo 1.23\n"})

		// Denying execute/search permission on moduleDir itself makes
		// os.Stat(moduleDir/go.mod) fail with a permission error, not
		// ErrNotExist -- the one os.Stat failure mode the IsNotExist
		// continue-branch does not swallow.
		if err := os.Chmod(moduleDir, 0); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}

		//nolint:gosec // restoring this test's own t.TempDir() fixture to a usable (traversable) directory afterward, not attacker-controlled input
		t.Cleanup(func() { _ = os.Chmod(moduleDir, 0o750) })

		if _, err := copyClosure(t.TempDir(), moduleDir, nil); err == nil {
			t.Skip("copyClosure() error = nil: running as a user unaffected by directory permissions (e.g. root); skipping")
		}
	})

	t.Run("copyDirFiles fails on a nonexistent dirs entry", func(t *testing.T) {
		t.Parallel()

		moduleDir := t.TempDir()
		missing := filepath.Join(moduleDir, "does-not-exist")

		if _, err := copyClosure(t.TempDir(), moduleDir, map[string]bool{missing: true}); err == nil {
			t.Fatal("copyClosure() error = nil, want a non-nil error for a dirs entry that does not exist")
		}
	})
}

// TestCopySymlinkErrors covers copySymlink's two error-propagation points:
// os.Readlink failing on a path that isn't actually a symlink, and
// os.MkdirAll failing because a regular file blocks part of dst's path.
func TestCopySymlinkErrors(t *testing.T) {
	t.Parallel()

	t.Run("src is not a symlink", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		notALink := filepath.Join(dir, "plain.txt")
		writeFiles(t, map[string]string{notALink: "hello\n"})

		if err := copySymlink(notALink, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Fatal("copySymlink() error = nil, want a non-nil error from os.Readlink on a non-symlink")
		}
	})

	t.Run("dst path is blocked by a regular file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "link")
		target := filepath.Join(dir, "target.txt")
		writeFiles(t, map[string]string{target: "hello\n"})

		if err := os.Symlink(target, src); err != nil {
			t.Fatalf("creating symlink fixture: %v", err)
		}

		blocker := filepath.Join(t.TempDir(), "blocker")
		writeFiles(t, map[string]string{blocker: "not a directory\n"})

		if err := copySymlink(src, filepath.Join(blocker, "sub", "link")); err == nil {
			t.Fatal("copySymlink() error = nil, want a non-nil error from os.MkdirAll under a regular file")
		}
	})
}

// TestCopyDirFilesErrors covers copyDirFiles' error-propagation points and
// its non-regular-file skip, none of which the happy-path TestCopyDirFiles
// above exercises: os.ReadDir failing on a nonexistent src, the inner
// copySymlink call failing, the inner copyFile call failing, and a named
// pipe (neither a regular file nor a symlink) being silently skipped.
func TestCopyDirFilesErrors(t *testing.T) {
	t.Parallel()

	t.Run("src does not exist", func(t *testing.T) {
		t.Parallel()

		if err := copyDirFiles(filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir()); err == nil {
			t.Fatal("copyDirFiles() error = nil, want a non-nil error for a nonexistent src")
		}
	})

	t.Run("inner copySymlink fails", func(t *testing.T) {
		t.Parallel()

		src := t.TempDir()
		if err := os.Symlink(filepath.Join(src, "target.txt"), filepath.Join(src, "link")); err != nil {
			t.Fatalf("creating symlink fixture: %v", err)
		}

		blocker := filepath.Join(t.TempDir(), "blocker")
		writeFiles(t, map[string]string{blocker: "not a directory\n"})

		if err := copyDirFiles(src, filepath.Join(blocker, "sub")); err == nil {
			t.Fatal("copyDirFiles() error = nil, want a non-nil error when the inner copySymlink call fails")
		}
	})

	t.Run("inner copyFile fails", func(t *testing.T) {
		t.Parallel()

		src := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(src, "a.go"): "package p\n"})

		dst := filepath.Join(t.TempDir(), "out")
		// Pre-create the destination file as a directory so copyFile's
		// OpenFile collides with it.
		if err := os.MkdirAll(filepath.Join(dst, "a.go"), 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}

		if err := copyDirFiles(src, dst); err == nil {
			t.Fatal("copyDirFiles() error = nil, want a non-nil error when the inner copyFile call fails")
		}
	})

	t.Run("a named pipe is silently skipped", func(t *testing.T) {
		t.Parallel()

		src := t.TempDir()
		writeFiles(t, map[string]string{filepath.Join(src, "a.go"): "package p\n"})

		fifo := filepath.Join(src, "pipe")
		if err := runnerMkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unsupported on this platform: %v", err)
		}

		dst := filepath.Join(t.TempDir(), "out")

		if err := copyDirFiles(src, dst); err != nil {
			t.Fatalf("copyDirFiles() error = %v, want the named pipe silently skipped, not an error", err)
		}

		if _, err := os.Stat(filepath.Join(dst, "pipe")); !os.IsNotExist(err) {
			t.Errorf("copyDirFiles() copied the named pipe, want it skipped: stat err = %v", err)
		}

		if _, err := os.Stat(filepath.Join(dst, "a.go")); err != nil {
			t.Errorf("copyDirFiles() did not copy the regular file alongside the skipped pipe: %v", err)
		}
	})
}

// TestCopyTreeSymlinksAndSpecialFiles covers two copyTree cases the
// copyModule-driven TestCopyModuleWithLocalReplace above never exercises
// directly: a symlink preserved as a symlink (not resolved), and a named
// pipe silently skipped by the walk's default case — the same "nothing a
// build reads" contract copyDirFiles already gives its own shallow copy.
func TestCopyTreeSymlinksAndSpecialFiles(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	writeFiles(t, map[string]string{filepath.Join(src, "sub", "a.go"): "package p\n"})

	link := filepath.Join(src, "sub", "link.go")
	if err := os.Symlink(filepath.Join(src, "sub", "a.go"), link); err != nil {
		t.Fatalf("creating symlink fixture: %v", err)
	}

	fifo := filepath.Join(src, "sub", "pipe")

	fifoCreated := true
	if err := runnerMkfifo(fifo, 0o600); err != nil {
		fifoCreated = false
	}

	dst := t.TempDir()

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree() error = %v", err)
	}

	got, err := os.Readlink(filepath.Join(dst, "sub", "link.go"))
	if err != nil {
		t.Fatalf("copyTree() did not preserve link.go as a symlink: %v", err)
	}

	if want := filepath.Join(src, "sub", "a.go"); got != want {
		t.Errorf("copyTree() symlink target = %q, want %q", got, want)
	}

	if fifoCreated {
		if _, err := os.Stat(filepath.Join(dst, "sub", "pipe")); !os.IsNotExist(err) {
			t.Errorf("copyTree() copied the named pipe, want it skipped: stat err = %v", err)
		}
	}
}

// TestCopyTreeWalkDirError covers copyTree's own WalkDir-error-propagation
// branch: a subdirectory with its read permission removed makes
// filepath.WalkDir hand copyTree's callback a non-nil err directly (rather
// than a normal DirEntry), which must be returned rather than swallowed.
func TestCopyTreeWalkDirError(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	blocked := filepath.Join(src, "blocked")
	writeFiles(t, map[string]string{filepath.Join(blocked, "a.go"): "package p\n"})

	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	//nolint:gosec // restoring this test's own t.TempDir() fixture to a usable (traversable) directory afterward, not attacker-controlled input
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

	if err := copyTree(src, t.TempDir()); err == nil {
		t.Skip("copyTree() error = nil: running as a user unaffected by directory permissions (e.g. root); skipping")
	}
}

// TestCopyFileErrors covers copyFile's error-propagation points: os.Open
// failing on a nonexistent src, os.MkdirAll failing because a regular file
// blocks dst's parent directory, os.OpenFile failing because dst is itself
// an existing directory, and io.Copy failing because src -- opened
// successfully by os.Open, since a directory is a valid thing to open --
// cannot actually be read as file content.
func TestCopyFileErrors(t *testing.T) {
	t.Parallel()

	t.Run("src does not exist", func(t *testing.T) {
		t.Parallel()

		if err := copyFile(filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(t.TempDir(), "out"), 0o600); err == nil {
			t.Fatal("copyFile() error = nil, want a non-nil error for a nonexistent src")
		}
	})

	t.Run("dst parent is blocked by a regular file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "a.go")
		writeFiles(t, map[string]string{src: "package p\n"})

		blocker := filepath.Join(t.TempDir(), "blocker")
		writeFiles(t, map[string]string{blocker: "not a directory\n"})

		if err := copyFile(src, filepath.Join(blocker, "sub", "a.go"), 0o600); err == nil {
			t.Fatal("copyFile() error = nil, want a non-nil error from os.MkdirAll under a regular file")
		}
	})

	t.Run("dst is an existing directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "a.go")
		writeFiles(t, map[string]string{src: "package p\n"})

		dst := filepath.Join(t.TempDir(), "out")
		if err := os.MkdirAll(dst, 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}

		if err := copyFile(src, dst, 0o600); err == nil {
			t.Fatal("copyFile() error = nil, want a non-nil error opening an existing directory for write")
		}
	})

	t.Run("src is a directory: io.Copy cannot read it", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()

		if err := copyFile(srcDir, filepath.Join(t.TempDir(), "out"), 0o600); err == nil {
			t.Fatal("copyFile() error = nil, want a non-nil error reading a directory as file content")
		}
	})
}

// The tests below cover git-worktree execution: gitRepoRoot, gitWorktreeClean,
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

// TestGitPrefixFailure covers gitPrefix's own error return: a plain,
// non-git directory has no repository for `git rev-parse --show-prefix` to
// answer against.
func TestGitPrefixFailure(t *testing.T) {
	t.Parallel()

	if _, ok := gitPrefix(t.Context(), t.TempDir()); ok {
		t.Error("gitPrefix() ok = true for a plain, non-git directory, want false")
	}
}

// TestCopyWorktreeAddFailure covers copyWorktree's own `git worktree add`
// failure branch: with a regular file pre-existing at the exact path
// copyWorktree would check the worktree out to, `git worktree add` refuses
// to use it, and copyWorktree must report ok==false rather than a half-built
// result.
func TestCopyWorktreeAddFailure(t *testing.T) {
	t.Parallel()

	repo := gitFixtureModule(t)
	dst := t.TempDir()

	writeFiles(t, map[string]string{filepath.Join(dst, "wt"): "blocking the worktree checkout path\n"})

	if _, _, ok := copyWorktree(t.Context(), dst, repo); ok {
		t.Error("copyWorktree() ok = true, want false: `git worktree add` should fail against a path already occupied by a regular file")
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

	// Checked against the exact "worktree <path>" porcelain line, not a raw
	// substring search over the whole blob: --porcelain's output always
	// includes repo's own primary worktree entry first (its own directory,
	// with a real branch — unlike root/wt's detached checkout), and a
	// generic Contains(out, filepath.Base(dst)) risks matching that entry's
	// path by coincidence whenever its numeric t.TempDir() suffix collides
	// with dst's own (both are small sequential integers drawn from the
	// same counter, e.g. ".../001" vs ".../002" — confirmed flaky in CI:
	// https://github.com/jh125486/turango/actions/runs/31759817647). root
	// is exactly the worktree path copyWorktree registered (moduleDir here
	// is repo itself, so gitPrefix's rel is empty and root == wt with no
	// suffix), so this is the only line that removal is being asked about.
	if strings.Contains(string(out), "worktree "+root+"\n") {
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

// TestWorkspaceForPrefersClosureOverWorktree covers the dependency-closure
// workspace copy's activation: a non-nil dirs must win even when
// r.workspace requests a worktree and the module is a real, clean git repo — a worktree always
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

// The tests below cover run()'s own error paths — every one reachable
// without a real go binary, since each fails before goTest is ever reached
// (proven the same way TestRunSkipsNoOpMutation already does, with a
// deliberately unusable goBin) — plus direct unit coverage of a handful of
// small helpers (cacheLookup, renderNode, testArgs/packagePattern,
// isTCEEquivalent, decodeTestEvent/parseTestEvents) whose own error/edge
// branches the tests above never happen to exercise.

// runnerIfMutationFixture builds a small real function with an if-condition
// mutation that genuinely changes the printed source (so the no-op
// short-circuit can never be what makes a caller's test pass) — shared by
// the run()-error-path tests below, none of which expect the mutation to
// ever actually reach a toolchain call.
func runnerIfMutationFixture(t *testing.T) (fset *token.FileSet, file *ast.File, baseline []byte, stmt *ast.IfStmt, mutation mutator.Mutation) {
	t.Helper()

	const src = `package p

func f(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`
	fset = token.NewFileSet()

	var err error

	file, err = parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("Fprint() error = %v", err)
	}

	baseline = buf.Bytes()

	decl := file.Decls[0].(*ast.FuncDecl)
	stmt = decl.Body.List[0].(*ast.IfStmt)
	original := stmt.Cond

	mutation = mutator.Mutation{
		Description: "if -> false",
		Apply:       func() { stmt.Cond = ast.NewIdent("false") },
		Revert:      func() { stmt.Cond = original },
	}

	return fset, file, baseline, stmt, mutation
}

// TestRunTestArgsError covers run()'s earliest possible error return: when
// mutant.testArgs() itself fails — here, packagePattern's filepath.Rel
// erroring on an absolute-vs-relative moduleDir/pkgDir mismatch — run must
// return the error before ever creating a workspace or touching the
// toolchain.
func TestRunTestArgsError(t *testing.T) {
	t.Parallel()

	fset, file, baseline, stmt, mutation := runnerIfMutationFixture(t)

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	_, _, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "/nowhere/p.go",
		baseline:  baseline,
		moduleDir: filepath.FromSlash("/abs/module"), // absolute
		pkgDir:    "relative/pkg",                    // relative: filepath.Rel(moduleDir, pkgDir) fails
		operator:  "control/if",
		line:      4,
		scope:     ScopePackage,
		node:      stmt,
		mutation:  mutation,
	})
	if err == nil {
		t.Fatal("run() error = nil, want a non-nil error from testArgs()'s packagePattern failing")
	}
}

// TestRunWorkspaceCopyError covers workspaceFor's error return inside run():
// a nonexistent moduleDir makes copyModule's copyTree fail at the very first
// filepath.WalkDir callback (which also, incidentally, is copyTree's own
// walk-error-propagation branch — see copyTree's doc comment).
func TestRunWorkspaceCopyError(t *testing.T) {
	t.Parallel()

	fset, file, baseline, stmt, mutation := runnerIfMutationFixture(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	_, _, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      filepath.Join(missing, "p.go"),
		baseline:  baseline,
		moduleDir: missing,
		pkgDir:    missing,
		operator:  "control/if",
		line:      4,
		scope:     ScopePackage,
		node:      stmt,
		mutation:  mutation,
	})
	if err == nil {
		t.Fatal("run() error = nil, want a non-nil error from workspaceFor copying a nonexistent moduleDir")
	}
}

// TestRunRelocateMutatedFileError covers the filepath.Rel(m.moduleDir,
// m.path) error return: with a real, valid, absolute moduleDir (so
// workspaceFor itself succeeds) but a relative m.path, Rel cannot express
// one in terms of the other.
func TestRunRelocateMutatedFileError(t *testing.T) {
	t.Parallel()

	fset, file, baseline, stmt, mutation := runnerIfMutationFixture(t)

	moduleDir := t.TempDir()

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	_, _, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "p.go", // relative, while moduleDir is absolute
		baseline:  baseline,
		moduleDir: moduleDir,
		pkgDir:    moduleDir,
		operator:  "control/if",
		line:      4,
		scope:     ScopePackage,
		node:      stmt,
		mutation:  mutation,
	})
	if err == nil {
		t.Fatal("run() error = nil, want a non-nil error locating a relative path within an absolute moduleDir")
	}
}

// TestRunWriteMutatedFileError covers os.WriteFile's error return: the
// module being copied has a *directory* at the mutated file's own relative
// path, so writing the mutated source there afterward collides with an
// existing directory instead of a regular file.
func TestRunWriteMutatedFileError(t *testing.T) {
	t.Parallel()

	fset, file, baseline, stmt, mutation := runnerIfMutationFixture(t)

	moduleDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(moduleDir, "p.go"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	_, _, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      filepath.Join(moduleDir, "p.go"),
		baseline:  baseline,
		moduleDir: moduleDir,
		pkgDir:    moduleDir,
		operator:  "control/if",
		line:      4,
		scope:     ScopePackage,
		node:      stmt,
		mutation:  mutation,
	})
	if err == nil {
		t.Fatal("run() error = nil, want a non-nil error writing the mutated file over an existing directory")
	}
}

// TestRunMkdirTempError covers os.MkdirTemp's own error return: with TMPDIR
// pointed at a directory that does not exist, workspace creation fails
// before any copy or toolchain call is attempted. Not parallel: t.Setenv
// forbids it (and the whole point is a process-global env var, which is
// exactly the kind of shared state this project's own convention says not
// to run in parallel with siblings).
func TestRunMkdirTempError(t *testing.T) {
	fset, file, baseline, stmt, mutation := runnerIfMutationFixture(t)

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	r := &runner{goBin: "/nonexistent/go", testTimeout: MinBaselineTimeout}

	_, _, _, err := r.run(t.Context(), mutant{
		fset:      fset,
		file:      file,
		path:      "/nowhere/p.go",
		baseline:  baseline,
		moduleDir: "/nowhere",
		pkgDir:    "/nowhere",
		operator:  "control/if",
		line:      4,
		scope:     ScopePackage,
		node:      stmt,
		mutation:  mutation,
	})
	if err == nil {
		t.Fatal("run() error = nil, want a non-nil error from os.MkdirTemp with an unusable TMPDIR")
	}
}

// TestCacheLookup covers every branch cacheLookup itself decides on,
// directly, rather than only incidentally through run(): no cache
// configured, a replay bypassing even a populated cache, a genuine miss, an
// ordinary hit (Status/Output filled in from the record), and an
// equivalent hit (Status/Output left at their zero values, mirroring
// isTCEEquivalent's own return shape — see cacheLookup's own doc comment).
func TestCacheLookup(t *testing.T) {
	t.Parallel()

	const toolchain = "go1.26 darwin/arm64"

	base := mutant{scope: ScopeFull, cacheFingerprint: "fp1", id: "mutant1"}
	key := base.cacheKey(toolchain)
	skeleton := MutantResult{ID: base.id, Line: 4}

	tests := map[string]struct {
		cache      *cacheIndex
		replay     bool
		wantHit    bool
		wantOK     bool
		wantEquiv  bool
		wantStatus Status
		wantOutput string
	}{
		"no cache configured": {
			cache: nil,
		},
		"replay bypasses even a populated cache": {
			cache:  &cacheIndex{records: map[cacheKey]cacheRecord{key: {Key: key, Status: Killed, Output: "boom"}}},
			replay: true,
		},
		"cache miss": {
			cache: &cacheIndex{records: map[cacheKey]cacheRecord{}},
		},
		"ordinary hit fills status and output": {
			cache:      &cacheIndex{records: map[cacheKey]cacheRecord{key: {Key: key, Status: Killed, Output: "boom"}}},
			wantHit:    true,
			wantOK:     true,
			wantStatus: Killed,
			wantOutput: "boom",
		},
		"equivalent hit leaves status/output zero": {
			cache:     &cacheIndex{records: map[cacheKey]cacheRecord{key: {Key: key, Equivalent: true}}},
			wantHit:   true,
			wantEquiv: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := &runner{cache: tt.cache, toolchain: toolchain}

			m := base
			m.replay = tt.replay

			got, ok, equiv, hit := r.cacheLookup(m, skeleton)
			if hit != tt.wantHit {
				t.Fatalf("cacheLookup() hit = %v, want %v", hit, tt.wantHit)
			}

			if !hit {
				return
			}

			if ok != tt.wantOK || equiv != tt.wantEquiv {
				t.Errorf("cacheLookup() ok = %v, equivalent = %v, want ok=%v, equivalent=%v", ok, equiv, tt.wantOK, tt.wantEquiv)
			}

			if got.Status != tt.wantStatus || got.Output != tt.wantOutput {
				t.Errorf("cacheLookup() Status = %v, Output = %q, want %v, %q", got.Status, got.Output, tt.wantStatus, tt.wantOutput)
			}

			if got.ID != skeleton.ID || got.Line != skeleton.Line {
				t.Errorf("cacheLookup() did not preserve the caller's result skeleton: got ID=%q Line=%d", got.ID, got.Line)
			}
		})
	}
}

// TestRenderNodeUnsupportedNodeReturnsError covers renderNode's own error
// return: *ast.Field satisfies ast.Node (it has Pos/End) but is not an
// Expr, Stmt, Decl or Spec — none of the shapes go/printer's own printNode
// dispatch knows how to print — so Fprint takes its documented "unsupported
// node type" error return rather than panicking.
func TestRenderNodeUnsupportedNodeReturnsError(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	if _, err := renderNode(fset, &ast.Field{}); err == nil {
		t.Fatal("renderNode() error = nil, want a non-nil error for an unsupported node type")
	}
}

// TestIsTCEEquivalentPackagePatternError covers the one isTCEEquivalent
// branch that never reaches the toolchain at all: packagePattern failing
// before compileDisassembly is ever called. Unlike every other
// isTCEEquivalent test (see runner_integration_internal_test.go), this
// needs no real go binary — goBin is never actually invoked.
func TestIsTCEEquivalentPackagePatternError(t *testing.T) {
	t.Parallel()

	m := mutant{
		moduleDir:   filepath.FromSlash("/abs/module"),
		pkgDir:      "relative/pkg", // relative: filepath.Rel fails inside packagePattern
		tceBaseline: []byte("dummy baseline"),
	}

	if isTCEEquivalent(t.Context(), "/nonexistent/go", m.moduleDir, m) {
		t.Error("isTCEEquivalent() = true, want false: packagePattern must fail before any compile is attempted")
	}
}

func TestDecodeTestEvent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		line   string
		wantOK bool
	}{
		"valid event":                        {line: `{"Action":"pass","Package":"p"}`, wantOK: true},
		"not json at all":                    {line: "mathx/mathx.go:5:2: undefined: undefinedVar", wantOK: false},
		"starts with { but isn't valid json": {line: `{not valid json`, wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, ok := decodeTestEvent(tt.line); ok != tt.wantOK {
				t.Errorf("decodeTestEvent(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
		})
	}
}

// TestParseTestEventsRawNonJSONLines covers the fallback path some
// toolchains take: a raw compiler diagnostic line interleaved directly into
// stdout rather than wrapped in a JSON event — decodeTestEvent's own
// ok==false case, bubbling up into parseTestEvents' raw-output
// accumulation, which classify's compile-error heuristic then reads.
func TestParseTestEventsRawNonJSONLines(t *testing.T) {
	t.Parallel()

	const stdout = `mathx/mathx.go:5:2: undefined: undefinedVar
{"Action":"fail","Package":"example.com/fixture/mathx"}
`

	sawTest, testFailed, pkgFailed, buildFail, out := parseTestEvents([]byte(stdout))
	if sawTest || testFailed || buildFail {
		t.Errorf("parseTestEvents() sawTest=%v testFailed=%v buildFail=%v, want all false", sawTest, testFailed, buildFail)
	}

	if !pkgFailed {
		t.Error("parseTestEvents() pkgFailed = false, want true from the fail event")
	}

	if !strings.Contains(out, "undefined: undefinedVar") {
		t.Errorf("parseTestEvents() out = %q, want it to contain the raw diagnostic line", out)
	}
}
