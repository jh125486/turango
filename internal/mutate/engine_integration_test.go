//go:build integration

// Integration tests: each one resolves and shells out to the real Go
// toolchain, compiling and testing at least one throwaway module copy. Slow
// relative to the rest of the suite (seconds to minutes each, real go test
// subprocesses), so they're gated behind the "integration" build tag rather
// than just a testing.Short() skip — `go test ./...` never even compiles
// them in; `make test-integration` (-tags=integration) does. See the
// Makefile.
//
// fixtureModule comes from engine_test.go: that file has no build tag, so it
// always compiles alongside this one, and the two share one package
// (mutate_test) — redeclaring the helper here would collide with it.
package mutate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/mutate"
)

// TestRunAgainstRealModule is the end-to-end check: a real multi-package module
// is mutated, every mutant is built and tested in its own workspace copy, and
// the run must produce both kills and survivors. Anything that breaks the
// workspace copy or the classifier collapses this into all-NotViable or
// all-Survived, which no amount of unit testing the pieces would catch.
func TestRunAgainstRealModule(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	// TestTimeout is deliberately left zero so the run exercises the derived
	// baseline path end to end, not just the arithmetic.
	result, err := mutate.Run(ctx, mutate.Options{
		Packages: []string{"./..."},
		Dir:      root,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	killed, survived, notViable := result.Counts()
	t.Logf("mutants=%d killed=%d survived=%d not-viable=%d", len(result.Mutants), killed, survived, notViable)

	for _, m := range result.Mutants {
		t.Logf("%s:%d %s: %s -> %s", filepath.Base(m.File), m.Line, m.Operator, m.Description, m.Status)
	}

	if len(result.Mutants) == 0 {
		t.Fatal("Run() produced no mutants")
	}

	if killed == 0 {
		t.Error("Run() killed no mutants: the workspace copy or the classifier is broken")
	}

	if survived == 0 {
		t.Error("Run() reported no survivors, but the fixture has deliberately untested branches")
	}

	score, ok := result.Score()
	if !ok {
		t.Fatal("Score() reported no viable mutants")
	}

	if score <= 0 || score >= 1 {
		t.Errorf("Score() = %v, want a mix of kills and survivors", score)
	}

	// Every result must be attributable: a report row with no file, operator
	// or ID is useless to a user.
	seen := make(map[string]mutate.MutantResult, len(result.Mutants))

	for _, m := range result.Mutants {
		if m.File == "" || m.Line == 0 || m.Operator == "" || m.Description == "" || m.ID == "" {
			t.Errorf("incomplete MutantResult: %+v", m)
		}

		if dup, ok := seen[m.ID]; ok {
			t.Errorf("duplicate mutant ID %q: %+v and %+v", m.ID, dup, m)
		}

		seen[m.ID] = m
	}
}

// TestRunIsDeterministicUnderParallelism is the concurrency contract: the same
// module mutated with one worker and with several must produce byte-identical
// reports. It is the test that fails if the collector loses or interleaves
// appends, and — under -race — if the workers touch anything they share.
//
// Package scope and an explicit timeout keep it to one `go test` per mutant with
// no baseline runs; the concurrency being tested is turango's, not the
// toolchain's.
func TestRunIsDeterministicUnderParallelism(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	base := mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       mutate.ScopePackage,
		TestTimeout: 2 * time.Minute,
	}

	sequential := base
	sequential.Parallel = 1

	parallel := base
	parallel.Parallel = 4

	seqResult, err := mutate.Run(ctx, sequential)
	if err != nil {
		t.Fatalf("Run(parallel=1) error = %v", err)
	}

	parResult, err := mutate.Run(ctx, parallel)
	if err != nil {
		t.Fatalf("Run(parallel=4) error = %v", err)
	}

	if len(seqResult.Mutants) == 0 {
		t.Fatal("Run() produced no mutants")
	}

	if len(seqResult.Mutants) != len(parResult.Mutants) {
		t.Fatalf("mutant count = %d with 4 workers, %d with 1", len(parResult.Mutants), len(seqResult.Mutants))
	}

	// Output is excluded from the comparison: it is the toolchain's text, and a
	// timing-dependent line in it would make this test flap for reasons that
	// have nothing to do with the worker pool.
	for i := range seqResult.Mutants {
		got, want := parResult.Mutants[i], seqResult.Mutants[i]

		if got.ID != want.ID || got.File != want.File || got.Line != want.Line ||
			got.Operator != want.Operator || got.Description != want.Description ||
			got.Status != want.Status {
			t.Errorf("mutant %d = %s %s:%d %s %q %v, want %s %s:%d %s %q %v",
				i, got.ID, got.File, got.Line, got.Operator, got.Description, got.Status,
				want.ID, want.File, want.Line, want.Operator, want.Description, want.Status)
		}
	}
}

// TestRunReplaysOneMutantByID covers Options.MutantID end to end: a full run
// discovers a real mutant's ID, and a second run scoped to just that ID
// reproduces exactly that one mutant and nothing else.
func TestRunReplaysOneMutantByID(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	base := mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       mutate.ScopePackage,
		TestTimeout: 2 * time.Minute,
	}

	full, err := mutate.Run(ctx, base)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(full.Mutants) == 0 {
		t.Fatal("Run() produced no mutants to pick a target ID from")
	}

	want := full.Mutants[len(full.Mutants)/2]

	replay := base
	replay.MutantID = want.ID

	got, err := mutate.Run(ctx, replay)
	if err != nil {
		t.Fatalf("Run() with MutantID=%q error = %v", want.ID, err)
	}

	if len(got.Mutants) != 1 {
		t.Fatalf("Run() with MutantID=%q produced %d mutants, want exactly 1", want.ID, len(got.Mutants))
	}

	if diff := got.Mutants[0]; diff.ID != want.ID || diff.File != want.File || diff.Line != want.Line ||
		diff.Operator != want.Operator || diff.Description != want.Description || diff.Status != want.Status {
		t.Errorf("replayed mutant = %+v, want %+v", diff, want)
	}

	// A syntactically valid ID nothing in this module actually produces must
	// come back empty, not error: the walk simply never finds a match.
	miss := base
	miss.MutantID = strings.Repeat("0", len(want.ID))

	empty, err := mutate.Run(ctx, miss)
	if err != nil {
		t.Fatalf("Run() with an unmatched MutantID error = %v", err)
	}

	if len(empty.Mutants) != 0 {
		t.Errorf("Run() with an unmatched MutantID produced %d mutants, want 0", len(empty.Mutants))
	}
}

