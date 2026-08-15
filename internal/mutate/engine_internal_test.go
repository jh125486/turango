// Whitebox: these tests exercise engine.go's unexported machinery directly —
// mutantID, the collector, mutateFile, Options.mutators/parallel,
// baselineTimeout/resolveTimeout, and walkForEstimate/estimateTally — none
// of which is reachable through the exported Run/Estimate/Options/Result
// surface alone. Blackbox coverage of Run's exported behaviour lives in
// engine_test.go; the whitebox integration test touching the loadTypedCalls
// counter lives in engine_integration_internal_test.go (behind the
// "integration" build tag).
package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/jh125486/turango/internal/mutator"
)

// fixtureModule writes a small but genuine two-package module — app imports
// mathx, both have real, passing tests — and returns its root.
//
// It is generated rather than committed as testdata because a nested module in
// the repository would need its own go.mod, and because the point of the
// fixture is that the engine copies and builds a *real* module tree, not that
// the sources are interesting.
//
// Duplicated from engine_test.go's identical helper: that copy backs
// blackbox tests calling only the exported Run; this one backs the two
// whitebox tests below that need unexported internals, and a blackbox file
// cannot be imported from a whitebox one (or vice versa) to share it.
func fixtureModule(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		"go.mod": "module example.com/fixture\n\ngo 1.23\n",

		"mathx/mathx.go": `package mathx

// Clamp constrains v to [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Describe labels n. The negative branch is deliberately untested, so mutating
// it produces survivors.
func Describe(n int) string {
	if n < 0 {
		return "negative"
	}
	return "other"
}
`,
		"mathx/mathx_test.go": `package mathx

import "testing"

func TestClamp(t *testing.T) {
	if got := Clamp(5, 0, 10); got != 5 {
		t.Fatalf("Clamp(5,0,10) = %d", got)
	}
	if got := Clamp(-1, 0, 10); got != 0 {
		t.Fatalf("Clamp(-1,0,10) = %d", got)
	}
	if got := Clamp(11, 0, 10); got != 10 {
		t.Fatalf("Clamp(11,0,10) = %d", got)
	}
}
`,
		"app/app.go": `package app

import "example.com/fixture/mathx"

// Score clamps raw into a percentage.
func Score(raw int) int {
	return mathx.Clamp(raw, 0, 100)
}
`,
		"app/app_test.go": `package app

import "testing"

func TestScore(t *testing.T) {
	if got := Score(120); got != 100 {
		t.Fatalf("Score(120) = %d", got)
	}
}
`,
		"mathx/limits.go": `package mathx

// LowLimit and HighLimit are two package-level constants of the same type,
// declared in the same const block, deliberately close enough in value that
// swapping one for the other is both compilable and behaviourally different
// — the fixture identifier/constswap needs.
const (
	LowLimit  = 3
	HighLimit = 30
)

// Bounded reports whether n is at least LowLimit. Swapping in HighLimit
// changes Bounded's result for every n in [LowLimit, HighLimit), which
// mathx_test.go's TestBounded exercises.
func Bounded(n int) bool {
	return n >= LowLimit
}
`,
		"mathx/limits_test.go": `package mathx

import "testing"

func TestBounded(t *testing.T) {
	if !Bounded(LowLimit) {
		t.Fatalf("Bounded(LowLimit) = false, want true")
	}
	if Bounded(LowLimit - 1) {
		t.Fatalf("Bounded(LowLimit-1) = true, want false")
	}
}
`,
	}

	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	return root
}

// TestMutateFileStopsWhenCancelled covers the worker's cancellation awareness,
// which SIGINT handling hangs off: an already-cancelled context must stop the
// walk before a single mutant is executed.
func TestMutateFileStopsWhenCancelled(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)
	path := filepath.Join(root, "mathx", "mathx.go")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	sink := newCollector()

	// A deliberately unusable go binary: reaching the runner at all would fail
	// loudly rather than silently passing this test.
	run := &runner{goBin: "/nonexistent/go", testTimeout: time.Second}

	job := &fileJob{moduleDir: root, path: path, mutators: mutator.All()}

	err := mutateFile(ctx, run, job, sink, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mutateFile() error = %v, want %v", err, context.Canceled)
	}

	result := sink.close()
	if len(result.Mutants) != 0 {
		t.Errorf("mutateFile() ran %d mutants after cancellation", len(result.Mutants))
	}
}

// TestMutantID pins mutantID's two load-bearing properties: identical inputs
// always hash to the same ID (needed for cross-run stability), and changing
// any single coordinate of the tuple changes the ID (needed so two distinct
// mutations, e.g. expression/remove's two operand removals on one node, or
// colliding binary-chain siblings disambiguated via rank, never collide).
func TestMutantID(t *testing.T) {
	t.Parallel()

	base := mutantID("pkg/file.go", 10, 4, "operator/binary", 0, 0)

	if got := mutantID("pkg/file.go", 10, 4, "operator/binary", 0, 0); got != base {
		t.Errorf("mutantID() = %q, %q on identical inputs, want equal", base, got)
	}

	if n := len(base); n != 12 {
		t.Errorf("len(mutantID()) = %d, want 12", n)
	}

	tests := map[string]string{
		"path":     mutantID("pkg/other.go", 10, 4, "operator/binary", 0, 0),
		"line":     mutantID("pkg/file.go", 11, 4, "operator/binary", 0, 0),
		"column":   mutantID("pkg/file.go", 10, 5, "operator/binary", 0, 0),
		"operator": mutantID("pkg/file.go", 10, 4, "operator/boundary", 0, 0),
		"index":    mutantID("pkg/file.go", 10, 4, "operator/binary", 1, 0),
		"rank":     mutantID("pkg/file.go", 10, 4, "operator/binary", 0, 1),
	}

	for name, got := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got == base {
				t.Errorf("mutantID() unchanged when only %s varied: both = %q", name, got)
			}
		})
	}
}

// TestMutantIDRankZeroMatchesPreGap13Hash locks in the binary-chain
// collision fix's backward-compatibility promise: rank 0 — every mutation
// that never collided with a same-position sibling, i.e. the overwhelming
// majority of real code, including every *ast.BinaryExpr that is not itself
// nested inside another one starting at the same position — must hash to
// exactly the same bytes mutantID produced before rank existed. This is asserted
// against a hardcoded pre-gap-13 SHA-256 (truncated to 12 hex characters,
// same as mutantID's own output), not merely against mutantID's current
// rank==0 branch, so a refactor that accidentally starts hashing rank
// unconditionally would fail this test even though TestMutantID's "rank"
// case above would not catch it (that case only proves rank changes the
// ID when non-zero, not that rank zero leaves it alone).
func TestMutantIDRankZeroMatchesPreGap13Hash(t *testing.T) {
	t.Parallel()

	const wantPreGap13ID = "cf9966da6fdf" // sha256("pkg/file.go\x0010\x004\x00operator/binary\x000")[:12], hex

	if got := mutantID("pkg/file.go", 10, 4, "operator/binary", 0, 0); got != wantPreGap13ID {
		t.Errorf("mutantID(rank=0) = %q, want %q (the pre-gap-13 hash for this tuple)", got, wantPreGap13ID)
	}
}

