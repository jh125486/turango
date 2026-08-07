package mutate

import (
	"context"
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

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	return root
}

// TestRunAgainstRealModule is the end-to-end check: a real multi-package module
// is mutated, every mutant is built and tested in its own workspace copy, and
// the run must produce both kills and survivors. Anything that breaks the
// workspace copy or the classifier collapses this into all-NotViable or
// all-Survived, which no amount of unit testing the pieces would catch.
func TestRunAgainstRealModule(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end run compiles and tests one module copy per mutant")
	}

	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// TestTimeout is deliberately left zero so the run exercises the derived
	// baseline path end to end, not just the arithmetic.
	result, err := Run(ctx, Options{
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

	// Every result must be attributable: a report row with no file or operator
	// is useless to a user.
	for _, m := range result.Mutants {
		if m.File == "" || m.Line == 0 || m.Operator == "" || m.Description == "" {
			t.Errorf("incomplete MutantResult: %+v", m)
		}
	}
}

// TestMutateFileStopsWhenCancelled covers the worker's cancellation awareness,
// which SIGINT handling hangs off: an already-cancelled context must stop the
// walk before a single mutant is executed.
func TestMutateFileStopsWhenCancelled(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)
	path := filepath.Join(root, "mathx", "mathx.go")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sink := &collector{result: &Result{}}

	// A deliberately unusable go binary: reaching the runner at all would fail
	// loudly rather than silently passing this test.
	run := &runner{goBin: "/nonexistent/go", testTimeout: time.Second}

	job := fileJob{moduleDir: root, path: path, mutators: mutator.All()}

	err := mutateFile(ctx, run, job, sink)
	if err != context.Canceled {
		t.Fatalf("mutateFile() error = %v, want %v", err, context.Canceled)
	}

	if len(sink.result.Mutants) != 0 {
		t.Errorf("mutateFile() ran %d mutants after cancellation", len(sink.result.Mutants))
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
	if testing.Short() {
		t.Skip("runs the real toolchain once per mutant, twice over")
	}

	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	base := Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       ScopePackage,
		TestTimeout: 2 * time.Minute,
	}

	sequential := base
	sequential.Parallel = 1

	parallel := base
	parallel.Parallel = 4

	seqResult, err := Run(ctx, sequential)
	if err != nil {
		t.Fatalf("Run(parallel=1) error = %v", err)
	}

	parResult, err := Run(ctx, parallel)
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

		if got.File != want.File || got.Line != want.Line ||
			got.Operator != want.Operator || got.Description != want.Description ||
			got.Status != want.Status {
			t.Errorf("mutant %d = %s:%d %s %q %v, want %s:%d %s %q %v",
				i, got.File, got.Line, got.Operator, got.Description, got.Status,
				want.File, want.Line, want.Operator, want.Description, want.Status)
		}
	}
}