// TestRunWithImpactScope exercises the coverage-directed path end to end: the
// per-test coverage map is built, mutants on covered lines are judged by the
// tests that cover them, and mutants on uncovered lines are reported as
// survivors without a test run at all.
func TestRunWithImpactScope(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	result, err := mutate.Run(ctx, mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       mutate.ScopeImpact,
		Parallel:    2,
		TestTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	killed, survived, _ := result.Counts()
	if killed == 0 {
		t.Error("impact scope killed nothing: the -run selection or the coverage map is wrong")
	}

	if survived == 0 {
		t.Error("impact scope reported no survivors, but the fixture has untested branches")
	}

	// Describe's body is never called by any test, so every mutant of it must
	// be a survivor decided by the coverage map rather than by a test run.
	var describeMutants int

	mathxGo := filepath.Join(root, "mathx", "mathx.go")
	descLine := describeLine(t, mathxGo)

	for _, m := range result.Mutants {
		// Matched by file as well as line: fixtureModule has more than one
		// file under mathx, each with its own independent line numbering, so
		// a line-only check would also sweep up limits.go's (covered)
		// mutants whenever their line number happens to land past
		// Describe's.
		if m.File == mathxGo && m.Line >= descLine {
			describeMutants++

			if m.Status != mutate.Survived {
				t.Errorf("mutant on uncovered line %d = %v, want %v", m.Line, m.Status, mutate.Survived)
			}

			if !strings.Contains(m.Output, "no test") {
				t.Errorf("mutant on uncovered line %d ran the tests anyway: %q", m.Line, m.Output)
			}
		}
	}

	if describeMutants == 0 {
		t.Error("no mutants were produced for the uncovered function")
	}
}

// describeLine reports the line Describe is declared on, so the assertions above
// do not hard-code a line number the fixture could drift away from.
func describeLine(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "func Describe(") {
			return i + 1
		}
	}

	t.Fatalf("no Describe function in %s", path)

	return 0
}

