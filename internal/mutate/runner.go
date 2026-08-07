package mutate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/jh125486/turango/internal/mutator"
)

// mutant is everything the runner needs to execute one mutation.
//
// The AST and its FileSet are shared with the engine and mutated in place: the
// runner applies the mutation, prints, and reverts before returning, so the
// caller's tree is always back to its pristine state.
type mutant struct {
	fset *token.FileSet
	file *ast.File

	// path is the absolute path of the file being mutated.
	path string

	// baseline is the printed form of the unmutated AST, used to detect
	// mutations that produce no visible source change.
	baseline []byte

	// moduleDir is the absolute root of the module containing path — the
	// directory holding its go.mod.
	moduleDir string

	// pkgDir is the absolute directory of the package containing path, used to
	// scope the mutant's test run.
	pkgDir string

	// scope selects which of the module's tests decide this mutant's verdict.
	scope Scope

	// covering names the tests that execute the mutated line. It is only
	// meaningful under [ScopeImpact], where empty means "no test in this
	// package touches this line" — a verdict on its own, with nothing to run.
	covering []string

	operator string
	line     int
	mutation mutator.Mutation
}

// runner executes mutants one at a time, each in its own throwaway copy of the
// target module.
type runner struct {
	// goBin is the real Go toolchain binary, resolved once per run. It is never
	// the bare string "go": turango may itself be installed as "go" on PATH, and
	// invoking that would recurse into the shim (see package goproxy).
	goBin string

	// testTimeout bounds a single mutant's test run.
	testTimeout time.Duration
}

// timeoutGrace is how long a mutant's process is allowed to overrun its
// `go test -timeout` before the runner kills it outright. `go test` reacts to
// its own timeout by dumping goroutine stacks and shutting down the test
// binary, which takes a moment; killing at exactly the timeout would lose that
// output and misreport the cause.
const timeoutGrace = 15 * time.Second

// maxOutputBytes caps the captured test output retained per mutant. A run over
// a real module produces thousands of mutants, and holding every full test log
// would dwarf everything else in the report; the tail is kept because that is
// where the failure is.
const maxOutputBytes = 16 << 10

// allPackages is the "everything" package pattern go test itself understands.
const allPackages = "./..."