// TestOptionsMutators covers operator selection, including the rule that an
// unknown name fails the run rather than quietly shrinking the mutant set.
func TestOptionsMutators(t *testing.T) {
	t.Parallel()

	all, err := Options{}.mutators()
	if err != nil {
		t.Fatalf("mutators() error = %v", err)
	}

	if len(all) != len(mutator.List()) {
		t.Errorf("mutators() returned %d operators, want every registered one (%d)", len(all), len(mutator.List()))
	}

	some, err := Options{Operators: []string{"control/if"}}.mutators()
	if err != nil {
		t.Fatalf("mutators() error = %v", err)
	}

	if len(some) != 1 || some[0].Name() != "control/if" {
		t.Errorf("mutators() = %v, want just control/if", some)
	}

	_, err = Options{Operators: []string{"control/if", "no/such/operator"}}.mutators()
	if err == nil {
		t.Fatal("mutators() error = nil, want an error naming the unknown operator")
	}

	if !strings.Contains(err.Error(), "no/such/operator") {
		t.Errorf("mutators() error = %v, want it to name the unknown operator", err)
	}
}

func TestOptionsParallel(t *testing.T) {
	t.Parallel()

	tests := map[int]int{-1: 1, 0: 1, 1: 1, 8: 8}

	for in, want := range tests {
		if got := (Options{Parallel: in}).parallel(); got != want {
			t.Errorf("Options{Parallel: %d}.parallel() = %d, want %d", in, got, want)
		}
	}
}

// TestCollectorSorts pins the report order the doc comment promises, which is
// now restored by sorting rather than produced by the walk.
func TestCollectorSorts(t *testing.T) {
	t.Parallel()

	sink := newCollector()

	var wg sync.WaitGroup

	mutants := []MutantResult{
		{File: "b.go", Line: 1, Operator: "control/if", Description: "x"},
		{File: "a.go", Line: 9, Operator: "control/if", Description: "x"},
		{File: "a.go", Line: 2, Operator: "operator/binary", Description: "b"},
		{File: "a.go", Line: 2, Operator: "control/if", Description: "a"},
	}

	for _, m := range mutants {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sink.mutant(m)
			sink.suppression(SuppressionResult{File: m.File, Line: m.Line})
			sink.equivalent(EquivalentResult{File: m.File, Line: m.Line, Operator: m.Operator})
		}()
	}

	wg.Wait()

	got := sink.close()

	if len(got.Mutants) != len(mutants) || len(got.Suppressions) != len(mutants) || len(got.Equivalents) != len(mutants) {
		t.Fatalf("collector kept %d mutants, %d suppressions and %d equivalents, want %d of each",
			len(got.Mutants), len(got.Suppressions), len(got.Equivalents), len(mutants))
	}

	want := []string{"a.go:2:control/if", "a.go:2:operator/binary", "a.go:9:control/if", "b.go:1:control/if"}

	for i, m := range got.Mutants {
		if key := fmt.Sprintf("%s:%d:%s", m.File, m.Line, m.Operator); key != want[i] {
			t.Errorf("mutant %d = %s, want %s", i, key, want[i])
		}
	}
}

// TestBaselineTimeout covers the derivation itself: the mean of the timed runs,
// scaled by the CPU count, floored at MinBaselineTimeout.
func TestBaselineTimeout(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		durations []time.Duration
		runs      int
		cpus      int
		want      time.Duration
	}{
		"mean times cpu count": {
			durations: []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second},
			runs:      3,
			cpus:      4,
			// mean 60s x 4 CPUs
			want: 240 * time.Second,
		},
		"single cpu still scales by the mean": {
			durations: []time.Duration{20 * time.Second, 20 * time.Second, 50 * time.Second},
			runs:      3,
			cpus:      1,
			want:      30 * time.Second,
		},
		"a fast suite is floored": {
			durations: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			runs:      3,
			cpus:      2,
			want:      MinBaselineTimeout,
		},
		"exactly at the floor": {
			durations: []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second},
			runs:      3,
			cpus:      2,
			want:      MinBaselineTimeout,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls int

			timer := func(context.Context) (time.Duration, error) {
				d := tt.durations[calls]
				calls++

				return d, nil
			}

			got, err := baselineTimeout(t.Context(), tt.runs, tt.cpus, timer)
			if err != nil {
				t.Fatalf("baselineTimeout() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("baselineTimeout() = %v, want %v", got, tt.want)
			}

			if calls != tt.runs {
				t.Errorf("baselineTimeout() timed the suite %d times, want %d", calls, tt.runs)
			}
		})
	}
}

// TestBaselineTimeoutFailsOnBrokenSuite pins the rule that a suite failing on
// unmutated code aborts the run: every mutant would otherwise be "killed" by a
// failure that was already there.
func TestBaselineTimeoutFailsOnBrokenSuite(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("suite is red")

	var calls int

	timer := func(context.Context) (time.Duration, error) {
		calls++

		return 0, wantErr
	}

	got, err := baselineTimeout(t.Context(), baselineRuns, 4, timer)
	if !errors.Is(err, wantErr) {
		t.Fatalf("baselineTimeout() error = %v, want it to wrap %v", err, wantErr)
	}

	if got != 0 {
		t.Errorf("baselineTimeout() = %v, want 0 on a failed baseline run", got)
	}

	if !strings.Contains(err.Error(), "baseline run 1 of 3") {
		t.Errorf("baselineTimeout() error = %v, want it to say which run of how many failed", err)
	}

	if calls != 1 {
		t.Errorf("baselineTimeout() kept going after a failure: %d calls", calls)
	}
}

