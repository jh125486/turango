// TestCorpus and its helpers run turango's checked-in mutation-testing
// regression corpus: every corpus/*/golden*.json entry [Discover] finds is
// run for real through internal/mutate.Run, and the result is compared
// against the counts the golden file pins.
//
// This is a slow, real end-to-end suite — every entry compiles and tests its
// target once per mutant, and the slowest single entry alone can run over an
// hour (see corpusRunTimeout's own doc comment for real, observed numbers) —
// so it is skipped under -short. This is the one deliberate exception to
// this project's usual "integration" build-tag convention: unlike
// internal/mutate's own real-module integration tests (TestRunAgainstRealModule,
// internal/mutate/engine_integration_test.go), which moved to a build tag
// specifically so `go test ./...` never even compiles them in, TestCorpus
// stays reachable via a plain `go test ./internal/corpus/...` (no tag
// needed) for a quick local check — it's the -short flag, not compilation,
// that keeps it out of the fast path. Run explicitly via
// `go test -run TestCorpus ./internal/corpus/...`, or see
// .github/workflows/corpus.yml for how CI runs it — deliberately not on
// every push/PR the way the rest of the suite is (see that file's own
// comment for why).
package corpus_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/corpus"
	"github.com/jh125486/turango/internal/mutate"
)

// corpusRunTimeout is the overall safety-net context timeout for one entry's
// mutate.Run call. It is deliberately generous and distinct from the
// per-mutant -mutatetimeout that comes from the golden file itself: this
// bound only exists to keep a hung entry from stalling the whole test
// binary forever, not to constrain any single mutant.
//
// 2 hours, not the 30 minutes this constant originally shipped with — that
// number was a guess ("the aes-full fixture runs hundreds of mutants"),
// made before any real capture existed. It was wrong by more than an order
// of magnitude: stdlib-crypto-aes's real, captured golden is 7746 mutants
// (dominated by internal/fips140/aes alone), and a real run against it, at
// -mutateparallel=GOMAXPROCS, took just over an hour on real hardware —
// the actual numbers this bound is now sized against, not a re-guess.
// This is also why TestCorpus moved out
// of ci.yml's per-PR steps and into corpus.yml's own schedule/
// workflow_dispatch-triggered workflow (see that file's own comment): a
// bound sized for a 1-hour-plus entry has no business gating every PR.
//
// It is also distinct from -- and does not raise -- `go test`'s own
// process-wide -timeout (a default 10m applies unless a caller overrides it,
// e.g. corpus.yml's explicit -timeout flag, kept in sync with this
// constant): a per-entry context timeout only ever produces a clean
// ctx.Err() from mutate.Run and a readable t.Fatalf for that one subtest,
// whereas the process-wide -timeout firing panics the whole binary, mid-run,
// with no per-entry attribution. Both need to be generous; only the first is
// this package's to set.
const corpusRunTimeout = 2 * time.Hour

// TestDiscover exercises corpus.Discover's own walk/parse/sort logic against
// synthetic fixtures built fresh per subtest — unlike TestCorpus below, which
// discovers the real corpus/ tree only as a means to an end-to-end mutation
// run, this is Discover's dedicated, fast unit coverage and never touches the
// real corpus/ directory.
func TestDiscover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(t *testing.T) string // returns corpusDir
		want  []string                  // expected Entry.Name values, in Discover's order
	}{
		{
			name: "a corpusDir that does not exist yet is not an error",
			build: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			want: nil,
		},
		{
			name: "a subdirectory holding two golden files sharing one module gets one entry per file",
			build: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				sub := filepath.Join(dir, "stdlib-crypto-aes")
				mustMkdir(t, sub)
				mustWriteGolden(t, sub, "golden.json", `{"scope":"package"}`)
				mustWriteGolden(t, sub, "golden-full.json", `{"scope":"full"}`)

				return dir
			},
			want: []string{"stdlib-crypto-aes/golden", "stdlib-crypto-aes/golden-full"},
		},
		{
			name: "a subdirectory with no golden file contributes nothing, silently",
			build: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				mustMkdir(t, filepath.Join(dir, "no-golden-here"))
				sub := filepath.Join(dir, "has-one")
				mustMkdir(t, sub)
				mustWriteGolden(t, sub, "golden.json", `{"scope":"full"}`)

				return dir
			},
			want: []string{"has-one/golden"},
		},
		{
			name: "a non-directory entry sitting in corpusDir is skipped, not treated as a fixture",
			build: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a corpus entry"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}

				sub := filepath.Join(dir, "real-entry")
				mustMkdir(t, sub)
				mustWriteGolden(t, sub, "golden.json", `{"scope":"full"}`)

				return dir
			},
			want: []string{"real-entry/golden"},
		},
		{
			// "aes-full/golden" sorts before "aes/golden": '-' (0x2D) is less
			// than '/' (0x2F), so a naive directory-traversal order (which
			// visits "aes" before "aes-full", since directory names alone
			// sort the other way) would emit these two entries in the wrong
			// order without Discover's explicit sort.Slice by Name.
			name: "entries come back sorted by Name, not by directory traversal order",
			build: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				mustMkdir(t, filepath.Join(dir, "aes"))
				mustWriteGolden(t, filepath.Join(dir, "aes"), "golden.json", `{"scope":"package"}`)
				mustMkdir(t, filepath.Join(dir, "aes-full"))
				mustWriteGolden(t, filepath.Join(dir, "aes-full"), "golden.json", `{"scope":"full"}`)

				return dir
			},
			want: []string{"aes-full/golden", "aes/golden"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			corpusDir := tt.build(t)

			entries, err := corpus.Discover(corpusDir)
			if err != nil {
				t.Fatalf("Discover(%s) error = %v", corpusDir, err)
			}

			if tt.want == nil {
				if entries != nil {
					t.Fatalf("Discover(%s) = %v, want nil", corpusDir, entries)
				}

				return
			}

			got := make([]string, len(entries))
			for i, e := range entries {
				got[i] = e.Name

				// Path is set from the real file Discover parsed it from, not
				// just copied from the glob match -- confirm it still points
				// at a real file rather than trusting the string alone.
				if _, err := os.Stat(e.Path); err != nil {
					t.Errorf("entry %q Path = %q does not exist: %v", e.Name, e.Path, err)
				}
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("Discover(%s) names = %v, want %v", corpusDir, got, tt.want)
			}
		})
	}
}

// mustMkdir creates dir, failing the test immediately if it can't -- every
// TestDiscover fixture is built fresh under t.TempDir(), so a failure here
// means the test's own setup is broken, not the code under test.
func mustMkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
}

// mustWriteGolden writes a golden*.json fixture file for TestDiscover.
func mustWriteGolden(t *testing.T, dir, name, data string) {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

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
