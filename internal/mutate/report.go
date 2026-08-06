// Package mutate implements turango's mutation-testing engine: it walks the
// AST of every target package, asks each registered operator what it would
// change, and runs the package's tests against each individual change in an
// isolated copy of the module to find out whether the suite notices.
//
// The engine is split across three files:
//
//	report.go   the result types a run produces, and how they are reported
//	engine.go   orchestration: package resolution, the AST walk, the mutant loop
//	runner.go   one mutant: apply, print, copy the module, run `go test`, classify
package mutate

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

// Status is the verdict for a single mutant.
type Status int

const (
	// Killed means the mutated code compiled and at least one test failed: the
	// suite noticed the change.
	Killed Status = iota

	// Survived means the mutated code compiled and every test still passed:
	// nothing in the suite asserts on the behaviour that was changed.
	Survived

	// NotViable means the mutated code did not compile. It says nothing about
	// the test suite, so these mutants are excluded from the score entirely
	// rather than counted as kills — a compile error is not a caught bug.
	NotViable
)

// String reports the status name used in reports.
func (s Status) String() string {
	switch s {
	case Killed:
		return "killed"
	case Survived:
		return "survived"
	case NotViable:
		return "not-viable"
	default:
		return "unknown"
	}
}

// MarshalJSON encodes the status as its [Status.String] name.
//
// Status is an iota'd int, and the default encoding — 0, 1, 2 — makes the JSON
// report unreadable without a copy of this file, and silently reinterprets every
// existing report the day a status is inserted in the middle of the list. The
// names are the stable, self-describing wire form; the numbers are an
// implementation detail that stops at the package boundary.
func (s Status) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON decodes the name form written by [Status.MarshalJSON].
//
// The pair exists so a report round-trips: turango's own tests read reports back
// in, and a consumer decoding into these types deserves the same. An unknown
// name is an error rather than a silent [Killed] — mapping a status turango does
// not recognise onto the one that means "the suite caught it" would inflate
// whatever the consumer computes from it.
func (s *Status) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("mutate: decoding a status: %w", err)
	}

	switch name {
	case Killed.String():
		*s = Killed
	case Survived.String():
		*s = Survived
	case NotViable.String():
		*s = NotViable
	default:
		return fmt.Errorf("mutate: unknown status %q (want %q, %q or %q)",
			name, Killed, Survived, NotViable)
	}

	return nil
}

// MutantResult is the outcome of running the test suite against exactly one
// applied mutation.
//
// A mutation whose printed source is byte-identical to the unmutated source
// never becomes a MutantResult: it is not a real mutant and the engine skips it
// silently (see [runner.run]).
type MutantResult struct {
	// File is the absolute path of the mutated source file. Reporting layers
	// are expected to relativise it for display; the engine keeps it absolute
	// so results stay unambiguous across modules.
	File string

	// Line is the line the mutated node starts on, in the original file.
	Line int

	// Operator is the registry name of the mutator that produced the mutation,
	// e.g. "operator/binary".
	Operator string

	// Description is the operator's human-readable summary of the edit, e.g.
	// "== -> !=".
	Description string

	// Status is the verdict.
	Status Status

	// Output is the captured output of the mutant's `go test` run. It is the
	// only way to explain a Survived or NotViable verdict to a user, so it is
	// retained even though it is by far the largest field here.
	Output string
}

// SuppressionResult records one node the walk refused to mutate because of a
// //nomutant directive.
//
// It is not a mutant and never becomes one: the walk stops at the suppressed
// node, so which mutations the operators would have offered inside it is never
// discovered. That is why suppressions are tracked separately from
// [MutantResult] rather than as a fourth [Status] — there is no verdict to
// record, only the fact that nothing was attempted.
type SuppressionResult struct {
	// File is the absolute path of the file holding the suppressed node,
	// matching [MutantResult.File].
	File string

	// Line is the line the suppressed node starts on. For a node suppressed by
	// a directive on a compound statement — where the whole subtree is skipped
	// — this is the compound statement's line, which is also where the
	// directive that caused it can be found.
	Line int

	// Reason is the text following the directive's colon
	// (`//nomutant: known flaky`), or empty for a bare `//nomutant`.
	Reason string
}

// Result accumulates every mutant a run produced, in walk order: files in
// package order, nodes in AST order, operators sorted by name. The order is
// deterministic so two runs over unchanged sources produce identical reports.
//
// Later phases extend this with scoring thresholds; the counts below are
// derived on demand rather than maintained incrementally so that a
// partially-filled Result (from a cancelled run) is always self consistent.
type Result struct {
	Mutants []MutantResult

	// Suppressions holds every //nomutant hit, in the same walk order as
	// Mutants. It is deliberately outside the scoring path: a suppressed node
	// produced no mutant, so it appears in neither [Result.Counts] nor
	// [Result.Score]. Reporting surfaces it separately, as
	// [Result.SuppressionRatio], so that liberal suppression is visible rather
	// than quietly inflating the score.
	Suppressions []SuppressionResult
}

// SuppressedCount reports how many nodes were skipped because of a //nomutant
// directive.
func (r *Result) SuppressedCount() int { return len(r.Suppressions) }

// Counts reports how many mutants landed in each status.
func (r *Result) Counts() (killed, survived, notViable int) {
	for _, m := range r.Mutants {
		switch m.Status {
		case Killed:
			killed++
		case Survived:
			survived++
		case NotViable:
			notViable++
		}
	}

	return killed, survived, notViable
}