// TestResolveTimeout covers the short circuit: an explicit timeout must be used
// verbatim, and must not cost three suite runs to discover.
func TestResolveTimeout(t *testing.T) {
	t.Parallel()

	t.Run("explicit timeout skips the baseline", func(t *testing.T) {
		t.Parallel()

		var calls int

		timer := func(context.Context) (time.Duration, error) {
			calls++

			return time.Second, nil
		}

		got, err := resolveTimeout(t.Context(), Options{TestTimeout: 7 * time.Second}, timer)
		if err != nil {
			t.Fatalf("resolveTimeout() error = %v", err)
		}

		if got != 7*time.Second {
			t.Errorf("resolveTimeout() = %v, want %v", got, 7*time.Second)
		}

		if calls != 0 {
			t.Errorf("resolveTimeout() ran the baseline %d times for an explicit timeout", calls)
		}
	})

	t.Run("zero timeout derives a baseline", func(t *testing.T) {
		t.Parallel()

		var calls int

		timer := func(context.Context) (time.Duration, error) {
			calls++

			return time.Minute, nil
		}

		got, err := resolveTimeout(t.Context(), Options{}, timer)
		if err != nil {
			t.Fatalf("resolveTimeout() error = %v", err)
		}

		if calls != baselineRuns {
			t.Errorf("resolveTimeout() timed the suite %d times, want %d", calls, baselineRuns)
		}

		if want := time.Minute * time.Duration(runtime.NumCPU()); got != want {
			t.Errorf("resolveTimeout() = %v, want %v", got, want)
		}
	})
}

// TestWalkForEstimateSpawnsNoSubprocess is the concrete, checkable form of
// the claim that Estimate's counting phase costs "an AST walk with no go
// test subprocess anywhere": it asserts execCalls (see
// runner.go) never increments while walkForEstimate runs, the same
// call-counter technique TestRunWithoutConstSwapNeverLoadsTypes
// (engine_integration_internal_test.go) uses to prove loadTyped is skipped
// when unneeded.
//
// The fixture is corpus/op-operator-binary, whose golden.json already pins
// its expected mutant count (1) as part of this project's own regression
// harness (internal/corpus's TestCorpus) — read directly from the golden
// file here via goldenMutantCount, rather than hardcoded a second time, so
// this test cannot silently drift from the file it is supposed to be
// cross-checking against.
//
// goBin is a deliberately nonexistent path, the same defensive pattern
// every other whitebox test in this package uses (see e.g.
// TestRunSkipsNoOpMutation in runner_internal_test.go): walkForEstimate
// should never reach a point where it would matter, and reaching the
// toolchain at all would fail loudly rather than silently passing this
// test.
//
// It deliberately does not call t.Parallel(): execCalls is a process-wide
// counter shared by every test in this compiled binary, and this is the one
// test that specifically needs its own delta to be exactly zero rather than
// merely whatever it happened to be — running non-parallel, and therefore
// to completion before any t.Parallel() sibling resumes past its own
// Parallel() call, is what makes that assertion reliable rather than a race
// (see TestRunWithoutConstSwapNeverLoadsTypes's identical reasoning).
func TestWalkForEstimateSpawnsNoSubprocess(t *testing.T) {
	root := repoModuleRoot(t)
	moduleDir := filepath.Join(root, "corpus", "op-operator-binary", "module")
	golden := filepath.Join(root, "corpus", "op-operator-binary", "golden.json")

	want := goldenMutantCount(t, golden)

	before := execCalls.Load()

	counts, err := walkForEstimate(t.Context(), "/nonexistent/go", Options{
		Packages: []string{"./..."},
		Dir:      moduleDir,
	})
	if err != nil {
		t.Fatalf("walkForEstimate() error = %v", err)
	}

	if counts.total != want {
		t.Errorf("walkForEstimate() total = %d, want %d (per %s)", counts.total, want, golden)
	}

	if after := execCalls.Load(); after != before {
		t.Errorf("walkForEstimate() spawned %d go test/go build subprocess(es), want 0", after-before)
	}
}

// goldenMutantCount reads a corpus golden.json's pinned expect.mutants
// field directly, so a test asserting against it cannot silently drift from
// the file it is meant to be cross-checking. Only the one field this
// package's tests need is decoded — internal/corpus.Entry's full schema
// belongs to that package, not duplicated here for one int.
func goldenMutantCount(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}

	var golden struct {
		Expect struct {
			Mutants int `json:"mutants"`
		} `json:"expect"`
	}

	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}

	return golden.Expect.Mutants
}

// repoModuleRoot resolves the repository root from the test binary's
// working directory (internal/mutate, under `go test`'s default behaviour)
// by walking upward to the directory holding go.mod — the same approach
// internal/corpus/corpus_test.go's repoRoot and mutate_bench_test.go's
// benchRepoRoot both take, duplicated here (a two-line helper) rather than
// shared, for the same reason benchRepoRoot's own doc comment gives: each
// caller needs a different testing type (*testing.T here), and promoting
// this to shared exported API for a handful of callers isn't worth the
// churn.
func repoModuleRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoModuleRoot: no go.mod found above %s", wd)
		}

		dir = parent
	}
}

// --- Coverage gap-closing tests below (engine.go). Helpers are prefixed
// "engine" to avoid colliding with same-named helpers other concurrently
// edited _test.go files in this package might add. ---