// TestRunConstSwapOperator is the engine-level check for gap 1
// (identifier/constswap): selecting the operator against a real module
// produces the expected swap, on the expected line, and the swap is
// compilable and behaviourally different enough that the fixture's own test
// kills it.
func TestRunConstSwapOperator(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := mutate.Run(ctx, mutate.Options{
		Packages:  []string{"./..."},
		Dir:       root,
		Operators: []string{"identifier/constswap"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// LowLimit is the fixture's only const *use* in scope for the operator:
	// HighLimit is declared but never referenced, and every other constant in
	// the module either does not exist or has no same-block sibling.
	if len(result.Mutants) != 1 {
		t.Fatalf("Run() produced %d mutants, want exactly 1: %+v", len(result.Mutants), result.Mutants)
	}

	got := result.Mutants[0]

	if got.Operator != "identifier/constswap" {
		t.Errorf("Operator = %q, want %q", got.Operator, "identifier/constswap")
	}

	if got.Description != "LowLimit -> HighLimit" {
		t.Errorf("Description = %q, want %q", got.Description, "LowLimit -> HighLimit")
	}

	if !strings.HasSuffix(got.File, filepath.Join("mathx", "limits.go")) {
		t.Errorf("File = %q, want it to end in %s", got.File, filepath.Join("mathx", "limits.go"))
	}

	if got.Status != mutate.Killed {
		t.Errorf("Status = %v, want %v: TestBounded should catch this swap", got.Status, mutate.Killed)
	}
}

// TestRunFailsOnBrokenBaseline is the end-to-end half of the same rule: a module
// whose tests already fail must be rejected before a single mutant runs.
func TestRunFailsOnBrokenBaseline(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	// Break the suite without breaking the build.
	broken := "package mathx\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) {\n\tt.Fatal(\"already red\")\n}\n"
	if err := os.WriteFile(filepath.Join(root, "mathx", "mathx_test.go"), []byte(broken), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := mutate.Run(t.Context(), mutate.Options{Packages: []string{"./..."}, Dir: root})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a suite that fails unmutated")
	}

	if !strings.Contains(err.Error(), "unmutated test suite") {
		t.Errorf("Run() error = %v, want it to name the unmutated suite", err)
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
// Duplicated from suppress_internal_test.go's identical fixture: that copy
// backs a whitebox test exercising the unexported mutateFile walk directly,
// this one backs TestRunSuppressesCompoundStatement below, which only calls
// the exported Run and belongs in this blackbox file per Run being declared
// in engine.go. Sharing one copy across packages isn't possible without
// exporting test-only machinery, so the fixture is kept in both places.
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

// TestRunSuppressesCompoundStatement is the end-to-end half of the suppression
// cascade check: a full Run over a real module whose only mutable code is
// inside a //nomutant loop must report no mutants, one suppression, and no
// score at all — there is nothing to score.
func TestRunSuppressesCompoundStatement(t *testing.T) {
	t.Parallel()

	root, path := cascadeModule(t, "// nomutant: hand-verified")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// An explicit timeout skips the three baseline suite runs: no mutant is
	// expected to run, so timing one would only slow the test down.
	result, err := mutate.Run(ctx, mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		TestTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(result.Mutants) != 0 {
		t.Errorf("Run() produced %d mutants, want none: every mutable node is suppressed", len(result.Mutants))
	}

	want := []mutate.SuppressionResult{{File: path, Line: suppressedLine, Reason: "hand-verified"}}
	if !reflect.DeepEqual(result.Suppressions, want) {
		t.Errorf("Suppressions = %+v, want %+v", result.Suppressions, want)
	}

	if _, ok := result.Score(); ok {
		t.Error("Score() reported a score for a run with no mutants")
	}
}

// tceDeadStoreDescription is the Description statement/remover gives the
// mutant that deletes the fixture's dead store below. Matched by
// Description, not Line: statement/remover's reported Line is the walk's
// outer node — the containing block — not the individual statement (a
// documented gap-3 limitation, see ROADMAP.md), so every removable
// statement in Sum's body shares one Line and only Description tells them
// apart.
const tceDeadStoreDescription = "remove statement: total = 999"

// tceModule writes a one-package module with a genuine dead store: total's
// first write is immediately overwritten before ever being read, so the
// statement/remover mutant deleting it is textbook compiler-equivalent — the
// same fixture shape ROADMAP.md gap 2's spike validated by hand. Kept
// separate from fixtureModule so this test's Operators/TCE options never
// have to account for any of that fixture's own mutants.
func tceModule(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/tce\n\ngo 1.26\n",
		filepath.Join(root, "deadstore.go"): `package deadstore

func Sum(vs []int) (total int) {
	total = 999
	total = 0
	for _, v := range vs {
		total += v
	}
	return
}
`,
		filepath.Join(root, "deadstore_test.go"): `package deadstore

import "testing"

func TestSum(t *testing.T) {
	if got := Sum([]int{1, 2, 3}); got != 6 {
		t.Fatalf("Sum() = %d", got)
	}
}
`,
	})

	return root
}

// TestRunWithTCEFiltersEquivalentMutant is the dual-mode check ROADMAP.md
// gap 2's Verification section calls for: with TCE enabled, the dead
// store's removal is filtered into Result.Equivalents and never reaches
// Result.Mutants at all; with TCE disabled (the default), the identical
// mutation shows up as an ordinary Survived MutantResult — proving the
// toggle actually changes behavior, not just that the new path exists.
func TestRunWithTCEFiltersEquivalentMutant(t *testing.T) {
	t.Parallel()

	root := tceModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	base := mutate.Options{
		Packages:  []string{"."},
		Dir:       root,
		Operators: []string{"statement/remover"},
		Scope:     mutate.ScopePackage,
	}

	withTCE := base
	withTCE.TCE = true

	got, err := mutate.Run(ctx, withTCE)
	if err != nil {
		t.Fatalf("Run() with TCE error = %v", err)
	}

	var foundEquivalent bool

	for _, eq := range got.Equivalents {
		if eq.Description == tceDeadStoreDescription {
			foundEquivalent = true
		}
	}

	if !foundEquivalent {
		t.Errorf("Run() with TCE: no Equivalents entry for %q, got %+v", tceDeadStoreDescription, got.Equivalents)
	}

	for _, m := range got.Mutants {
		if m.Description == tceDeadStoreDescription {
			t.Errorf("Run() with TCE: %q also appears as an ordinary MutantResult (%+v), want it filtered", tceDeadStoreDescription, m)
		}
	}

	without, err := mutate.Run(ctx, base)
	if err != nil {
		t.Fatalf("Run() without TCE error = %v", err)
	}

	if len(without.Equivalents) != 0 {
		t.Errorf("Run() without TCE produced %d Equivalents, want 0: TCE is off", len(without.Equivalents))
	}

	var foundSurvivor bool

	for _, m := range without.Mutants {
		if m.Description == tceDeadStoreDescription {
			foundSurvivor = true

			if m.Status != mutate.Survived {
				t.Errorf("Run() without TCE: mutant %q status = %v, want %v", tceDeadStoreDescription, m.Status, mutate.Survived)
			}
		}
	}

	if !foundSurvivor {
		t.Errorf("Run() without TCE: no MutantResult for %q, got %+v", tceDeadStoreDescription, without.Mutants)
	}
}

// TestRunWorkspaceWorktreeMatchesCopy is ROADMAP.md gap 6's end-to-end
// proof: the same fixture, mutated once with the default [mutate.WorkspaceCopy]
// and once with [mutate.WorkspaceWorktree], must classify every mutant
// identically. A git worktree is a different mechanism for building the
// same execution copy, not a different scope or classification rule, so
// any divergence here is a bug in the worktree path, not a legitimate
// difference in verdict.
func TestRunWorkspaceWorktreeMatchesCopy(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)
	gitCommitFixture(t, root)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	opts := mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       mutate.ScopePackage,
		TestTimeout: 30 * time.Second,
	}

	copyResult, err := mutate.Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run() with WorkspaceCopy error = %v", err)
	}

	opts.Workspace = mutate.WorkspaceWorktree

	worktreeResult, err := mutate.Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run() with WorkspaceWorktree error = %v", err)
	}

	if len(copyResult.Mutants) != len(worktreeResult.Mutants) {
		t.Fatalf("mutant count: copy=%d worktree=%d, want equal", len(copyResult.Mutants), len(worktreeResult.Mutants))
	}

	copyKilled, copySurvived, copyNotViable := copyResult.Counts()
	wtKilled, wtSurvived, wtNotViable := worktreeResult.Counts()

	if copyKilled != wtKilled || copySurvived != wtSurvived || copyNotViable != wtNotViable {
		t.Errorf("verdict counts differ: copy killed=%d survived=%d notViable=%d, worktree killed=%d survived=%d notViable=%d",
			copyKilled, copySurvived, copyNotViable, wtKilled, wtSurvived, wtNotViable)
	}
}

