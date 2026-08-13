//go:build integration

// Integration + whitebox: this test needs both the "integration" build tag
// (it shells out to the real Go toolchain, see engine_integration_test.go's
// header) and unexported access (the loadTypedCalls counter has no exported
// equivalent). fixtureModule comes from engine_internal_test.go: that file
// has no build tag, so it always compiles alongside this one, and the two
// share one package (mutate) — redeclaring the helper here would collide
// with it rather than being shadowed, the way the blackbox/whitebox split
// allows elsewhere.
package mutate

import (
	"reflect"
	"testing"
	"time"
)

// TestRunWithoutConstSwapNeverLoadsTypes is the concrete regression test for
// the "a run that does not select a TypedMutator operator pays nothing for
// type information" claim: it asserts loadTyped is never invoked during the
// run, rather than only inferring the claim from the run's output looking
// otherwise normal.
//
// It deliberately does not call t.Parallel(). loadTypedCalls is a
// process-wide counter shared by every test in this compiled binary, which
// would otherwise inflate the counter for reasons having nothing to do with
// this test. Go's test runner runs every non-parallel top-level test to
// completion before any t.Parallel() test's body resumes past its own
// Parallel() call, so running this one non-parallel — and first, textually,
// though order is not what makes this safe — is what makes the delta
// assertion reliable rather than a race against its siblings.
func TestRunWithoutConstSwapNeverLoadsTypes(t *testing.T) {
	root := fixtureModule(t)

	before := loadTypedCalls.Load()

	_, err := Run(t.Context(), Options{
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

// TestRunCacheResumeIsFree is the persistent cache's load-bearing
// end-to-end test: two Run calls against the same unmodified fixture
// module with the same CacheDir. The second must reuse every verdict the
// first computed — zero additional go test/go build subprocesses — and
// produce a byte-for-byte identical Result, the concrete proof of
// "resumes near-instantly" the feature exists to deliver, not an
// assertion in prose.
//
// Deliberately not t.Parallel(): execCalls is a process-wide counter shared
// by every test in this compiled binary, and this is the one test in this
// file that needs its own delta to be exactly zero, not merely smaller
// than some other test's increments — the same non-parallel requirement,
// and the same reasoning, TestRunWithoutConstSwapNeverLoadsTypes above
// already establishes for loadTypedCalls.
func TestRunCacheResumeIsFree(t *testing.T) {
	root := fixtureModule(t)
	cacheDir := t.TempDir()

	// Operators is deliberately narrowed to a single, cheap operator: this
	// test's whole point is the caching behavior around a Run, not
	// exercising every operator (TestRunAgainstRealModule already does
	// that against the same fixture) — Run() runs twice here, so keeping
	// the mutant count small keeps this test's wall-clock cost bounded
	// regardless of how many mutants the full default operator set would
	// produce.
	opts := Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Operators:   []string{"operator/binary"},
		CacheDir:    cacheDir,
		TestTimeout: 30 * time.Second,
	}

	before1 := execCalls.Load()

	result1, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() (first) error = %v", err)
	}

	after1 := execCalls.Load()

	if after1 <= before1 {
		t.Fatal("Run() (first) spawned no go test subprocess at all; test fixture problem, not the property under test")
	}

	if len(result1.Mutants) == 0 {
		t.Fatal("Run() (first) produced no mutants; test fixture problem, not the property under test")
	}

	result2, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() (second) error = %v", err)
	}

	after2 := execCalls.Load()

	if after2 != after1 {
		t.Errorf("Run() (second) spawned %d additional go test/go build subprocess(es) against unchanged source with the same CacheDir, want 0", after2-after1)
	}

	if !reflect.DeepEqual(result1, result2) {
		t.Errorf("Run() (second) Result differs from the first run's despite unchanged source and the same CacheDir:\nfirst:  %+v\nsecond: %+v", result1, result2)
	}
}

// TestRunCacheScopeInteraction covers the cache key's Scope field:
// the identical mutant, unchanged source, must not be served from a cache
// entry written under a different Scope — Scope is documented to change a
// mutant's real verdict (a neighbouring package's test can kill it under
// ScopeFull and never even run under ScopePackage), so it must be
// load-bearing in the key, not decorative.
func TestRunCacheScopeInteraction(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)
	cacheDir := t.TempDir()

	// Operators narrowed for the same wall-clock-bounding reason
	// TestRunCacheResumeIsFree's own comment gives — this test also runs
	// Run() twice.
	opts := Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Operators:   []string{"operator/binary"},
		Scope:       ScopeFull,
		CacheDir:    cacheDir,
		TestTimeout: 30 * time.Second,
	}

	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatalf("Run() (ScopeFull) error = %v", err)
	}

	before := execCalls.Load()

	opts2 := opts
	opts2.Scope = ScopePackage

	if _, err := Run(t.Context(), opts2); err != nil {
		t.Fatalf("Run() (ScopePackage) error = %v", err)
	}

	if execCalls.Load() <= before {
		t.Error("Run() (ScopePackage) spawned no go test subprocess: a ScopeFull-cached verdict was wrongly served under ScopePackage")
	}
}