// TestRunWithImpactScope exercises the coverage-directed path end to end: the
// per-test coverage map is built, mutants on covered lines are judged by the
// tests that cover them, and mutants on uncovered lines are reported as
// survivors without a test run at all.
func TestRunWithImpactScope(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a coverage map with the real toolchain")
	}

	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := Run(ctx, Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Scope:       ScopeImpact,
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

			if m.Status != Survived {
				t.Errorf("mutant on uncovered line %d = %v, want %v", m.Line, m.Status, Survived)
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
	if testing.Short() {
		t.Skip("end-to-end run compiles and tests one module copy per mutant")
	}

	t.Parallel()

	root := fixtureModule(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := Run(ctx, Options{
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

	if got.Status != Killed {
		t.Errorf("Status = %v, want %v: TestBounded should catch this swap", got.Status, Killed)
	}
}

// TestRunWithoutConstSwapNeverLoadsTypes is the concrete regression test for
// the "a run that does not select a TypedMutator operator pays nothing for
// type information" claim: it asserts loadTyped is never invoked during the
// run, rather than only inferring the claim from the run's output looking
// otherwise normal.
//
// It deliberately does not call t.Parallel(). loadTypedCalls is a
// process-wide counter, and every other test in this file that calls Run()
// with the default operator set now selects identifier/constswap too (it is
// a registered TypedMutator, per mutator.All()'s "enabled purely by being
// imported" contract), which would otherwise inflate the counter for reasons
// having nothing to do with this test. Go's test runner runs every
// non-parallel top-level test to completion before any t.Parallel() test's
// body resumes past its own Parallel() call, so running this one
// non-parallel — and first, textually, though order is not what makes this
// safe — is what makes the delta assertion reliable rather than a race
// against its siblings.
func TestRunWithoutConstSwapNeverLoadsTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real toolchain against every mutant of the fixture module")
	}

	root := fixtureModule(t)

	before := loadTypedCalls.Load()

	_, err := Run(context.Background(), Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Operators:   []string{"operator/binary"},
		Scope:       ScopePackage,
		TestTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if after := loadTypedCalls.Load(); after != before {
		t.Errorf("loadTyped was called %d time(s) during a run selecting no TypedMutator operator, want 0", after-before)
	}
}

func TestParseScope(t *testing.T) {
	t.Parallel()

	for _, scope := range []Scope{ScopeFull, ScopePackage, ScopeImpact} {
		got, err := ParseScope(scope.String())
		if err != nil {
			t.Fatalf("ParseScope(%q) error = %v", scope, err)
		}

		if got != scope {
			t.Errorf("ParseScope(%q) = %v, want %v", scope.String(), got, scope)
		}
	}

	if _, err := ParseScope("Package"); err == nil {
		t.Error("ParseScope(\"Package\") error = nil, want an error: the spellings are lower case")
	}

	if _, err := ParseScope(""); err == nil {
		t.Error("ParseScope(\"\") error = nil, want an error")
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

	sink := &collector{result: &Result{}}

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

	got := sink.sorted()

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

// TestRunRejectsUnknownPackages checks that a bad pattern is an error rather
// than an empty, apparently-successful run.
func TestRunRejectsUnknownPackages(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	if _, err := Run(context.Background(), Options{
		Packages: []string{"./does-not-exist/..."},
		Dir:      root,
	}); err == nil {
		t.Fatal("Run() error = nil, want an error for an unmatched pattern")
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

			got, err := baselineTimeout(context.Background(), tt.runs, tt.cpus, timer)
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

	_, err := baselineTimeout(context.Background(), baselineRuns, 4, timer)
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

		got, err := resolveTimeout(context.Background(), Options{TestTimeout: 7 * time.Second}, timer)
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

		got, err := resolveTimeout(context.Background(), Options{}, timer)
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

// TestRunFailsOnBrokenBaseline is the end-to-end half of the same rule: a module
// whose tests already fail must be rejected before a single mutant runs.
func TestRunFailsOnBrokenBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real toolchain")
	}

	t.Parallel()

	root := fixtureModule(t)

	// Break the suite without breaking the build.
	broken := "package mathx\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) {\n\tt.Fatal(\"already red\")\n}\n"
	if err := os.WriteFile(filepath.Join(root, "mathx", "mathx_test.go"), []byte(broken), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Run(context.Background(), Options{Packages: []string{"./..."}, Dir: root})
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a suite that fails unmutated")
	}

	if !strings.Contains(err.Error(), "unmutated test suite") {
		t.Errorf("Run() error = %v, want it to name the unmutated suite", err)
	}
}

func TestResultScore(t *testing.T) {
	t.Parallel()

	empty := &Result{}
	if _, ok := empty.Score(); ok {
		t.Error("Score() reported a score for an empty result")
	}

	r := &Result{Mutants: []MutantResult{
		{Status: Killed},
		{Status: Killed},
		{Status: Survived},
		{Status: NotViable},
	}}

	killed, survived, notViable := r.Counts()
	if killed != 2 || survived != 1 || notViable != 1 {
		t.Errorf("Counts() = %d, %d, %d; want 2, 1, 1", killed, survived, notViable)
	}

	// NotViable must not dilute the score.
	score, ok := r.Score()
	if !ok || score != 2.0/3.0 {
		t.Errorf("Score() = %v, %v; want %v, true", score, ok, 2.0/3.0)
	}
}