// Documented coverage gaps left in engine.go, matching the reasoning style
// internal/goproxy/passthrough_internal_test.go's own Forward exception
// uses: a real seam would need either unsafe mocking of the Go toolchain, a
// destructive process-wide filesystem change, or a genuine data race, none
// of which is worth taking on for one branch each.
//
//   - Run's and Estimate's own goproxy.Resolve() error branches (engine.go's
//     Run around "goBin, err := goproxy.Resolve()" and Estimate's identical
//     first line): goproxy.Resolve() always falls back to
//     runtime.GOROOT(), fixed at process start and essentially guaranteed to
//     hold a real, executable "go" in any environment that could build and
//     run this test binary at all. Forcing it to fail would need deleting or
//     renaming the real GOROOT's bin/go on disk — a destructive,
//     process-wide change unacceptable for a unit test (see
//     passthrough_internal_test.go's own Forward exception for the identical
//     reasoning against the same function).
//   - Run's and walkForEstimate's loadTyped() error branches: load() and
//     loadTyped() are called with the same opts.Dir/opts.patterns() a few
//     lines apart, so any Dir/pattern failure hits load()'s own earlier
//     error return first (already covered by TestRunRejectsInvalidOptions
//     et al.). The only way to make loadTyped() alone fail is a difference
//     specific to its extra Mode bits (NeedSyntax/NeedTypes/NeedDeps), which
//     the go/packages driver surfaces as a soft per-package IllTyped error
//     (already fail-soft handled in planPackage), not the hard top-level
//     Load() error this branch guards — there is no dependency-injection
//     seam over packages.Load in this codebase to fake the latter.
//   - Run's plan() error branch and walkForEstimate's mirror: reaching it
//     requires planPackage's own planPrecompute() call to fail (see
//     TestPlanPackage's "planPrecompute error propagates" case below, which
//     covers that logic directly), but every real trigger is either a
//     context cancellation racing load()'s real `go list` subprocess call
//     (whose ctx.Err()/Done() polling cadence is an x/tools implementation
//     detail, not a documented contract to build a deterministic test
//     against) or a filesystem race after a real module was already loaded
//     successfully. walkForEstimate's own mirror branch is additionally
//     unreachable *by construction*: planPackage skips planPrecompute
//     entirely when p.estimateOnly is true (see planPackage's own
//     "if !p.estimateOnly" gate), so plan() can never return a non-nil error
//     when called with estimateOnly:true — the walkForEstimate call site
//     this specific "if err != nil" guards is dead code today. Flagged here
//     per this task's instructions rather than silently left or deleted;
//     removing it is a production-code change outside a test-only task's
//     scope.
//   - load()'s "len(pkgs) == 0" branch: empirically, every packages.Load
//     invocation against the standard `go list`-backed driver — a
//     nonexistent Dir, an unmatched pattern, an empty pattern, a directory
//     with no Go files — returns either a non-nil top-level error or at
//     least one synthesized package carrying an Errors entry, never a bare
//     empty, error-free slice. This appears to guard a non-standard
//     packages.Driver implementation (see packages.Config's Driver field,
//     selectable via GOPACKAGESDRIVER) that this project does not use and
//     has no seam to fake.
//   - planScope's "ctx cancelled specifically between buildImpact() failing
//     and planScope's own follow-up ctx.Err() check" branch: the other two
//     outcomes of that call (ctx already cancelled beforehand; buildImpact
//     fails with ctx never cancelled) are both covered directly by
//     TestPlanScope below. This third one requires ctx to become cancelled
//     during buildImpact's real subprocess call specifically — a genuine
//     race with no deterministic trigger short of synchronizing against a
//     purpose-built fake toolchain binary, which is exactly the kind of
//     unsafe/brittle mocking this project's own documented-exception
//     precedent (see above) argues against taking on for one branch.
//   - mutateFile's printer.Fprint error branch: printer.Fprint's only
//     documented failure mode is the underlying io.Writer returning an
//     error, and the target here is a bytes.Buffer, whose Write never
//     errors. Reaching this branch would need a deliberately malformed
//     *ast.File engineered to make go/printer itself misbehave in a way
//     that returns an error rather than panicking — not a realistic input
//     this engine ever produces (every AST it walks came from parser.ParseFile
//     or packages.Load's own type-checked Syntax) and not worth fabricating
//     just to touch this line.

// engineCtxErrAfterFirst is a context.Context whose Err() returns nil on its
// first call and context.Canceled on every call after — used to
// deterministically trigger a cancellation check that sits *after* at least
// one other ctx.Err() call has already had to return nil, without racing a
// real goroutine-based cancel against the code under test.
type engineCtxErrAfterFirst struct {
	context.Context
	calls atomic.Int32
}

func (c *engineCtxErrAfterFirst) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}

	return context.Canceled
}

// TestOptionsPatterns covers patterns()'s empty-means-"." convention, the
// same zero-value-means-everything shape mutators()/parallel() already have
// their own dedicated tests for in this file.
func TestOptionsPatterns(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		packages []string
		want     []string
	}{
		"empty defaults to dot": {
			packages: nil,
			want:     []string{"."},
		},
		"nonempty kept verbatim": {
			packages: []string{"./...", "./cmd/..."},
			want:     []string{"./...", "./cmd/..."},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := (Options{Packages: tt.packages}).patterns()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("patterns() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBaselineTimeoutInvalidRunsOrCPUs covers the defensive floor: a
// non-positive runs or cpus count returns MinBaselineTimeout immediately,
// without ever calling the suite timer.
func TestBaselineTimeoutInvalidRunsOrCPUs(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ runs, cpus int }{
		"zero runs":     {runs: 0, cpus: 4},
		"negative runs": {runs: -1, cpus: 4},
		"zero cpus":     {runs: 3, cpus: 0},
		"negative cpus": {runs: 3, cpus: -1},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls int

			timer := func(context.Context) (time.Duration, error) {
				calls++

				return time.Minute, nil
			}

			got, err := baselineTimeout(t.Context(), tt.runs, tt.cpus, timer)
			if err != nil {
				t.Fatalf("baselineTimeout() error = %v", err)
			}

			if got != MinBaselineTimeout {
				t.Errorf("baselineTimeout() = %v, want %v (the floor)", got, MinBaselineTimeout)
			}

			if calls != 0 {
				t.Errorf("baselineTimeout() timed the suite %d time(s) despite runs=%d cpus=%d, want 0", calls, tt.runs, tt.cpus)
			}
		})
	}
}

// TestSortResultEquivalentsOrdering is [TestCollectorSorts]'s counterpart
// for Result.Equivalents: the same file/line/operator/description tie-break
// chain, but exercised directly against sortResult since collector.consume
// only ever calls it after every channel is closed.
func TestSortResultEquivalentsOrdering(t *testing.T) {
	t.Parallel()

	result := &Result{
		Equivalents: []EquivalentResult{
			{File: "b.go", Line: 1, Operator: "control/if", Description: "x"},
			{File: "a.go", Line: 9, Operator: "control/if", Description: "x"},
			{File: "a.go", Line: 2, Operator: "operator/binary", Description: "b"},
			{File: "a.go", Line: 2, Operator: "control/if", Description: "z"},
			{File: "a.go", Line: 2, Operator: "control/if", Description: "a"},
		},
	}

	sortResult(result)

	want := []string{
		"a.go:2:control/if:a",
		"a.go:2:control/if:z",
		"a.go:2:operator/binary:b",
		"a.go:9:control/if:x",
		"b.go:1:control/if:x",
	}

	for i, e := range result.Equivalents {
		if key := fmt.Sprintf("%s:%d:%s:%s", e.File, e.Line, e.Operator, e.Description); key != want[i] {
			t.Errorf("equivalent %d = %s, want %s", i, key, want[i])
		}
	}
}

