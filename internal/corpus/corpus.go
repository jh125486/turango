// Package corpus discovers turango's checked-in mutation-testing regression
// fixtures under the repo's corpus/ directory and parses each one's
// golden.json into an [Entry] that internal/corpus's own tests turn into an
// [internal/mutate.Options] run.
//
// The fixtures themselves — the frozen source modules and their pinned
// expected counts — are not this package's concern; discovery only knows the
// golden.json schema, not what any particular entry is about.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Expect holds the pinned outcome a corpus entry's mutation run must
// reproduce exactly: given frozen source, a frozen test suite and a fixed
// timeout, mutant generation and classification is fully deterministic, so
// these are exact counts, not ranges.
type Expect struct {
	Mutants   int `json:"mutants"`
	Killed    int `json:"killed"`
	Survived  int `json:"survived"`
	NotViable int `json:"notViable"`

	// Suppressed pins the //nomutant suppression count. It is a pointer
	// rather than a bare int so an absent field in golden.json — most
	// entries don't have any suppressions worth pinning — is distinguishable
	// from an explicit "expect zero suppressions"; a caller checks Suppressed
	// != nil before comparing.
	Suppressed *int `json:"suppressed,omitempty"`

	// Equivalent pins the count of mutants Trivial Compiler Equivalence
	// filtered before they ever reached the test suite (see
	// [internal/mutate.Result.EquivalentCount]) — meaningful only when the
	// entry itself has [Entry.TCE] set; a pointer for the same reason
	// Suppressed is one, since most entries don't run under TCE at all and
	// an absent field must not silently mean "expect zero".
	Equivalent *int `json:"equivalent,omitempty"`
}

// Entry is one parsed golden.json: what to run, and what the run must
// produce.
//
// The JSON-tagged fields mirror golden.json's schema directly. Name and Path
// are filled in by [Discover] and are never present in the file itself.
type Entry struct {
	// Name identifies the entry for t.Run/-run: the corpus subdirectory's
	// name plus the golden file's own base name (without extension), e.g.
	// "stdlib-crypto-aes/golden-full". The suffix matters because a
	// directory can hold more than one golden file sharing one module/ (the
	// crypto-aes fixture pins both a package-scope and a full-scope result
	// against the same frozen source) — without it, two entries from the
	// same directory would collide. Always unique across one [Discover]
	// call.
	Name string

	// Path is the absolute path of the golden.json (or golden-*.json) file
	// this Entry was parsed from, kept for error messages.
	Path string

	// Description documents what the entry exercises and, for the stdlib
	// fixtures, where the frozen source was cut from.
	Description string `json:"description"`

	// ModulePath is the frozen fixture module's directory, relative to the
	// repo root (e.g. "corpus/stdlib-crypto-aes/module"). Empty means there
	// is no separate module: the entry runs against turango's own repo
	// module in place, and Target names the pattern to mutate within it.
	ModulePath string `json:"modulePath"`

	// Target is the -mutate pattern to run in-place (e.g. "./example/...").
	// It is only meaningful, and only used, when ModulePath is empty.
	Target string `json:"target"`

	// Scope is the golden file's -mutatescope spelling ("package", "full" or
	// "impact"), parsed with internal/mutate.ParseScope by the caller.
	Scope string `json:"scope"`

	// Operators names the -mutateoperators to apply. Nil means every
	// registered operator.
	Operators []string `json:"operators"`

	// Timeout is a fixed, explicit Go duration (e.g. "30s") for every
	// mutant's `go test` run. Corpus entries never use the engine's
	// baseline-derived timeout: the source is frozen, so a fixed budget is
	// both known ahead of time and free of the run-to-run flakiness a
	// borderline-slow mutant near a derived boundary would otherwise cause.
	Timeout string `json:"timeout"`

	// TCE enables Trivial Compiler Equivalence (internal/mutate.Options.TCE)
	// for this entry. False (the zero value, and every entry's implicit
	// default before this field existed) matches every other entry's
	// existing behavior exactly. Only a fixture built specifically to
	// demonstrate TCE actually filtering a mutant (see
	// corpus/op-tce-equivalent) needs this set.
	TCE bool `json:"tce,omitempty"`

	// Expect is the pinned outcome the run must match.
	Expect Expect `json:"expect"`
}

// Discover walks corpusDir — a directory of subdirectories, each holding one
// or more golden*.json files — and parses every golden file it finds into an
// [Entry].
//
// A subdirectory is free to hold more than one golden file sharing one
// module/ (see [Entry.Name]); Discover globs for "golden*.json" within each
// subdirectory rather than assuming exactly one.
//
// A corpusDir that does not exist yet is not an error: the fixtures this
// package discovers are frozen into place by work independent of this
// harness, and a run against an empty or not-yet-populated corpus/ reports
// zero entries rather than failing.
func Discover(corpusDir string) ([]Entry, error) {
	subs, err := os.ReadDir(corpusDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("corpus: reading %s: %w", corpusDir, err)
	}

	var entries []Entry

	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}

		subDir := filepath.Join(corpusDir, sub.Name())

		goldenFiles, err := filepath.Glob(filepath.Join(subDir, "golden*.json"))
		if err != nil {
			return nil, fmt.Errorf("corpus: globbing %s: %w", subDir, err)
		}

		sort.Strings(goldenFiles)

		for _, gf := range goldenFiles {
			entry, err := parseGolden(gf)
			if err != nil {
				return nil, err
			}

			entry.Name = entryName(sub.Name(), gf)
			entries = append(entries, entry)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	return entries, nil
}

// parseGolden reads and decodes one golden.json file.
func parseGolden(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("corpus: reading %s: %w", path, err)
	}

	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, fmt.Errorf("corpus: parsing %s: %w", path, err)
	}

	entry.Path = path

	return entry, nil
}

// entryName derives a human-readable, unique subtest name for a golden file:
// the corpus subdirectory's name, plus the golden file's own base name
// (without extension).
//
// The suffix is always appended, even for the common case of a lone
// golden.json ("stdlib-x509-pkix/golden"), rather than only when a directory
// holds more than one golden file — a consistent shape is easier to predict
// and to match with -run than one that changes depending on a sibling file's
// existence.
func entryName(dirName, goldenPath string) string {
	base := strings.TrimSuffix(filepath.Base(goldenPath), filepath.Ext(goldenPath))

	return dirName + "/" + base
}