// run executes a single mutation end to end and reports the verdict.
//
// It returns ok false — deliberately, not an error and not a result — when the
// mutation produces source byte-identical to the original. Such a mutation is
// not a mutant at all: go/printer normalised it away (an implicit empty
// statement replacing an already-empty one, say), so testing it would waste a
// full `go test` cycle to learn nothing, and recording it would inflate the
// denominator of the score with a mutant that does not exist.
func (r *runner) run(ctx context.Context, m mutant) (result MutantResult, ok bool, err error) {
	m.mutation.Apply()
	// Revert unconditionally, including on every error path: the AST is shared
	// with the rest of the walk, so leaving a mutation applied would silently
	// compound it into every later mutant.
	defer m.mutation.Revert()

	var mutated bytes.Buffer
	if err := printer.Fprint(&mutated, m.fset, m.file); err != nil {
		return MutantResult{}, false, fmt.Errorf("mutate: printing mutated %s: %w", m.path, err)
	}

	src := mutated.Bytes()
	if bytes.Equal(src, m.baseline) {
		return MutantResult{}, false, nil
	}

	result = MutantResult{
		File:        m.path,
		Line:        m.line,
		Operator:    m.operator,
		Description: m.mutation.Description,
	}

	// Under impact scope a line no test executes needs no test run at all: the
	// answer is already known. This is the whole point of building the coverage
	// map — not a shortcut around running the tests, but the observation that
	// running them could not possibly produce a different verdict. It is also
	// where the scope pays for itself, since untested code is exactly where
	// mutants pile up.
	if m.scope == ScopeImpact && len(m.covering) == 0 {
		result.Status = Survived
		result.Output = "turango: no test in this package executes this line, so nothing could have caught the mutation"

		return result, true, nil
	}

	args, err := m.testArgs()
	if err != nil {
		return MutantResult{}, false, err
	}

	tmp, err := os.MkdirTemp("", "turango-mutant-")
	if err != nil {
		return MutantResult{}, false, fmt.Errorf("mutate: creating workspace: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	moduleDir, err := copyModule(tmp, m.moduleDir)
	if err != nil {
		return MutantResult{}, false, err
	}

	rel, err := filepath.Rel(m.moduleDir, m.path)
	if err != nil {
		return MutantResult{}, false, fmt.Errorf("mutate: locating %s within %s: %w", m.path, m.moduleDir, err)
	}

	if err := os.WriteFile(filepath.Join(moduleDir, rel), src, 0o600); err != nil {
		return MutantResult{}, false, fmt.Errorf("mutate: writing mutated %s: %w", rel, err)
	}

	stdout, stderr, timedOut, err := r.goTest(ctx, moduleDir, args)
	if err != nil {
		return MutantResult{}, false, err
	}

	if timedOut {
		// A mutant that never terminates is a mutant the suite noticed, even if
		// it noticed by hanging: the un-mutated code completes and this does
		// not. Counting it as Survived would be plainly wrong, and NotViable is
		// reserved for code that does not compile.
		result.Status = Killed
		result.Output = truncate(string(stdout) + string(stderr) + "\nturango: mutant exceeded the per-mutant timeout")

		return result, true, nil
	}

	result.Status, result.Output = classify(stdout, stderr)

	return result, true, nil
}

// testArgs renders the trailing `go test` arguments that decide which tests get
// to judge this mutant — the flag surface of [Scope], resolved per mutant.
//
// The package pattern is relative to the module root because the tests run in a
// *copy* of the module: an absolute path would point back at the original
// sources, which are unmutated.
func (m mutant) testArgs() ([]string, error) {
	if m.scope == ScopeFull {
		return []string{allPackages}, nil
	}

	pattern, err := packagePattern(m.moduleDir, m.pkgDir)
	if err != nil {
		return nil, err
	}

	if m.scope != ScopeImpact {
		return []string{pattern}, nil
	}

	// -coverpkg names the package under test explicitly, exactly as the
	// measurement run did. It matters for an external `p_test` package, whose
	// coverage would otherwise be attributed to the test package rather than to
	// p, and keeping the two invocations' build flags identical keeps the
	// selected tests running against the same instrumented shape they were
	// measured in.
	return []string{
		"-run=^(" + strings.Join(m.covering, "|") + ")$",
		"-coverpkg=" + pattern,
		pattern,
	}, nil
}

// packagePattern renders the `go test` pattern that selects just the mutated
// package, relative to the module root.
//
// It backs [ScopePackage], and is the base pattern [ScopeImpact] narrows
// further with -run.
func packagePattern(moduleDir, pkgDir string) (string, error) {
	rel, err := filepath.Rel(moduleDir, pkgDir)
	if err != nil {
		return "", fmt.Errorf("mutate: locating package %s within %s: %w", pkgDir, moduleDir, err)
	}

	if rel == "." {
		return ".", nil
	}

	return "./" + filepath.ToSlash(rel), nil
}

// goTest runs the mutant's selected tests in dir and captures the test2json
// event stream. args are the scope-dependent trailing arguments from
// [mutant.testArgs].
//
// It shells out directly rather than going through goproxy.Run: on Unix that is
// a syscall.Exec process replacement, which never returns and would take the
// engine with it. goproxy is still the only source of the binary path.
//
// timedOut reports that the mutant was killed by the runner's own deadline, as
// opposed to the parent context being cancelled — the caller must distinguish
// "this mutant hung" from "the user pressed Ctrl+C".
func (r *runner) goTest(ctx context.Context, dir string, args []string) (stdout, stderr []byte, timedOut bool, err error) {
	runCtx, cancel := context.WithTimeout(ctx, r.testTimeout+timeoutGrace)
	defer cancel()

	argv := append([]string{
		"test",
		"-json",
		// Test caching keys on the compiled inputs, and every mutant changes
		// them, so caching can never help here; -count=1 removes any chance of
		// a stale result being replayed for a mutant that only looks identical.
		"-count=1",
		// Vet failures are reported the same way build failures are, which
		// would classify perfectly viable mutants as NotViable. The question
		// this tool asks is whether the *tests* catch the change, not whether
		// vet does.
		"-vet=off",
		// -parallel is go test's *in-binary* parallelism and defaults to
		// GOMAXPROCS, which is a per-test-binary default that assumes it has
		// the machine to itself. It does not: -mutateparallel already runs that
		// many test binaries at once, and the two multiply into
		// GOMAXPROCS x workers threads fighting over the same cores — enough to
		// push slow mutants past their timeout and misreport them as killed. A
		// user who wants the multiplied concurrency can still ask for it
		// through the test-flag passthrough.
		"-parallel=1",
		"-timeout=" + r.testTimeout.String(),
	}, args...)

	//nolint:gosec // running "go test" against a mutated copy of the module is turango's core function, not attacker-controlled input
	cmd := exec.CommandContext(runCtx, r.goBin, argv...)
	cmd.Dir = dir

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	// A non-zero exit is the normal, expected outcome for a killed mutant, so
	// only a failure to start the toolchain at all is an engine error.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, nil, false, fmt.Errorf("mutate: running %s test: %w", r.goBin, runErr)
	}

	if ctx.Err() != nil {
		return nil, nil, false, ctx.Err()
	}

	return outBuf.Bytes(), errBuf.Bytes(), runCtx.Err() != nil, nil
}

// goTestSuite returns a [suiteTimer] that runs the unmutated suite for patterns
// in dir and reports its wall-clock duration.
//
// It runs against the original sources, not a workspace copy: nothing is
// mutated, so there is nothing to isolate, and timing the copy would fold the
// copy's cost into a number that is meant to describe the suite.
//
// -count=1 is load-bearing rather than defensive here: without it the second and
// third runs would be served from the test cache in microseconds and the mean
// would be a third of the truth.
func goTestSuite(goBin, dir string, patterns []string) suiteTimer {
	return func(ctx context.Context) (time.Duration, error) {
		args := append([]string{"test", "-count=1", "-vet=off"}, patterns...)

		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir = dir

		start := time.Now()
		out, err := cmd.CombinedOutput()
		elapsed := time.Since(start)

		if err != nil {
			return 0, fmt.Errorf("the unmutated test suite does not pass: %w\n%s", err, truncate(string(out)))
		}

		return elapsed, nil
	}
}

// testEvent is the subset of cmd/test2json's event shape the classifier reads.
type testEvent struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string
}