// TestPlanClosure covers every way planClosure declines before ever
// producing a real closure: [ScopeFull] (a forward closure is provably
// wrong there), no closurePkgs loaded this run, no variant found for dir,
// and resolveClosure itself declining (here, a variant with no Module info,
// the cheapest way to make resolveClosure's own moduleDir=="" check fire).
func TestPlanClosure(t *testing.T) {
	t.Parallel()

	const dir = "/some/pkg/dir"

	// resolvableFixture builds a closurePkgs entry that would genuinely
	// resolve to a non-nil closure — used by "ScopeFull always declines"
	// below so that guard is falsifiable by a mutant that skips it, not
	// merely masked by resolveClosure declining anyway for an empty
	// fixture the way the other decline subtests' fixtures do.
	resolvableFixture := func(t *testing.T) (targetDir string, closurePkgs map[string][]*packages.Package) {
		t.Helper()

		moduleDir := t.TempDir()
		targetDir = filepath.Join(moduleDir, "target")

		writeFiles(t, map[string]string{
			filepath.Join(moduleDir, "go.mod"):    "module example.com/mod\n\ngo 1.23\n",
			filepath.Join(targetDir, "target.go"): "package target\n",
		})

		pkg := &packages.Package{
			PkgPath: "example.com/mod/target",
			Module:  &packages.Module{Dir: moduleDir},
			GoFiles: []string{filepath.Join(targetDir, "target.go")},
		}

		return targetDir, map[string][]*packages.Package{targetDir: {pkg}}
	}

	t.Run("ScopeFull always declines", func(t *testing.T) {
		t.Parallel()

		targetDir, closurePkgs := resolvableFixture(t)

		if got := planClosure(ScopeFull, closurePkgs, targetDir); got != nil {
			t.Errorf("planClosure() = %v, want nil under ScopeFull even for an otherwise-resolvable closure", got)
		}
	})

	t.Run("nil closurePkgs declines", func(t *testing.T) {
		t.Parallel()

		if got := planClosure(ScopePackage, nil, dir); got != nil {
			t.Errorf("planClosure() = %v, want nil when closurePkgs is nil", got)
		}
	})

	t.Run("dir not resolved this run declines", func(t *testing.T) {
		t.Parallel()

		closurePkgs := map[string][]*packages.Package{"/other/dir": {{}}}
		if got := planClosure(ScopePackage, closurePkgs, dir); got != nil {
			t.Errorf("planClosure() = %v, want nil when dir has no entry", got)
		}
	})

	t.Run("resolveClosure declines", func(t *testing.T) {
		t.Parallel()

		closurePkgs := map[string][]*packages.Package{dir: {{}}}
		if got := planClosure(ScopePackage, closurePkgs, dir); got != nil {
			t.Errorf("planClosure() = %v, want nil when resolveClosure declines", got)
		}
	})

	// The one success path: every decline check above must actually be
	// passed through, not just short-circuited, for a clean module to
	// resolve to its real closure.
	t.Run("resolves a real closure", func(t *testing.T) {
		t.Parallel()

		targetDir, closurePkgs := resolvableFixture(t)

		got := planClosure(ScopePackage, closurePkgs, targetDir)

		want := map[string]bool{targetDir: true}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("planClosure() = %v, want %v", got, want)
		}
	})
}

