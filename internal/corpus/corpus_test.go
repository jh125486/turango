// TestCorpus and its helpers run turango's checked-in mutation-testing
// regression corpus: every corpus/*/golden*.json entry [Discover] finds is
// run for real through internal/mutate.Run, and the result is compared
// against the counts the golden file pins.
//
// This is a slow, real end-to-end suite — every entry compiles and tests its
// target once per mutant — so it follows the same testing.Short() skip
// idiom as internal/mutate's own real-module integration tests
// (TestRunAgainstRealModule in internal/mutate/engine_test.go): skipped by
// default, run explicitly (see .github/workflows/ci.yml's dedicated corpus
// step).
package corpus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/corpus"
	"github.com/jh125486/turango/internal/mutate"
)

// corpusRunTimeout is the overall safety-net context timeout for one entry's
// mutate.Run call. It is deliberately generous and distinct from the
// per-mutant -mutatetimeout that comes from the golden file itself: this
// bound only exists to keep a hung or pathologically large entry (the
// aes-full fixture runs hundreds of mutants) from stalling the whole test
// binary forever, not to constrain any single mutant.
//
// It is also distinct from -- and does not raise -- `go test`'s own
// process-wide -timeout (a default 10m applies unless a caller overrides it,
// e.g. the CI corpus step's explicit -timeout flag): a per-entry context
// timeout only ever produces a clean ctx.Err() from mutate.Run and a
// readable t.Fatalf for that one subtest, whereas the process-wide -timeout
// firing panics the whole binary, mid-run, with no per-entry attribution.
// Both need to be generous; only the first is this package's to set.
const corpusRunTimeout = 30 * time.Minute

// TestCorpus runs every discovered corpus entry as its own subtest.
//
// Entries run sequentially, not under t.Parallel(): each mutate.Run spawns
// its own `go test` subprocesses, and running every entry's mutant sweep
// concurrently makes them compete for the same CPUs rather than actually
// finishing sooner -- observed directly running this suite locally, where
// three entries in parallel took longer combined than the same entries would
// sequentially, and came close to tripping `go test`'s own default
// process-wide timeout. Sequential execution costs wall-clock time as the
// corpus grows, but keeps each entry's timing (and any timeout) attributable
// to the one entry that caused it, which matters more for a CI regression
// gate than raw speed.
func TestCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus entries compile and test a real module copy per mutant")
	}

	root := repoRoot(t)
	corpusDir := filepath.Join(root, "corpus")

	entries, err := corpus.Discover(corpusDir)
	if err != nil {
		t.Fatalf("Discover(%s) error = %v", corpusDir, err)
	}

	if len(entries) == 0 {
		t.Skip("no corpus entries found under " + corpusDir + " yet")
	}

	for _, entry := range entries {
		t.Run(entry.Name, func(t *testing.T) {
			runEntry(t, root, entry)
		})
	}
}

// runEntry builds internal/mutate.Options from entry, runs it, and compares
// the result against entry.Expect.
func runEntry(t *testing.T, root string, entry corpus.Entry) {
	t.Helper()

	opts := buildOptions(t, root, entry)

	ctx, cancel := context.WithTimeout(t.Context(), corpusRunTimeout)
	defer cancel()

	result, err := mutate.Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	killed, survived, notViable := result.Counts()
	mutants := len(result.Mutants)

	var mismatches []string

	if mutants != entry.Expect.Mutants {
		mismatches = append(mismatches, fmt.Sprintf("mutants: got %d, want %d", mutants, entry.Expect.Mutants))
	}

	if killed != entry.Expect.Killed {
		mismatches = append(mismatches, fmt.Sprintf("killed: got %d, want %d", killed, entry.Expect.Killed))
	}

	if survived != entry.Expect.Survived {
		mismatches = append(mismatches, fmt.Sprintf("survived: got %d, want %d", survived, entry.Expect.Survived))
	}

	if notViable != entry.Expect.NotViable {
		mismatches = append(mismatches, fmt.Sprintf("notViable: got %d, want %d", notViable, entry.Expect.NotViable))
	}

	if entry.Expect.Suppressed != nil {
		if got, want := result.SuppressedCount(), *entry.Expect.Suppressed; got != want {
			mismatches = append(mismatches, fmt.Sprintf("suppressed: got %d, want %d", got, want))
		}
	}

	if entry.Expect.Equivalent != nil {
		if got, want := result.EquivalentCount(), *entry.Expect.Equivalent; got != want {
			mismatches = append(mismatches, fmt.Sprintf("equivalent: got %d, want %d", got, want))
		}
	}

	for _, m := range mismatches {
		t.Errorf("%s (golden: %s)", m, entry.Path)
	}
}

// buildOptions turns one golden.json Entry into internal/mutate.Options.
//
// Dir is the fixture's frozen module when ModulePath is set, or the repo
// root when it is empty — the in-place example/example-legacy entries mutate
// turango's own module directly. Packages mirrors the same split: an
// in-place entry mutates exactly its Target pattern, while a dedicated
// module's whole frozen tree ("./...") is the target.
func buildOptions(t *testing.T, root string, entry corpus.Entry) mutate.Options {
	t.Helper()

	scope, err := mutate.ParseScope(entry.Scope)
	if err != nil {
		t.Fatalf("ParseScope(%q) error = %v (golden: %s)", entry.Scope, err, entry.Path)
	}

	timeout, err := time.ParseDuration(entry.Timeout)
	if err != nil {
		t.Fatalf("ParseDuration(%q) error = %v (golden: %s)", entry.Timeout, err, entry.Path)
	}

	var (
		dir      string
		packages []string
	)

	if entry.ModulePath == "" {
		dir = root
		packages = []string{entry.Target}
	} else {
		dir = filepath.Join(root, entry.ModulePath)
		packages = []string{"./..."}
	}

	return mutate.Options{
		Packages:    packages,
		Dir:         dir,
		Operators:   entry.Operators,
		Scope:       scope,
		TestTimeout: timeout,
		TCE:         entry.TCE,
		// Left unset (zero) here, an entry's own mutants would run one file
		// at a time -- engine.go treats Parallel <= 0 as 1 -- which doesn't
		// match the real CLI's GOMAXPROCS default and leaves real speedup on
		// the table for entries with real file/mutant counts (pkix's 291,
		// aes-full's 555+). This is orthogonal to TestCorpus's entry-level
		// sequencing above: that's about not running multiple *entries'*
		// go test sweeps concurrently (proven slower by contention), this is
		// about letting one entry's own file-level worker pool actually work.
		Parallel: runtime.GOMAXPROCS(0),
	}
}

// repoRoot resolves the repository root from the test binary's working
// directory (the package directory, internal/corpus, under `go test`'s
// default behaviour) by walking upward to the directory holding go.mod.
//
// Walking up to find go.mod rather than hard-coding "../.." keeps this
// working regardless of how deep internal/corpus ends up nested or how the
// test binary is invoked, as long as it still runs somewhere under the
// module.
func repoRoot(t *testing.T) string {
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
			t.Fatalf("repoRoot: no go.mod found above %s", wd)
		}

		dir = parent
	}
}