// compileError matches a Go compiler diagnostic line ("./x.go:7:9: undefined:
// y"). It is only ever consulted for output produced before any test-level
// event, so it cannot be fooled by a test that happens to print something of
// the same shape.
var compileError = regexp.MustCompile(`(?m)^.*\.go:\d+(:\d+)?: `)

// classify derives a mutant's verdict from a `go test -json` run, and returns
// the reconstructed plain-text test output alongside it.
//
// The three verdicts are distinguished by whether the stream ever reaches the
// test level:
//
//   - A build failure never does. Older toolchains emit the compiler's
//     diagnostics on stderr and follow with a package-level "fail"; newer ones
//     wrap them in "build-output"/"build-fail" events keyed by ImportPath. Both
//     produce a failure with no test ever having run, which is NotViable — the
//     mutation was not testable, so it says nothing about the suite.
//   - Tests ran and something failed: Killed.
//   - Tests ran and nothing failed: Survived. A package with no test files at
//     all lands here too, which is correct: nothing was watching, so nothing
//     caught it.
func classify(stdout, stderr []byte) (status Status, output string) {
	sawTest, testFailed, pkgFailed, buildFail, text := parseTestEvents(stdout)
	out := text + string(stderr)

	switch {
	case buildFail:
		return NotViable, truncate(out)
	case !sawTest && (pkgFailed || compileError.MatchString(out)):
		return NotViable, truncate(out)
	case testFailed || pkgFailed:
		return Killed, truncate(out)
	default:
		return Survived, truncate(out)
	}
}