// TestPlanScope covers the non-impact no-op short circuit, the
// already-cancelled-context short circuit ahead of buildImpact, and
// buildImpact failing (ctx never cancelled) demoting to [ScopePackage]
// rather than failing. See the documented-gaps comment above this block for
// the one remaining planScope branch left uncovered.
func TestPlanScope(t *testing.T) {
	t.Parallel()

	t.Run("non-impact scope is a no-op", func(t *testing.T) {
		t.Parallel()

		scope, cover, err := planScope(t.Context(), "/nonexistent/go", ScopePackage, nil, nil)
		if err != nil || scope != ScopePackage || cover != nil {
			t.Errorf("planScope() = (%v, %v, %v), want (%v, nil, nil)", scope, cover, err, ScopePackage)
		}
	})

	t.Run("cancelled context before buildImpact", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, _, err := planScope(ctx, "/nonexistent/go", ScopeImpact, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planScope() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("buildImpact failure demotes to package scope", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		pkg := &packages.Package{
			Module:  &packages.Module{Dir: root},
			GoFiles: []string{filepath.Join(root, "f.go")},
		}

		scope, cover, err := planScope(t.Context(), "/nonexistent/go", ScopeImpact, pkg, pkg.GoFiles)
		if err != nil {
			t.Fatalf("planScope() error = %v", err)
		}

		if scope != ScopePackage || cover != nil {
			t.Errorf("planScope() = (%v, %v), want (%v, nil): a failed coverage build must fail soft", scope, cover, ScopePackage)
		}
	})
}

// TestPlanTCEBaseline covers the TCE-disabled no-op, the
// already-cancelled-context short circuit, and packagePattern failing
// (mismatched absolute/relative moduleDir and firstFile) failing soft
// rather than propagating.
func TestPlanTCEBaseline(t *testing.T) {
	t.Parallel()

	t.Run("TCE disabled is a no-op", func(t *testing.T) {
		t.Parallel()

		got, err := planTCEBaseline(t.Context(), "/nonexistent/go", Options{}, "/mod", "/mod/f.go")
		if err != nil || got != nil {
			t.Errorf("planTCEBaseline() = (%v, %v), want (nil, nil) when TCE is off", got, err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := planTCEBaseline(ctx, "/nonexistent/go", Options{TCE: true}, "/mod", "/mod/f.go")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planTCEBaseline() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("packagePattern failure fails soft", func(t *testing.T) {
		t.Parallel()

		got, err := planTCEBaseline(t.Context(), "/nonexistent/go", Options{TCE: true}, "relative/moduledir", "/absolute/f.go")
		if err != nil || got != nil {
			t.Errorf("planTCEBaseline() = (%v, %v), want (nil, nil): a packagePattern failure must fail soft", got, err)
		}
	})
}

// TestPlanCacheFingerprint covers the CacheDir-unset no-op, the
// already-cancelled-context short circuit, and cacheFingerprint itself
// failing (a moduleDir that cannot be walked) propagating as a real error —
// unlike planScope/planTCEBaseline's optimisation failures, this one is not
// fail-soft (see planCacheFingerprint's own doc comment for why).
func TestPlanCacheFingerprint(t *testing.T) {
	t.Parallel()

	t.Run("CacheDir unset is a no-op", func(t *testing.T) {
		t.Parallel()

		got, err := planCacheFingerprint(t.Context(), Options{}, "/mod", nil, map[string]string{})
		if err != nil || got != "" {
			t.Errorf("planCacheFingerprint() = (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := planCacheFingerprint(ctx, Options{CacheDir: "cache"}, "/mod", nil, map[string]string{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planCacheFingerprint() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("fingerprint failure propagates", func(t *testing.T) {
		t.Parallel()

		_, err := planCacheFingerprint(t.Context(), Options{CacheDir: "cache"}, "/nonexistent/module/dir/xyz", nil, map[string]string{})
		if err == nil {
			t.Error("planCacheFingerprint() error = nil, want an error for a module directory that cannot be walked")
		}
	})
}

// TestPlanPrecomputePropagatesErrors isolates each of planPrecompute's three
// error-propagation branches by toggling exactly one of Scope/TCE/CacheDir
// against an already-cancelled context, so only the corresponding
// sub-precompute's own ctx.Err() check is ever reached.
func TestPlanPrecomputePropagatesErrors(t *testing.T) {
	t.Parallel()

	t.Run("planScope error", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		p := planner{goBin: "/nonexistent/go", opts: Options{Scope: ScopeImpact}}

		_, err := planPrecompute(ctx, p, &packages.Package{}, []string{"x.go"}, map[string]string{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planPrecompute() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("planTCEBaseline error", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		p := planner{goBin: "/nonexistent/go", opts: Options{TCE: true}}
		pkg := &packages.Package{Module: &packages.Module{Dir: "/mod"}}

		_, err := planPrecompute(ctx, p, pkg, []string{"/mod/x.go"}, map[string]string{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planPrecompute() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("planCacheFingerprint error", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		p := planner{goBin: "/nonexistent/go", opts: Options{CacheDir: "cache"}}
		pkg := &packages.Package{Module: &packages.Module{Dir: "/mod"}}

		_, err := planPrecompute(ctx, p, pkg, []string{"/mod/x.go"}, map[string]string{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("planPrecompute() error = %v, want %v", err, context.Canceled)
		}
	})
}

// TestPlanPackage covers planPackage's three "nothing to mutate here"
// no-ops (no module, empty module dir, every GoFile is a _test.go) plus
// planPrecompute's error propagating through unchanged.
func TestPlanPackage(t *testing.T) {
	t.Parallel()

	t.Run("nil module is skipped", func(t *testing.T) {
		t.Parallel()

		jobs, err := planPackage(t.Context(), planner{}, &packages.Package{}, map[string]string{})
		if err != nil || jobs != nil {
			t.Errorf("planPackage() = (%v, %v), want (nil, nil) for a package with no module", jobs, err)
		}
	})

	t.Run("empty module dir is skipped", func(t *testing.T) {
		t.Parallel()

		pkg := &packages.Package{Module: &packages.Module{Dir: ""}}

		jobs, err := planPackage(t.Context(), planner{}, pkg, map[string]string{})
		if err != nil || jobs != nil {
			t.Errorf("planPackage() = (%v, %v), want (nil, nil) for an empty module dir", jobs, err)
		}
	})

	t.Run("only test files is skipped", func(t *testing.T) {
		t.Parallel()

		pkg := &packages.Package{
			Module:  &packages.Module{Dir: "/mod"},
			GoFiles: []string{"/mod/foo_test.go"},
		}

		jobs, err := planPackage(t.Context(), planner{estimateOnly: true}, pkg, map[string]string{})
		if err != nil || jobs != nil {
			t.Errorf("planPackage() = (%v, %v), want (nil, nil) when every GoFile is a _test.go", jobs, err)
		}
	})

	t.Run("planPrecompute error propagates", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		pkg := &packages.Package{
			Module:  &packages.Module{Dir: "/mod"},
			GoFiles: []string{"/mod/f.go"},
		}
		p := planner{goBin: "/nonexistent/go", opts: Options{CacheDir: "cache"}}

		jobs, err := planPackage(ctx, p, pkg, map[string]string{})
		if !errors.Is(err, context.Canceled) || jobs != nil {
			t.Errorf("planPackage() = (%v, %v), want (nil, %v)", jobs, err, context.Canceled)
		}
	})

	// The three states of a per-package typed-mutator binding: no
	// typedPkgs at all (pkgMutators stays the shared, unbound slice),
	// resolved but IllTyped (falls back the same as not being resolved),
	// and a clean resolution (bound, and the job's typed fields populated).
	t.Run("typed-mutator binding", func(t *testing.T) {
		t.Parallel()

		plain, err := mutator.New("control/if")
		if err != nil {
			t.Fatalf("mutator.New(control/if) error = %v", err)
		}

		typed, err := mutator.New("identifier/constswap")
		if err != nil {
			t.Fatalf("mutator.New(identifier/constswap) error = %v", err)
		}

		mutators := []mutator.Mutator{plain, typed}

		pkg := &packages.Package{
			PkgPath: "example.com/pkg",
			Module:  &packages.Module{Dir: "/mod"},
			GoFiles: []string{"/mod/f.go"},
		}

		t.Run("no typedPkgs leaves mutators unbound", func(t *testing.T) {
			t.Parallel()

			p := planner{estimateOnly: true, mutators: mutators}

			jobs, err := planPackage(t.Context(), p, pkg, map[string]string{})
			if err != nil || len(jobs) != 1 {
				t.Fatalf("planPackage() = (%v, %v), want one job", jobs, err)
			}

			if len(jobs[0].mutators) != len(mutators) {
				t.Errorf("mutators = %d, want %d (the shared, unbound slice)", len(jobs[0].mutators), len(mutators))
			}

			if jobs[0].typedFset != nil || jobs[0].typedSyntax != nil {
				t.Errorf("job = %+v, want no typed fields set", jobs[0])
			}
		})

		t.Run("IllTyped falls back to unbound", func(t *testing.T) {
			t.Parallel()

			p := planner{
				estimateOnly: true,
				mutators:     mutators,
				typedPkgs:    map[string]*packages.Package{pkg.PkgPath: {IllTyped: true}},
			}

			jobs, err := planPackage(t.Context(), p, pkg, map[string]string{})
			if err != nil || len(jobs) != 1 {
				t.Fatalf("planPackage() = (%v, %v), want one job", jobs, err)
			}

			if len(jobs[0].mutators) != 1 || jobs[0].mutators[0].Name() != plain.Name() {
				t.Errorf("mutators = %v, want just the plain one (typed dropped, IllTyped)", jobs[0].mutators)
			}

			if jobs[0].typedFset != nil || jobs[0].typedSyntax != nil {
				t.Errorf("job = %+v, want no typed fields set for an IllTyped package", jobs[0])
			}
		})

		// No TypedMutator in this subtest's own mutator set (unlike the
		// other two above): binding one for real needs a *packages.Package
		// with genuine type-checked info (Types/TypesInfo from a real
		// go/packages.Load), which is exactly the expensive-to-fabricate
		// case [TestBindMutators] itself only covers on the typedPkg==nil
		// side. What's cheap and still worth pinning here is planPackage's
		// own half of the contract: a resolved, non-IllTyped typedPkg makes
		// it through to the job's typed fields regardless of which
		// mutators are selected.
		t.Run("clean resolution populates typed fields", func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			syntax := &ast.File{Name: &ast.Ident{Name: "pkg"}}
			typedPkg := &packages.Package{
				Fset:            fset,
				CompiledGoFiles: []string{"/mod/f.go"},
				Syntax:          []*ast.File{syntax},
			}
			p := planner{
				estimateOnly: true,
				mutators:     []mutator.Mutator{plain},
				typedPkgs:    map[string]*packages.Package{pkg.PkgPath: typedPkg},
			}

			jobs, err := planPackage(t.Context(), p, pkg, map[string]string{})
			if err != nil || len(jobs) != 1 {
				t.Fatalf("planPackage() = (%v, %v), want one job", jobs, err)
			}

			if len(jobs[0].mutators) != 1 {
				t.Errorf("mutators = %d, want 1 (the plain one, bound but unaffected)", len(jobs[0].mutators))
			}

			if jobs[0].typedFset != fset || jobs[0].typedSyntax != syntax {
				t.Errorf("job typed fields = (%v, %v), want (%v, %v)", jobs[0].typedFset, jobs[0].typedSyntax, fset, syntax)
			}
		})
	})
}

// TestBindMutators covers the typedPkg==nil half of bindMutators: a
// TypedMutator selected without usable type information for the package is
// dropped entirely, while a plain, untyped mutator passes through as the
// identical, run-wide shared instance.
func TestBindMutators(t *testing.T) {
	t.Parallel()

	plain, err := mutator.New("control/if")
	if err != nil {
		t.Fatalf("mutator.New(control/if) error = %v", err)
	}

	typed, err := mutator.New("identifier/constswap")
	if err != nil {
		t.Fatalf("mutator.New(identifier/constswap) error = %v", err)
	}

	got := bindMutators([]mutator.Mutator{plain, typed}, nil)

	if len(got) != 1 {
		t.Fatalf("bindMutators() with no usable type info returned %d mutator(s), want 1 (the typed one dropped)", len(got))
	}

	if got[0].Name() != plain.Name() {
		t.Errorf("bindMutators() kept %q, want the untyped operator %q", got[0].Name(), plain.Name())
	}
}

// TestSyntaxForNoMatch covers syntaxFor's fallback: a path that is not
// among typedPkg.CompiledGoFiles returns nil rather than panicking or
// returning a mismatched tree.
func TestSyntaxForNoMatch(t *testing.T) {
	t.Parallel()

	typedPkg := &packages.Package{
		CompiledGoFiles: []string{"/a/b/other.go"},
		Syntax:          []*ast.File{{}},
	}

	if got := syntaxFor(typedPkg, "/a/b/target.go"); got != nil {
		t.Errorf("syntaxFor() = %v, want nil when path is not among CompiledGoFiles", got)
	}
}

// TestSyntaxForMatch is [TestSyntaxForNoMatch]'s counterpart: the one
// success path, including that the returned *ast.File is looked up by the
// matching index, not merely "some" syntax tree off the package.
func TestSyntaxForMatch(t *testing.T) {
	t.Parallel()

	want := &ast.File{Name: &ast.Ident{Name: "target"}}
	other := &ast.File{Name: &ast.Ident{Name: "other"}}

	typedPkg := &packages.Package{
		CompiledGoFiles: []string{"/a/b/other.go", "/a/b/target.go"},
		Syntax:          []*ast.File{other, want},
	}

	if got := syntaxFor(typedPkg, "/a/b/target.go"); got != want {
		t.Errorf("syntaxFor() = %v, want the syntax tree at the matching index", got)
	}
}

// TestMutateFileParseError covers the parser.ParseFile failure path: a file
// with invalid Go syntax must fail the walk with a wrapped parse error
// rather than panicking or silently skipping the file.
func TestMutateFileParseError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "broken.go")

	if err := os.WriteFile(path, []byte("package broken\n\nfunc F( {\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	job := &fileJob{moduleDir: root, path: path}

	err := mutateFile(t.Context(), nil, job, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("mutateFile() error = %v, want it to name a parse failure", err)
	}
}

// TestMutateFileRelPathFallback covers filepath.Rel's own failure inside
// mutateFile: a moduleDir that cannot be made relative to path (mismatched
// absolute/relative) must fall back to the absolute path rather than
// failing the whole walk — mutateFile has nothing else to do with an empty
// mutators list, so a clean, error-free return is the only observable
// proof the fallback was taken instead of the walk aborting.
func TestMutateFileRelPathFallback(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "ok.go")

	if err := os.WriteFile(path, []byte("package ok\n\nfunc F() {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// A relative moduleDir against an absolute path is exactly what makes
	// filepath.Rel itself return an error (verified directly against
	// filepath.Rel's own documented behaviour, not an assumption).
	job := &fileJob{moduleDir: "relative/moduledir", path: path}

	if err := mutateFile(t.Context(), nil, job, nil, nil); err != nil {
		t.Errorf("mutateFile() error = %v, want nil: an unrelatable moduleDir should fall back, not fail the walk", err)
	}
}

// TestMutateFileCtxCancelledDuringWalk covers the ast.Inspect-level
// cancellation check, distinct from [TestMutateFileStopsWhenCancelled]'s
// already-cancelled-before-the-call case: engineCtxErrAfterFirst lets the
// pre-parse check (the first ctx.Err() call) see a live context, then
// reports cancellation starting from the very first AST node the walk
// visits.
func TestMutateFileCtxCancelledDuringWalk(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "ok.go")

	if err := os.WriteFile(path, []byte("package ok\n\nfunc F() {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx := &engineCtxErrAfterFirst{Context: t.Context()}
	job := &fileJob{moduleDir: root, path: path}

	err := mutateFile(ctx, nil, job, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("mutateFile() error = %v, want %v", err, context.Canceled)
	}
}

// TestVisitNodeFuncPatternSkip covers the FuncPattern filter: a function
// whose name does not match must be skipped (visit=false) before suppression
// or any mutator is ever consulted.
func TestVisitNodeFuncPatternSkip(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "skip.go", "package skip\n\nfunc Foo() {}\n", parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var funcDecl *ast.FuncDecl

	ast.Inspect(file, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok {
			funcDecl = fd

			return false
		}

		return true
	})

	if funcDecl == nil {
		t.Fatal("no FuncDecl found in fixture source")
	}

	w := walkState{job: &fileJob{funcPattern: regexp.MustCompile("^Bar$")}}
	spec := &mutant{fset: fset}

	visit, err := visitNode(t.Context(), w, spec, funcDecl)
	if err != nil {
		t.Fatalf("visitNode() error = %v", err)
	}

	if visit {
		t.Error("visitNode() visit = true, want false: FuncPattern should have skipped Foo")
	}
}

// TestVisitMutationsCtxCancelled covers visitMutations' own mid-loop
// cancellation check, called directly with an already-cancelled context so
// the very first mutation offered hits it — distinct from
// [TestMutateFileStopsWhenCancelled]'s outer-walk check and
// [TestMutateFileCtxCancelledDuringWalk]'s ast.Inspect-level one.
func TestVisitMutationsCtxCancelled(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	src := "package cond\n\nfunc F() {\n\tif true {\n\t\tprintln()\n\t}\n}\n"

	file, err := parser.ParseFile(fset, "cond.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	var ifStmt *ast.IfStmt

	ast.Inspect(file, func(n ast.Node) bool {
		if is, ok := n.(*ast.IfStmt); ok {
			ifStmt = is

			return false
		}

		return true
	})

	if ifStmt == nil {
		t.Fatal("no IfStmt found in fixture source")
	}

	m, err := mutator.New("control/if")
	if err != nil {
		t.Fatalf("mutator.New(control/if) error = %v", err)
	}

	if !m.Applies(ifStmt) {
		t.Fatal("control/if does not apply to the fixture's if statement")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	w := walkState{job: &fileJob{}, dedup: make(idDeduper)}
	spec := &mutant{fset: fset, path: "cond.go"}

	if err := visitMutations(ctx, w, spec, m, ifStmt); !errors.Is(err, context.Canceled) {
		t.Errorf("visitMutations() error = %v, want %v", err, context.Canceled)
	}
}

// TestPackageBaselineErrors covers both of packageBaseline's fail-soft
// zero-baseline outcomes: packagePattern failing (mismatched
// absolute/relative moduleDir/pkgDir) and goTestSuite itself failing (a
// nonexistent go binary, the same defensive pattern every other whitebox
// test in this package uses).
func TestPackageBaselineErrors(t *testing.T) {
	t.Parallel()

	t.Run("packagePattern failure", func(t *testing.T) {
		t.Parallel()

		got := packageBaseline(t.Context(), "/nonexistent/go", "relative/moduledir", "/absolute/pkgdir")
		if got != 0 {
			t.Errorf("packageBaseline() = %v, want 0 when the package cannot be located within the module", got)
		}
	})

	t.Run("goTestSuite failure", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		got := packageBaseline(t.Context(), "/nonexistent/go", root, root)
		if got != 0 {
			t.Errorf("packageBaseline() = %v, want 0 when the toolchain cannot be run", got)
		}
	})
}

// TestBuildEstimateResultScopeFull covers the [ScopeFull] branch: a single
// whole-module baseline sample is timed once (never reached under a
// narrower scope, which every other buildEstimateResult-exercising test in
// this package uses instead) and applied identically to every package.
func TestBuildEstimateResultScopeFull(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	counts := estimateCounts{
		total: 3,
		order: []string{"example.com/pkg"},
		hits:  map[string]*pkgHits{"example.com/pkg": {moduleDir: root, pkgDir: root, count: 3}},
	}

	result := buildEstimateResult(t.Context(), "/nonexistent/go", Options{Scope: ScopeFull, Parallel: 2}, counts)

	if result.Total != 3 {
		t.Errorf("Total = %d, want 3", result.Total)
	}

	if len(result.Packages) != 1 || result.Packages[0].Baseline != 0 {
		t.Errorf("Packages = %+v, want one entry with a zero baseline (the toolchain call failed)", result.Packages)
	}
}

// TestLoadTopLevelError, TestLoadTypedTopLevelError and
// TestLoadClosuresTopLevelError each cover their own function's top-level
// packages.Load() error return — as opposed to the per-package errs>0 path
// [TestRunRejectsInvalidOptions]/[TestEstimateRejectsInvalidOptions] already
// cover — via a Dir that does not exist at all, which fails during
// packages.Load's own chdir before it ever gets to resolving a pattern.
// Calling these directly, independent of load()'s own success, is the only
// way to isolate loadTyped's/loadClosures's own error return: called
// through Run/Estimate, an unusable Dir always fails load()'s identical,
// earlier call first. This is a real (but cheap — `go list` fails near
// instantly on a missing directory, no compilation or test execution)
// subprocess call, the same "go list is not toolchain execution" precedent
// [TestWalkForEstimateSpawnsNoSubprocess] already establishes for this file.
func TestLoadTopLevelError(t *testing.T) {
	t.Parallel()

	if _, err := load(t.Context(), Options{Dir: "/nonexistent/dir/turango-xyz"}); err == nil {
		t.Fatal("load() error = nil, want an error for a nonexistent Dir")
	}
}

func TestLoadTypedTopLevelError(t *testing.T) {
	t.Parallel()

	if _, err := loadTyped(t.Context(), Options{Dir: "/nonexistent/dir/turango-xyz"}); err == nil {
		t.Fatal("loadTyped() error = nil, want an error for a nonexistent Dir")
	}
}

func TestLoadClosuresTopLevelError(t *testing.T) {
	t.Parallel()

	if _, err := loadClosures(t.Context(), Options{Dir: "/nonexistent/dir/turango-xyz"}); err == nil {
		t.Fatal("loadClosures() error = nil, want an error for a nonexistent Dir")
	}
}

// TestLoadClosuresSkipsFilelessPackage covers loadClosures' fileless-package
// skip: a directory holding only a black-box "_test.go" file (package
// foo_test, no foo.go at all) makes packages.Load, under Tests:true,
// synthesize a base production-package entry with empty GoFiles *and*
// CompiledGoFiles *and* no Errors — confirmed empirically against the real
// go/packages driver, not assumed — which loadClosures must skip rather
// than recording under a zero-length files[0] index.
func TestLoadClosuresSkipsFilelessPackage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	files := map[string]string{
		filepath.Join(root, "go.mod"):                 "module example.com/fileless\n\ngo 1.23\n",
		filepath.Join(root, "extonly", "foo_test.go"): "package foo_test\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n",
	}

	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	byDir, err := loadClosures(t.Context(), Options{Dir: root, Packages: []string{"./..."}})
	if err != nil {
		t.Fatalf("loadClosures() error = %v", err)
	}

	extonlyDir := filepath.Join(root, "extonly")

	entries := byDir[extonlyDir]
	if len(entries) == 0 {
		t.Fatalf("loadClosures() has no entries for %s, want at least the external test variant", extonlyDir)
	}

	for _, pkg := range entries {
		if len(pkg.GoFiles) == 0 && len(pkg.CompiledGoFiles) == 0 {
			t.Errorf("loadClosures() kept a fileless package variant %+v, want the fileless base variant skipped entirely", pkg)
		}
	}
}