// TestRunCacheTCEInteraction covers the cache key's TCE field:
// a mutation cached as Equivalent under TCE=true must never be
// short-circuited as equivalent again when a later run has TCE=false —
// that mutation was never actually run against the suite the first time,
// so under TCE=false it needs a real Killed/Survived/NotViable
// classification, which the cache does not have.
func TestRunCacheTCEInteraction(t *testing.T) {
	t.Parallel()

	root := tceModule(t, tceFixture)
	cacheDir := t.TempDir()

	opts := Options{
		Packages:    []string{"."},
		Dir:         root,
		Operators:   []string{"statement/remover"},
		TCE:         true,
		CacheDir:    cacheDir,
		TestTimeout: 30 * time.Second,
	}

	result1, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() (TCE=true) error = %v", err)
	}

	if len(result1.Equivalents) == 0 {
		t.Fatal("Run() (TCE=true) found no equivalent mutations; test fixture problem, not the property under test")
	}

	before := execCalls.Load()

	opts2 := opts
	opts2.TCE = false

	result2, err := Run(t.Context(), opts2)
	if err != nil {
		t.Fatalf("Run() (TCE=false) error = %v", err)
	}

	if execCalls.Load() <= before {
		t.Error("Run() (TCE=false) spawned no go test/go build subprocess: an Equivalent-cached verdict from the TCE=true run was wrongly served under TCE=false")
	}

	if len(result2.Equivalents) != 0 {
		t.Errorf("Run() (TCE=false) reported %d equivalent mutation(s), want 0: TCE=false must never short-circuit a mutant as equivalent", len(result2.Equivalents))
	}

	if want := len(result1.Mutants) + len(result1.Equivalents); len(result2.Mutants) != want {
		t.Errorf("Run() (TCE=false) produced %d mutants, want %d (every mutation TCE=true filtered as equivalent must now receive a real classification)", len(result2.Mutants), want)
	}
}

// TestRunCacheMutantReplayInteraction covers the cache's
// -mutatemutant interaction: a replay must always bypass a cache *read*
// (it exists specifically to reproduce one mutant against a real go test
// run, so serving it a cached verdict would defeat its whole purpose), but
// its freshly-confirmed verdict is still written through afterward,
// benefiting a later normal run's cache hit rate.
//
// Deliberately not t.Parallel(): its third Run() call needs its own
// execCalls delta to be exactly zero, the same process-wide-counter
// requirement TestRunCacheResumeIsFree's own comment gives — this test also
// asserts an exact-zero delta (see below), so it needs the same isolation.
// This test originally shipped with t.Parallel() set despite having the
// same shape, which surfaced as a real CI-only flake: on a low-core-count
// runner, ~15 other t.Parallel() tests in this file queue through few
// concurrent slots, widening the window for a sibling's subprocess spawn to
// land between this test's own Run() calls and inflate the shared counter.
func TestRunCacheMutantReplayInteraction(t *testing.T) {
	root := fixtureModule(t)
	cacheDir := t.TempDir()

	opts := Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Operators:   []string{"operator/binary"},
		CacheDir:    cacheDir,
		TestTimeout: 30 * time.Second,
	}

	result1, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() (first, normal) error = %v", err)
	}

	if len(result1.Mutants) == 0 {
		t.Fatal("Run() (first, normal) produced no mutants; test fixture problem, not the property under test")
	}

	pickedID := result1.Mutants[0].ID

	before := execCalls.Load()

	replayOpts := opts
	replayOpts.MutantID = pickedID

	result2, err := Run(t.Context(), replayOpts)
	if err != nil {
		t.Fatalf("Run() (replay) error = %v", err)
	}

	afterReplay := execCalls.Load()

	if afterReplay <= before {
		t.Error("Run() (replay) spawned no go test subprocess: -mutatemutant replay must always bypass a cache read, even though this exact mutant is already cached")
	}

	if len(result2.Mutants) != 1 || result2.Mutants[0].ID != pickedID {
		t.Fatalf("Run() (replay) Mutants = %+v, want exactly one mutant with ID %q", result2.Mutants, pickedID)
	}

	// A subsequent normal run must now be served entirely from cache,
	// including the replayed mutant's freshly-written record — the same
	// resume-is-free property TestRunCacheResumeIsFree pins directly,
	// checked here specifically across a replay boundary.
	result3, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run() (third, normal) error = %v", err)
	}

	if afterThird := execCalls.Load(); afterThird != afterReplay {
		t.Errorf("Run() (third, normal) spawned %d go test/go build subprocess(es), want 0: the replay's own verdict should already be cached", afterThird-afterReplay)
	}

	// Compared by ID -> Status rather than reflect.DeepEqual on the whole
	// Mutants slice: Output legitimately differs between result1 and
	// result3 for the replayed mutant specifically, since `go test -json`
	// embeds real elapsed-time text that is never identical across two
	// separate, genuine subprocess runs (result1's original run and
	// result2's replay) — that is expected non-determinism in a field the
	// cache never claims to make byte-stable, not a sign the cache
	// mis-served anything. Status is the actual verdict, and is what
	// "reflects that replay's result" means here.
	want := make(map[string]Status, len(result1.Mutants))
	for _, m := range result1.Mutants {
		want[m.ID] = m.Status
	}

	got := make(map[string]Status, len(result3.Mutants))
	for _, m := range result3.Mutants {
		got[m.ID] = m.Status
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run() (third, normal) ID->Status = %+v, want %+v (unchanged now that every mutant, including the replayed one, is cache-resident)", got, want)
	}
}
