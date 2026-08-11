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
	"sync/atomic"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"

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

	// node is the walk's outer node for this mutation — the fallback
	// before/after render target when mutation.Node is nil (see
	// [renderNode]).
	node ast.Node

	// id is this mutation's content-hashed, stable identifier — see
	// [mutantID].
	id string

	// tceBaseline is the mutated package's normalized -S disassembly,
	// compiled once from the *unmutated* sources before any of the
	// package's mutants ran (see [engine.planPackage]). Nil means TCE is
	// not active for this mutant, either because [Options.TCE] is unset or
	// because the baseline compile itself failed and the package was
	// fail-soft demoted to running without TCE.
	tceBaseline []byte

	// closureDirs is the mutated package's forward dependency closure (see
	// [resolveClosure]), computed once per package in [engine.planPackage]
	// — non-nil only when scope is not [ScopeFull] and the closure was
	// resolved safely. Nil means "use the whole-module copy" ([copyModule]
	// or [copyWorktree]), either because scope is ScopeFull (where a
	// forward closure is provably wrong — see ROADMAP.md gap 5) or because
	// resolveClosure declined (a vendor/ directory, an embed directive, an
	// unsafe replace target) and the caller must fall back.
	closureDirs map[string]bool
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

	// workspace selects how each mutant's throwaway execution copy is
	// built — see [workspaceFor].
	workspace Workspace
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

// goModFile is the file every Go module is rooted by.
const goModFile = "go.mod"

// execCalls counts every subprocess turango spawns to actually build or
// run the target module's code — [runner.goTest] (a mutant's test run),
// [goTestSuite] (a baseline timing sample, real or [Estimate]'s
// per-package one) and [compileDisassembly] (a TCE compile) — as opposed
// to go/packages' own `go list` metadata queries, which are comparatively
// cheap and not what this counter is about.
//
// It exists so a test can prove [walkForEstimate]'s counting phase spawns
// none of these — the concrete, checkable form of ROADMAP.md gap 11a's
// claim that estimate mode's walk costs "an AST walk with no go test
// subprocess anywhere" — the same call-counter technique [loadTypedCalls]
// (engine.go) already established to prove loadTyped is skipped when
// unneeded.
var execCalls atomic.Int64