// Score is the mutation score: killed / (killed + survived).
//
// NotViable mutants are excluded from both halves of the ratio — they measure
// the mutation operators' precision, not the test suite's. Score reports 0 when
// no viable mutant ran at all, and the second return value reports whether the
// score is meaningful, so a caller can distinguish "scored zero" from "nothing
// to score".
func (r *Result) Score() (float64, bool) {
	killed, survived, _ := r.Counts()

	viable := killed + survived
	if viable == 0 {
		return 0, false
	}

	return float64(killed) / float64(viable), true
}

// SuppressionRatio is suppressed / (killed + survived + suppressed): the share
// of the code turango was asked to judge that a //nomutant directive put out of
// reach.
//
// It is the counterweight to [Result.Score]. A suppressed node is excluded from
// the score's denominator, so suppressing the parts of a package the tests do
// not cover raises the score without a single new assertion being written —
// exactly the way a `// nocoverage`-style pragma games a coverage number. The
// two numbers are only trustworthy together, which is why reporting prints them
// side by side.
//
// NotViable mutants are left out of the denominator for the same reason
// [Result.Score] leaves them out: they measure the operators, not the suite. The
// second return value reports whether the ratio is meaningful, so a caller can
// tell "nothing was suppressed" from "there was nothing to suppress".
func (r *Result) SuppressionRatio() (float64, bool) {
	killed, survived, _ := r.Counts()
	suppressed := r.SuppressedCount()

	total := killed + survived + suppressed
	if total == 0 {
		return 0, false
	}

	return float64(suppressed) / float64(total), true
}

// Relativize returns a copy of r with every File expressed relative to base.
//
// The engine records absolute paths so a mutant is unambiguous across the module
// copies it is tested in, but an absolute path is the wrong thing to *publish*:
// a report generated in a CI container names directories that exist nowhere
// else, and the same run on two machines produces two different reports. Both
// the console summary and the JSON report are relativised — against the working
// directory — so a report can be downloaded and read anywhere, and so a path in
// the summary is the same string as the path in the JSON.
//
// A path that cannot be expressed relative to base (a different volume, or an
// already-relative path) is kept exactly as it is: a correct absolute path beats
// a mangled relative one. Relativize never modifies r.
func (r *Result) Relativize(base string) *Result {
	out := &Result{
		Mutants:      make([]MutantResult, len(r.Mutants)),
		Suppressions: make([]SuppressionResult, len(r.Suppressions)),
	}

	copy(out.Mutants, r.Mutants)
	copy(out.Suppressions, r.Suppressions)

	for i := range out.Mutants {
		out.Mutants[i].File = relPath(base, out.Mutants[i].File)
	}

	for i := range out.Suppressions {
		out.Suppressions[i].File = relPath(base, out.Suppressions[i].File)
	}

	return out
}

// relPath renders path for display relative to base, falling back to path
// unchanged whenever that is not possible.
func relPath(base, path string) string {
	if base == "" || path == "" || !filepath.IsAbs(path) {
		return path
	}

	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}

	return rel
}

// WriteSummary prints the human-readable summary of a run to w, with file paths
// relativised against base (see [Result.Relativize]; an empty base leaves them
// absolute).
//
// The layout answers three questions in the order a user asks them: how much was
// attempted, how it scored, and what to go and fix. Only surviving mutants are
// listed individually, because they are the only actionable ones — a killed
// mutant is the suite working, and a not-viable one is an operator producing
// code that does not compile, which says nothing about the tests.
func (r *Result) WriteSummary(w io.Writer, base string) {
	killed, survived, notViable := r.Counts()

	fmt.Fprintf(w, "mutants:    %d\n", len(r.Mutants))
	fmt.Fprintf(w, "killed:     %d\n", killed)
	fmt.Fprintf(w, "survived:   %d\n", survived)
	fmt.Fprintf(w, "not-viable: %d\n", notViable)

	fmt.Fprintln(w)

	if score, ok := r.Score(); ok {
		fmt.Fprintf(w, "score:      %.1f%% (%d killed of %d viable)\n", score*100, killed, killed+survived)
	} else {
		fmt.Fprintln(w, "score:      n/a (no viable mutants)")
	}

	r.writeSuppressions(w)
	r.writeSurvivors(w, base)
}

// writeSuppressions prints the suppression line: the count, and the share of the
// run it kept out of the score.
func (r *Result) writeSuppressions(w io.Writer) {
	suppressed := r.SuppressedCount()

	ratio, ok := r.SuppressionRatio()
	if !ok {
		fmt.Fprintf(w, "suppressed: %d\n", suppressed)

		return
	}

	killed, survived, _ := r.Counts()

	fmt.Fprintf(w, "suppressed: %d of %d nodes (%.1f%% excluded from the score by //nomutant)\n",
		suppressed, killed+survived+suppressed, ratio*100)
}

// writeSurvivors lists the mutants nothing in the suite noticed.
//
// The columns are padded to the widest entry rather than to a fixed width: the
// list is what a user reads down, and a ragged left edge on the operator column
// is what makes it hard to scan.
func (r *Result) writeSurvivors(w io.Writer, base string) {
	type row struct{ location, operator, description string }

	var (
		rows              []row
		locWidth, opWidth int
	)

	for _, m := range r.Mutants {
		if m.Status != Survived {
			continue
		}

		next := row{
			location:    fmt.Sprintf("%s:%d", relPath(base, m.File), m.Line),
			operator:    m.Operator,
			description: m.Description,
		}

		locWidth = max(locWidth, len(next.location))
		opWidth = max(opWidth, len(next.operator))

		rows = append(rows, next)
	}

	if len(rows) == 0 {
		return
	}

	fmt.Fprintf(w, "\nSurviving mutants (%d):\n", len(rows))

	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %-*s  %s\n", locWidth, r.location, opWidth, r.operator, r.description)
	}
}