// parseTestEvents walks a `go test -json` stdout stream and extracts the
// signals classify needs: whether any test ran, whether a test or the
// package failed, whether the build itself failed, and the reconstructed
// plain-text output.
func parseTestEvents(stdout []byte) (sawTest, testFailed, pkgFailed, buildFail bool, out string) {
	var text, raw strings.Builder

	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var ev testEvent
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil {
			// Not an event: some toolchains interleave raw build diagnostics
			// into stdout. Keep it as output and let the compile-error check
			// see it.
			raw.WriteString(line)
			raw.WriteString("\n")

			continue
		}

		if ev.Output != "" {
			text.WriteString(ev.Output)
		}

		if ev.Test != "" {
			sawTest = true
		}

		switch ev.Action {
		case "build-fail":
			buildFail = true
		case "fail":
			if ev.Test != "" {
				testFailed = true
			} else {
				pkgFailed = true
			}
		}
	}

	return sawTest, testFailed, pkgFailed, buildFail, text.String() + raw.String()
}

// truncate caps captured output at maxOutputBytes, keeping the tail.
func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}

	return "...(truncated)...\n" + s[len(s)-maxOutputBytes:]
}

// copyModule builds a throwaway copy of the module rooted at moduleDir inside
// dst and reports the copy's module root.
//
// The copy is a full recursive copy, not a copy of the .go files: //go:embed
// directives reference arbitrary assets by relative path, a vendor/ directory
// must stay consistent with the go.mod beside it, and testdata is often load
// bearing. Only .git is skipped, since it can dwarf the module itself and no
// build reads it.
//
// Local replace directives are followed: the replacement module is copied too,
// positioned so that its path relative to the main module is preserved, and the
// directive is rewritten to the new relative path. Without this a module that
// replaces a dependency with a sibling checkout could not be built at all
// outside its original directory.
func copyModule(dst, moduleDir string) (string, error) {
	replaces, err := localReplaces(moduleDir)
	if err != nil {
		return "", err
	}

	roots := []string{moduleDir}
	for _, rp := range replaces {
		roots = append(roots, rp.target)
	}

	dests := placeRoots(dst, roots)

	for src, dest := range dests {
		if err := copyTree(src, dest); err != nil {
			return "", err
		}
	}

	newModuleDir := dests[moduleDir]

	if len(replaces) > 0 {
		if err := rewriteReplaces(newModuleDir, replaces, dests); err != nil {
			return "", err
		}
	}

	return newModuleDir, nil
}

// localReplace is a replace directive whose right-hand side is a filesystem
// path rather than a module version.
type localReplace struct {
	oldPath    string
	oldVersion string
	// target is the replacement's absolute, cleaned directory.
	target string
}

// localReplaces reads moduleDir's go.mod and returns its filesystem-path
// replace directives.
//
// Replacements pointing inside the module itself are ignored: they are copied
// wholesale with the module and their relative paths still resolve, so
// rewriting them would only risk breaking something that already works.
func localReplaces(moduleDir string) ([]localReplace, error) {
	path := filepath.Join(moduleDir, "go.mod")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("mutate: reading %s: %w", path, err)
	}

	mod, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, fmt.Errorf("mutate: parsing %s: %w", path, err)
	}

	var out []localReplace

	for _, rep := range mod.Replace {
		if !isLocalPath(rep.New.Path) {
			continue
		}

		target := rep.New.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(moduleDir, target)
		}

		target = filepath.Clean(target)
		if target == moduleDir || strings.HasPrefix(target, moduleDir+string(filepath.Separator)) {
			continue
		}

		out = append(out, localReplace{
			oldPath:    rep.Old.Path,
			oldVersion: rep.Old.Version,
			target:     target,
		})
	}

	return out, nil
}