// run executes a single mutation end to end and reports the verdict.
//
// It returns ok false — deliberately, not an error and not a result — when the
// mutation produces source byte-identical to the original. Such a mutation is
// not a mutant at all: go/printer normalised it away (an implicit empty
// statement replacing an already-empty one, say), so testing it would waste a
// full `go test` cycle to learn nothing, and recording it would inflate the
// denominator of the score with a mutant that does not exist.
//
// equivalent reports a different kind of "nothing to score": the mutation is
// a real, syntactically-different mutant, but Trivial Compiler Equivalence
// (TCE) found its compiled output identical to the baseline's, so it was
// never run against the suite either. ok and equivalent are never both true.
func (r *runner) run(ctx context.Context, m mutant) (result MutantResult, ok, equivalent bool, err error) {
	// The before/after render target: the mutation's own node if it set one
	// (needed when the mutation replaces a whole sub-node rather than
	// editing a field of the walk's outer node — see statement/remover's
	// doc comment), else the walk's outer node.
	renderTarget := m.mutation.Node
	if renderTarget == nil {
		renderTarget = m.node
	}

	before, _ := renderNode(m.fset, renderTarget)

	m.mutation.Apply()
	// Revert unconditionally, including on every error path: the AST is shared
	// with the rest of the walk, so leaving a mutation applied would silently
	// compound it into every later mutant.
	defer m.mutation.Revert()

	var mutated bytes.Buffer
	if err := printer.Fprint(&mutated, m.fset, m.file); err != nil {
		return MutantResult{}, false, false, fmt.Errorf("mutate: printing mutated %s: %w", m.path, err)
	}

	src := mutated.Bytes()
	if bytes.Equal(src, m.baseline) {
		return MutantResult{}, false, false, nil
	}

	result = MutantResult{
		ID:          m.id,
		File:        m.path,
		Line:        m.line,
		Operator:    m.operator,
		Description: m.mutation.Description,
		Before:      before,
		After:       renderAfter(m.fset, renderTarget, before),
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

		return result, true, false, nil
	}

	args, err := m.testArgs()
	if err != nil {
		return MutantResult{}, false, false, err
	}

	tmp, err := os.MkdirTemp("", "turango-mutant-")
	if err != nil {
		return MutantResult{}, false, false, fmt.Errorf("mutate: creating workspace: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	moduleDir, cleanup, err := r.workspaceFor(ctx, tmp, m.moduleDir, m.closureDirs)
	if err != nil {
		return MutantResult{}, false, false, err
	}

	defer cleanup()

	rel, err := filepath.Rel(m.moduleDir, m.path)
	if err != nil {
		return MutantResult{}, false, false, fmt.Errorf("mutate: locating %s within %s: %w", m.path, m.moduleDir, err)
	}

	if err := os.WriteFile(filepath.Join(moduleDir, rel), src, 0o600); err != nil {
		return MutantResult{}, false, false, fmt.Errorf("mutate: writing mutated %s: %w", rel, err)
	}

	// TCE sits here deliberately: after the workspace copy and the mutated
	// file write (compiling needs the whole module graph to resolve
	// imports, same reason copyModule copies more than just the mutated
	// file), and before the expensive part (a full `go test` cycle). A
	// mutant whose compiled output matches the baseline byte-for-byte never
	// reaches r.goTest at all.
	if isTCEEquivalent(ctx, r.goBin, moduleDir, m) {
		return result, false, true, nil
	}

	stdout, stderr, timedOut, err := r.goTest(ctx, moduleDir, args)
	if err != nil {
		return MutantResult{}, false, false, err
	}

	if timedOut {
		// A mutant that never terminates is a mutant the suite noticed, even if
		// it noticed by hanging: the un-mutated code completes and this does
		// not. Counting it as Survived would be plainly wrong, and NotViable is
		// reserved for code that does not compile.
		result.Status = Killed
		result.Output = truncate(string(stdout) + string(stderr) + "\nturango: mutant exceeded the per-mutant timeout")

		return result, true, false, nil
	}

	result.Status, result.Output = classify(stdout, stderr)

	return result, true, false, nil
}

// renderNode prints node's source text, or "" with a nil error if node
// itself is nil — a mutant built directly rather than through the engine's
// walk (some tests construct one this way) simply has no before/after to
// report, which is not a failure.
func renderNode(fset *token.FileSet, node ast.Node) (string, error) {
	if node == nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return "", fmt.Errorf("mutate: rendering mutated node: %w", err)
	}

	return buf.String(), nil
}

// renderAfter renders node's post-Apply text — the caller must call this
// after Apply, with before already captured from the same node
// pre-Apply — and applies [MutantResult.After]'s documented reconciliation:
// identical before/after text means node's own printed form wasn't what
// changed (its containing list's slot was repointed elsewhere instead, a
// deleted statement say), so the honest "after" is empty, not a stale
// duplicate of before. A render error, on either side, is swallowed into an
// empty after rather than propagated: Before/After are a reporting nicety,
// not a reason to fail an otherwise-real mutant.
func renderAfter(fset *token.FileSet, node ast.Node, before string) string {
	after, err := renderNode(fset, node)
	if err != nil || after == before {
		return ""
	}

	return after
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
	execCalls.Add(1)

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

// tceBuildID is a fixed literal passed to every TCE compile — the
// once-per-package baseline and every mutant alike — so the toolchain's own
// content-derived build ID (which legitimately differs whenever the source
// changed, which is not the signal TCE measures) can never itself be the
// source of a spurious diff. See ROADMAP.md gap 2's design notes.
const tceBuildID = "turango-tce"

// positionComment matches the trailing source-position annotation `go tool
// compile -S` prints on each instruction line, e.g. "(pkg/file.go:7)".
// [compileDisassembly] strips these before comparing: position shifts
// whenever a mutation changes the source's line count, even when the
// generated code is byte-for-byte identical, so leaving them in would make
// the comparison detect almost nothing — confirmed empirically in ROADMAP.md
// gap 2's spike, where a textbook dead-store-elimination case differed only
// in this annotation.
var positionComment = regexp.MustCompile(`\([^)]*\.go:\d+\)`)

// compileDisassembly builds pattern in dir and returns its normalized -S
// disassembly: the target package's own assembly listing, made reproducible
// across different temp-dir copies of otherwise-identical source via
// -trimpath and a fixed build ID (see [tceBuildID]), with every trailing
// source-position comment stripped (see [positionComment]).
//
// -gcflags, not -gcflags=all=..., is deliberate: all= also dumps the
// assembly of every dependency in the build graph, confirmed the hard way in
// the spike — a one-package build pulled in the entire stdlib closure as a
// multi-million-line dump. Plain -gcflags scopes the -S output to pattern
// itself.
func compileDisassembly(ctx context.Context, goBin, dir, pattern string) ([]byte, error) {
	execCalls.Add(1)

	//nolint:gosec // compiling a package of the module under mutation for TCE is turango's core function, not attacker-controlled input
	cmd := exec.CommandContext(ctx, goBin,
		"build",
		"-trimpath",
		"-gcflags=-S -buildid="+tceBuildID,
		"-o", os.DevNull,
		pattern,
	)
	cmd.Dir = dir

	// -S writes the disassembly to stderr, not stdout; stdout is empty
	// (build output, ordinarily nothing on success).
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mutate: compiling %s for TCE: %w", pattern, err)
	}

	return positionComment.ReplaceAll(stderr.Bytes(), nil), nil
}

// isTCEEquivalent reports whether m's mutation, already applied and written
// into moduleDir, compiles to output identical to m.tceBaseline. false
// whenever m.tceBaseline is nil (TCE inactive for this mutant) or the
// compile itself fails.
//
// Fail soft, per mutant, the same shape ScopeImpact's coverage-map build and
// the typed-mutator binding already use elsewhere in this package: a mutant
// that fails *this* compile is not necessarily uncompilable, and the
// ordinary r.goTest call the caller falls through to already detects a
// genuinely uncompilable mutant and classifies it NotViable on its own (as
// does a cancelled ctx, which goTest's own context derives from and checks).
// An error here never aborts the run; it just means this one mutant runs
// without TCE's help.
func isTCEEquivalent(ctx context.Context, goBin, moduleDir string, m mutant) bool {
	if m.tceBaseline == nil {
		return false
	}

	pattern, err := packagePattern(m.moduleDir, m.pkgDir)
	if err != nil {
		return false
	}

	asm, err := compileDisassembly(ctx, goBin, moduleDir, pattern)
	if err != nil {
		return false
	}

	return bytes.Equal(asm, m.tceBaseline)
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
		execCalls.Add(1)

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

	for line := range strings.SplitSeq(string(stdout), "\n") {
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

// workspaceFor builds one mutant's throwaway execution copy, choosing among
// three strategies, and returns a cleanup func the caller must always call
// once done with the workspace (in addition to, not instead of, the
// caller's own tmp directory cleanup — see [copyWorktree]'s doc comment for
// why both are needed).
//
// Precedence, and why: dirs — this mutant's package's pre-resolved
// dependency closure (see [resolveClosure]) — wins first when non-nil.
// A worktree always checks out the *whole* repository at HEAD, so it is not
// a narrower alternative to a closure copy the way it is to [copyModule]; a
// non-nil dirs already implies scope is not [ScopeFull] (see
// [engine.planPackage]), and gap 5's own resolution is that a forward
// closure is only ever correct under a narrower scope in the first place.
// r.workspace's git-worktree option is only ever consulted when dirs is nil
// — i.e. under [ScopeFull], or whenever the closure could not be safely
// resolved for this package — and even then only actually used when
// [copyWorktree] reports it created one; every other case, including
// r.workspace being the zero value [WorkspaceCopy], falls back to
// [copyModule] transparently. The caller never has to know which strategy
// ran.
func (r *runner) workspaceFor(ctx context.Context, tmp, moduleDir string, dirs map[string]bool) (root string, cleanup func(), err error) {
	if dirs != nil {
		root, err = copyClosure(tmp, moduleDir, dirs)

		return root, func() {}, err
	}

	if r.workspace == WorkspaceWorktree {
		if wtRoot, wtCleanup, ok := copyWorktree(ctx, tmp, moduleDir); ok {
			return wtRoot, wtCleanup, nil
		}
	}

	root, err = copyModule(tmp, moduleDir)

	return root, func() {}, err
}

// copyWorktree builds a mutant's workspace with `git worktree add` instead
// of a filesystem copy: a worktree shares the repository's object store
// rather than duplicating files, and a sibling local replace-target module
// already resolves via its real relative path with no rewriting needed,
// since a worktree is the same repository checked out twice.
//
// ok reports whether a worktree was actually created; false means the
// caller must fall back to [copyModule] — moduleDir is not inside a usable
// git working tree (not a git repo at all, e.g. any corpus fixture under
// corpus/*/module/, which is deliberately a plain directory per ROADMAP.md
// gap 6's own "must never become a hard git dependency" note; or one with
// uncommitted changes, see [gitWorktreeClean]).
//
// `git worktree add` checks out HEAD — the last *commit* — into wt, so the
// returned root is only a faithful copy of moduleDir's current on-disk
// content when the working tree is clean; that precondition is
// [gitWorktreeClean]'s job, not this function's.
//
// The returned cleanup removes the worktree via `git worktree remove`, not
// a plain filesystem delete: a worktree is registered in the parent
// repository's .git/worktrees/ administrative area, and deleting only the
// checkout directory would leave that entry dangling — harmless once, but
// this runs per mutant, so an accumulating leak across a real run is not
// hypothetical. A cleanup failure is silently ignored, the same as
// copyModule's own os.RemoveAll(tmp) defer already does for its temp
// directory; the caller's tmp-directory cleanup runs afterward regardless
// and mops up whatever a failed `git worktree remove` left behind.
func copyWorktree(ctx context.Context, dst, moduleDir string) (root string, cleanup func(), ok bool) {
	repoRoot, gitOK := gitRepoRoot(ctx, moduleDir)
	if !gitOK || !gitWorktreeClean(ctx, repoRoot) {
		return "", nil, false
	}

	// moduleDir's path *within* the repository is asked of git directly
	// (`--show-prefix`) rather than computed with filepath.Rel(repoRoot,
	// moduleDir): on macOS, t.TempDir()-style paths under /var/... are
	// themselves a symlink to /private/var/..., which `git
	// rev-parse --show-toplevel` always resolves through, but a moduleDir
	// handed to this function is not guaranteed to be pre-resolved the same
	// way — comparing the two lexically with filepath.Rel silently produces
	// a wrong relative path in that case, one that (by the coincidence of
	// how many directory levels the mismatch costs) can even land back on
	// moduleDir's own original files instead of the new worktree's copy.
	// Asking git — which resolves both sides consistently, from the same
	// -C moduleDir invocation — sidesteps the mismatch entirely.
	rel, prefixOK := gitPrefix(ctx, moduleDir)
	if !prefixOK {
		return "", nil, false
	}

	wt := filepath.Join(dst, "wt")

	//nolint:gosec // repoRoot/wt are turango-controlled paths, not user input.
	if err := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "--detach", "--quiet", wt, "HEAD").Run(); err != nil {
		return "", nil, false
	}

	cleanup = func() {
		//nolint:gosec // same as above.
		_ = exec.CommandContext(context.WithoutCancel(ctx), "git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run()
	}

	return filepath.Join(wt, filepath.FromSlash(rel)), cleanup, true
}

// gitRepoRoot resolves the git repository containing dir, or ok==false if
// dir is not inside a git working tree at all.
func gitRepoRoot(ctx context.Context, dir string) (root string, ok bool) {
	//nolint:gosec // dir is turango-controlled (a resolved module directory), not user input.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(string(out)), true
}

// gitPrefix reports dir's path relative to its repository's root, computed
// by git itself (`--show-prefix`) rather than string-compared against
// [gitRepoRoot]'s result — see [copyWorktree]'s doc comment for why that
// comparison is unsafe. Empty string means dir is the repository root
// itself, matching git's own convention for this flag.
func gitPrefix(ctx context.Context, dir string) (rel string, ok bool) {
	//nolint:gosec // dir is turango-controlled (a resolved module directory), not user input.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-prefix").Output()
	if err != nil {
		return "", false
	}

	return strings.TrimSuffix(strings.TrimSpace(string(out)), "/"), true
}

// gitWorktreeClean reports whether repoRoot's working tree has no
// uncommitted changes anywhere in it, not just under the mutated module.
//
// `git worktree add` checks out HEAD, never the on-disk working copy; if
// repoRoot had any uncommitted edit — anywhere, not necessarily inside the
// mutated module itself — a worktree-based mutant would silently run
// against different source than [copyModule] would have copied. This
// checks the whole repository rather than just the module directory
// because an uncommitted edit outside the module but inside the same
// module's build graph (a sibling package the mutated one imports) has the
// identical exposure.
func gitWorktreeClean(ctx context.Context, repoRoot string) bool {
	//nolint:gosec // repoRoot is turango-controlled, not user input.
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain").Output()

	return err == nil && len(bytes.TrimSpace(out)) == 0
}

// closureDirs returns the set of same-module package directories pkg needs
// to build and test *on its own*: pkg's own directory, plus every
// same-module package it (transitively) imports. Standard-library and
// other-module imports are excluded entirely — they resolve from the build
// cache like any other build, no copying needed.
//
// closureDirs walks exactly the one *packages.Package it is given — it does
// not know about, and cannot discover on its own, sibling variants of the
// same target directory (a "[pkg.test]" test-augmented variant, or a
// black-box "pkg_test" external test package). A caller that hands it only
// the production variant gets a closure describing what the production code
// needs, not what `go test` needs. [resolveClosure] is the entry point that
// takes on assembling those variants into one root (see [mergeVariants])
// before calling here — callers wanting a test-complete closure must go
// through resolveClosure, not call closureDirs directly with a bare
// production package.
//
// ok is false whenever the closure cannot be safely computed, or safely
// copied narrower than the whole module — a package outside pkg's own
// module in the walk failing to resolve its own directory, or a
// `//go:embed` directive anywhere in the closure (embed paths are resolved
// relative to the package directory at build time, and only the referenced
// asset, not necessarily the whole package's file set, would need copying —
// correctly identifying that asset means parsing the directive, which this
// v1 does not do; declining to copy narrower is always safe, just not
// optimal, so an embed anywhere in the closure means "give up, tell the
// caller to fall back to copyModule" rather than risk shipping a workspace
// missing an asset the build needs). The caller's only correct response to
// ok==false is the existing full-module copyModule — never a
// partially-resolved closure.
//
// Wired into the engine via [engine.planPackage]/[runner.workspaceFor],
// gated to [ScopePackage]/[ScopeImpact] only — see ROADMAP.md gap 5 for why
// applying this under [ScopeFull] would be provably wrong, not just a
// missed optimisation.
func closureDirs(pkg *packages.Package) (dirs map[string]bool, ok bool) {
	if pkg.Module == nil || pkg.Module.Dir == "" {
		return nil, false
	}

	dirs = map[string]bool{}
	seen := map[string]bool{}
	safe := true

	var walk func(p *packages.Package)
	walk = func(p *packages.Package) {
		if p == nil || seen[p.PkgPath] || !safe {
			return
		}

		seen[p.PkgPath] = true

		if len(p.Errors) > 0 {
			safe = false

			return
		}

		if p.Module == nil || p.Module.Dir != pkg.Module.Dir {
			return // resolves from the module cache, nothing to copy
		}

		files := p.GoFiles
		if len(files) == 0 {
			files = p.CompiledGoFiles
		}

		if len(files) == 0 {
			safe = false // can't even determine this package's directory

			return
		}

		dirs[filepath.Dir(files[0])] = true

		if hasEmbedDirective(files) {
			safe = false

			return
		}

		for _, imp := range p.Imports {
			walk(imp)
		}
	}

	walk(pkg)

	if !safe {
		return nil, false
	}

	return dirs, true
}

// hasEmbedDirective reports whether any file in files contains a
// `//go:embed` directive — a cheap, conservative text scan, not directive
// parsing.
//
// A false positive (a file that merely mentions the string, in a comment or
// string literal unrelated to an actual directive) only costs an
// unnecessary fallback to the full-module copy — never a broken build. A
// false negative is not survivable — a missing embedded asset fails the
// build — so an unreadable file is treated the same as a match: this errs
// toward triggering the fallback, never toward skipping it.
func hasEmbedDirective(files []string) bool {
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || bytes.Contains(data, []byte("//go:embed")) {
			return true
		}
	}

	return false
}

// resolveClosure is [closureDirs] plus three other reasons a narrower copy
// isn't attempted for v1: a `vendor/` directory (vendored dependencies'
// actual files may not live where the import graph says they do), any local
// `replace` directive whose target lies outside the module (re-scoping
// replace-target copying to a partial closure, and rewriting it, is real
// complexity this v1 does not take on — see ROADMAP.md gap 5's design
// sketch), or an in-module local `replace` whose target directory falls
// outside the computed closure (see the loop below). Any of the three means
// "fall back to copyModule," the same as an unsafe [closureDirs] result.
//
// pkgs is every go/packages variant of the target package relevant to
// `go test`: the production package, and — when loaded with Tests:true —
// its internal test-augmented variant ("pkg [pkg.test]") and any black-box
// external test package ("pkg_test [pkg.test]"). Passing only the
// production variant silently drops any same-module import that appears
// solely in a _test.go file, including this project's own convention of a
// separate "_test" package for black-box tests (see engine_test.go,
// report_test.go): that package is a distinct *packages.Package with its
// own Imports map, invisible to a walk that only ever sees the production
// variant. A future caller wiring this in MUST pass all variants it loaded
// for the target directory, not just the one matching the production
// PkgPath — see [mergeVariants].
func resolveClosure(pkgs ...*packages.Package) (dirs map[string]bool, ok bool) {
	var moduleDir string

	for _, p := range pkgs {
		if p != nil && p.Module != nil && p.Module.Dir != "" {
			moduleDir = p.Module.Dir

			break
		}
	}

	if moduleDir == "" {
		return nil, false
	}

	if info, err := os.Stat(filepath.Join(moduleDir, "vendor")); err == nil && info.IsDir() {
		return nil, false
	}

	replaces, err := localReplaces(moduleDir)
	if err != nil || len(replaces) > 0 {
		return nil, false
	}

	dirs, ok = closureDirs(mergeVariants(pkgs))
	if !ok {
		return nil, false
	}

	// go.mod is copied unmodified into the narrower workspace (copyClosure
	// does no replace rewriting, unlike copyModule's rewriteReplaces): an
	// in-module replace directive is only safe to leave as-is if its target
	// is itself part of the closure just computed, since only then is it
	// guaranteed to exist at the same relative offset in the copy. The Go
	// toolchain resolves a module's replace directives while loading the
	// module graph regardless of whether the specific package under test
	// needs them, so a dangling one is not merely wasted — it is a build
	// failure for every mutant in the run, not just an inefficiency.
	inModule, err := inModuleReplaceTargets(moduleDir)
	if err != nil {
		return nil, false
	}

	for _, target := range inModule {
		if !dirs[target] {
			return nil, false
		}
	}

	return dirs, true
}

// mergeVariants combines every non-nil package in pkgs — assumed to be
// go/packages variants of the same target directory — into one synthetic
// root [closureDirs] can walk: their GoFiles/CompiledGoFiles pooled (so
// [hasEmbedDirective] sees a black-box test file's `//go:embed` directive
// too, not just the production file's), their Imports maps unioned (so a
// same-module package imported only by a _test.go file ends up in the
// closure), and their Errors concatenated (so an error on any variant marks
// the whole thing unsafe, same as [closureDirs] already does for a single
// package). Module is taken from the first variant that has one; PkgPath is
// only used as [closureDirs]' recursion-root dedupe key, so which variant's
// it ends up as does not matter.
func mergeVariants(pkgs []*packages.Package) *packages.Package {
	merged := &packages.Package{Imports: map[string]*packages.Package{}}

	for _, p := range pkgs {
		if p == nil {
			continue
		}

		if merged.Module == nil {
			merged.Module = p.Module
		}

		merged.PkgPath = p.PkgPath
		merged.Errors = append(merged.Errors, p.Errors...)
		merged.GoFiles = append(merged.GoFiles, p.GoFiles...)
		merged.CompiledGoFiles = append(merged.CompiledGoFiles, p.CompiledGoFiles...)

		for path, imp := range p.Imports {
			merged.Imports[path] = imp
		}
	}

	return merged
}

// copyClosure builds a workspace containing only dirs — pkg's forward
// dependency closure (see [resolveClosure]) — inside dst, rather than the
// whole module [copyModule] copies. It returns the copied module's root
// (holding go.mod/go.sum), the same shape copyModule returns, so the two
// are interchangeable from a caller's perspective.
//
// Every file directly inside each directory in dirs is copied — not just
// .go files — deliberately: the target package's own _test.go files are
// needed to run its tests, and copying a dependency's _test.go files too
// (never read, since only the target package's tests run) is harmless
// waste, not incorrect, and far simpler than distinguishing "this
// directory is the target" from "this directory is a dependency" at copy
// time. Subdirectories are never recursed into: a subdirectory of a
// package's directory is a *different* Go package, not part of this one,
// and if it's actually needed it is already its own entry in dirs.
func copyClosure(dst, moduleDir string, dirs map[string]bool) (string, error) {
	newModuleDir := filepath.Join(dst, "mod")

	for _, name := range []string{goModFile, "go.sum"} {
		src := filepath.Join(moduleDir, name)

		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue // go.sum is optional for a module with no external deps
			}

			return "", err
		}

		if err := copyFile(src, filepath.Join(newModuleDir, name), 0o600); err != nil {
			return "", err
		}
	}

	for dir := range dirs {
		rel, err := filepath.Rel(moduleDir, dir)
		if err != nil {
			return "", fmt.Errorf("mutate: locating %s within %s: %w", dir, moduleDir, err)
		}

		if err := copyDirFiles(dir, filepath.Join(newModuleDir, rel)); err != nil {
			return "", err
		}
	}

	return newModuleDir, nil
}