// TestEstimateMatchesRun is ROADMAP.md gap 11's required end-to-end proof:
// Estimate's walk-only count must exactly match a real Run's
// len(Result.Mutants) + len(Result.Equivalents) for the same [mutate.Options]
// — Equivalents count too, since an estimate deliberately ignores TCE (gap
// 11d) and reports every matching mutation as if it will run.
//
// fixtureModule is deliberately two packages (app, mathx), not one — this is
// exactly what exercises gap 11b's per-package-not-whole-module baseline
// design: a single-package fixture would not catch a bug where every
// package's timing was accidentally computed once, module-wide, instead of
// per package.
func TestEstimateMatchesRun(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	opts := mutate.Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       mutate.ScopePackage,
		TestTimeout: 30 * time.Second,
	}

	result, err := mutate.Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	estimate, err := mutate.Estimate(ctx, opts)
	if err != nil {
		t.Fatalf("Estimate() error = %v", err)
	}

	wantTotal := len(result.Mutants) + len(result.Equivalents)
	if estimate.Total != wantTotal {
		t.Errorf("Estimate().Total = %d, want %d (= len(Result.Mutants)+len(Result.Equivalents))", estimate.Total, wantTotal)
	}

	if len(estimate.Packages) < 2 {
		t.Fatalf("Estimate().Packages has %d entries, want at least 2 (fixtureModule has app and mathx)", len(estimate.Packages))
	}

	var summedMutants int

	for _, p := range estimate.Packages {
		summedMutants += p.Mutants

		if p.Mutants <= 0 {
			t.Errorf("PackageEstimate{Package: %q}.Mutants = %d, want > 0 (packages with zero mutants should not be listed at all)", p.Package, p.Mutants)
		}

		// Under ScopePackage each package's baseline must be its own
		// sample, not a shared whole-module one — gap 11b's own point. This
		// does not directly prove the two samples differ (nothing stops a
		// coincidental tie), but every package having its own positive
		// sample at all is the mechanical evidence that packageBaseline,
		// not a single module-wide value, ran once per package.
		if p.Baseline <= 0 {
			t.Errorf("PackageEstimate{Package: %q}.Baseline = %v, want > 0", p.Package, p.Baseline)
		}
	}

	if summedMutants != estimate.Total {
		t.Errorf("sum of PackageEstimate.Mutants = %d, want Total = %d", summedMutants, estimate.Total)
	}

	// opts.Parallel is left unset (zero) above, which Options.parallel()
	// clamps to 1 — the same zero-value convention TestOptionsParallel
	// (engine_internal_test.go) already pins directly.
	if estimate.Workers != 1 {
		t.Errorf("Estimate().Workers = %d, want 1 (opts.Parallel was left unset)", estimate.Workers)
	}

	if estimate.SerialEstimate <= 0 {
		t.Errorf("Estimate().SerialEstimate = %v, want > 0", estimate.SerialEstimate)
	}

	if estimate.ParallelEstimate <= 0 || estimate.ParallelEstimate > estimate.SerialEstimate {
		t.Errorf("Estimate().ParallelEstimate = %v, want in (0, %v]", estimate.ParallelEstimate, estimate.SerialEstimate)
	}
}

// gitCommitFixture turns root — an already-populated fixture module
// directory — into a real, committed git repository, so
// [mutate.WorkspaceWorktree] (which requires one, see runner.go's
// gitWorktreeClean) has something real to work against.
func gitCommitFixture(t *testing.T, root string) {
	t.Helper()

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "turango-test@example.com"},
		{"config", "user.name", "turango test"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "fixture"},
	} {
		//nolint:gosec // root and args are test-controlled, not external input.
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