// isLocalPath reports whether a replace directive's right-hand side names a
// directory rather than a module. This is go.mod's own rule: a replacement is a
// filesystem path exactly when it is absolute or begins with ./ or ../.
func isLocalPath(path string) bool {
	return filepath.IsAbs(path) ||
		strings.HasPrefix(path, "./") ||
		strings.HasPrefix(path, "../") ||
		path == "." ||
		path == ".."
}

// placeRoots decides where each source root lands inside dst.
//
// Roots are laid out under their deepest common ancestor so that the relative
// offsets between them — which is what a `replace ../sibling` directive depends
// on — survive the copy. Roots with no usable common ancestor (different
// Windows volumes, say) fall back to a flat, uniquely-named slot; the directive
// rewrite fixes up the path either way.
func placeRoots(dst string, roots []string) map[string]string {
	common := commonDir(roots)
	dests := make(map[string]string, len(roots))

	for i, root := range roots {
		if _, seen := dests[root]; seen {
			continue
		}

		rel, err := filepath.Rel(common, root)
		if common == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rel = fmt.Sprintf("_mod%d_%s", i, filepath.Base(root))
		}

		dests[root] = filepath.Join(dst, rel)
	}

	return dests
}

// commonDir returns the deepest directory that is a prefix of every path, or ""
// if they share none.
func commonDir(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	sep := string(filepath.Separator)
	parts := strings.Split(filepath.Clean(paths[0]), sep)

	for _, p := range paths[1:] {
		other := strings.Split(filepath.Clean(p), sep)

		n := min(len(other), len(parts))

		i := 0
		for i < n && parts[i] == other[i] {
			i++
		}

		parts = parts[:i]
	}

	if len(parts) == 0 {
		return ""
	}

	if len(parts) == 1 && parts[0] == "" {
		return sep
	}

	return strings.Join(parts, sep)
}

// rewriteReplaces points the copied go.mod's local replace directives at the
// copied replacement modules.
func rewriteReplaces(moduleDir string, replaces []localReplace, dests map[string]string) error {
	path := filepath.Join(moduleDir, "go.mod")

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mutate: reading copied %s: %w", path, err)
	}

	mod, err := modfile.Parse(path, data, nil)
	if err != nil {
		return fmt.Errorf("mutate: parsing copied %s: %w", path, err)
	}

	for _, rp := range replaces {
		dest, ok := dests[rp.target]
		if !ok {
			continue
		}

		rel, err := filepath.Rel(moduleDir, dest)
		if err != nil {
			return fmt.Errorf("mutate: relocating replace %s: %w", rp.oldPath, err)
		}

		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "./") && !strings.HasPrefix(rel, "../") {
			rel = "./" + rel
		}

		if err := mod.AddReplace(rp.oldPath, rp.oldVersion, rel, ""); err != nil {
			return fmt.Errorf("mutate: rewriting replace %s: %w", rp.oldPath, err)
		}
	}

	mod.Cleanup()

	out, err := mod.Format()
	if err != nil {
		return fmt.Errorf("mutate: formatting copied go.mod: %w", err)
	}

	//nolint:gosec // path is moduleDir/go.mod inside turango's own temp workspace copy, not external input
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("mutate: writing copied go.mod: %w", err)
	}

	return nil
}

// copyTree recursively copies src to dst, preserving relative layout, file
// modes, and symlinks.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		switch {
		case entry.IsDir():
			if entry.Name() == ".git" && path != src {
				return fs.SkipDir
			}

			info, err := entry.Info()
			if err != nil {
				return err
			}

			// The owner write and search bits are forced on regardless of the
			// source mode: a read-only directory in the original would stop us
			// populating its copy.
			return os.MkdirAll(target, info.Mode().Perm()|0o700)

		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}

			//nolint:gosec // copying the developer's own trusted module into a temp workspace, not a multi-tenant or attacker-controlled path
			return os.Symlink(link, target)

		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				return err
			}

			return copyFile(path, target, info.Mode().Perm())

		default:
			// Sockets, devices and named pipes: nothing a build reads.
			return nil
		}
	})
}

// copyFile copies one regular file, creating the destination with mode.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o200)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}