// copyDirFiles copies every regular file, and preserves every symlink (same
// as copyTree), directly inside src — not subdirectories — into dst.
func copyDirFiles(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}

			//nolint:gosec // copying the developer's own trusted module into a temp workspace, not a multi-tenant or attacker-controlled path
			if err := os.Symlink(link, dstPath); err != nil {
				return err
			}

			continue
		}

		if !entry.Type().IsRegular() {
			continue // sockets, devices, named pipes: nothing a build reads
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if err := copyFile(srcPath, dstPath, info.Mode().Perm()); err != nil {
			return err
		}
	}

	return nil
}

// localReplace is a replace directive whose right-hand side is a filesystem
// path rather than a module version.
type localReplace struct {
	oldPath    string
	oldVersion string
	// target is the replacement's absolute, cleaned directory.
	target string
}

// parseReplaces reads moduleDir's go.mod and returns every replace directive
// whose right-hand side is a filesystem path (per [isLocalPath]), each
// resolved to an absolute, cleaned target directory — regardless of whether
// that target lies inside or outside moduleDir. [localReplaces] and
// [inModuleReplaceTargets] each filter this to the half they care about, so
// the two agree on what counts as a "local" replace by construction.
func parseReplaces(moduleDir string) ([]localReplace, error) {
	path := filepath.Join(moduleDir, goModFile)

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

		out = append(out, localReplace{
			oldPath:    rep.Old.Path,
			oldVersion: rep.Old.Version,
			target:     filepath.Clean(target),
		})
	}

	return out, nil
}

