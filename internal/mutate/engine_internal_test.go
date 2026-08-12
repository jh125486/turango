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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
// ROADMAP.md gap 13's colliding binary-chain siblings via rank, never
// collide).
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

// TestMutantIDRankZeroMatchesPreGap13Hash locks in ROADMAP.md gap 13's
// backward-compatibility promise: rank 0 — every mutation that never
// collided with a same-position sibling, i.e. the overwhelming majority of
// real code, including every *ast.BinaryExpr that is not itself nested
// inside another one starting at the same position — must hash to exactly
// the same bytes mutantID produced before rank existed. This is asserted
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
		}()
	}

	wg.Wait()

	got := sink.close()

	if len(got.Mutants) != len(mutants) || len(got.Suppressions) != len(mutants) {
		t.Fatalf("collector kept %d mutants and %d suppressions, want %d of each",
			len(got.Mutants), len(got.Suppressions), len(mutants))
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

	_, err := baselineTimeout(t.Context(), baselineRuns, 4, timer)
	if !errors.Is(err, wantErr) {
		t.Fatalf("baselineTimeout() error = %v, want it to wrap %v", err, wantErr)
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
// ROADMAP.md gap 11a's claim that Estimate's counting phase costs "an AST
// walk with no go test subprocess anywhere": it asserts execCalls (see
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