// isInModule reports whether target — already absolute and cleaned, as
// [parseReplaces] produces it — lies at or under moduleDir.
func isInModule(moduleDir, target string) bool {
	return target == moduleDir || strings.HasPrefix(target, moduleDir+string(filepath.Separator))
}

// localReplaces returns [parseReplaces]' filesystem-path directives whose
// target lies outside moduleDir — the ones [copyModule]'s rewriteReplaces
// needs to repoint.
//
// Replacements pointing inside the module itself are excluded: under
// copyModule they are copied wholesale with the module and their relative
// paths still resolve, so rewriting them would only risk breaking something
// that already works. [resolveClosure] cannot make that same assumption —
// see [inModuleReplaceTargets].
func localReplaces(moduleDir string) ([]localReplace, error) {
	all, err := parseReplaces(moduleDir)
	if err != nil {
		return nil, err
	}

	var out []localReplace

	for _, rp := range all {
		if !isInModule(moduleDir, rp.target) {
			out = append(out, rp)
		}
	}

	return out, nil
}

// inModuleReplaceTargets returns the target directories of [parseReplaces]'
// filesystem-path directives that DO lie inside moduleDir — the half
// [localReplaces] excludes because [copyModule] copies them wholesale.
// [resolveClosure] copies only a package's dependency closure, not the whole
// module, so it cannot assume an in-module replace target was copied; it
// checks each of these against the closure it computed instead.
func inModuleReplaceTargets(moduleDir string) ([]string, error) {
	all, err := parseReplaces(moduleDir)
	if err != nil {
		return nil, err
	}

	var out []string

	for _, rp := range all {
		if isInModule(moduleDir, rp.target) {
			out = append(out, rp.target)
		}
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
	path := filepath.Join(moduleDir, goModFile)

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
