package mutate_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/mutate"
)

// TestStatusJSON pins the wire form of a status: the names, not the iota. A
// report that encoded 0/1/2 would be unreadable without a copy of report.go and
// would silently change meaning the day a status is inserted in the middle of
// the list.
func TestStatusJSON(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status mutate.Status
		want   string
	}{
		"killed":     {status: mutate.Killed, want: `"killed"`},
		"survived":   {status: mutate.Survived, want: `"survived"`},
		"not viable": {status: mutate.NotViable, want: `"not-viable"`},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.status)
			if err != nil {
				t.Fatalf("Marshal(%v) error = %v", tt.status, err)
			}

			if string(data) != tt.want {
				t.Errorf("Marshal(%v) = %s, want %s", tt.status, data, tt.want)
			}

			var got mutate.Status
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", data, err)
			}

			if got != tt.status {
				t.Errorf("round trip of %v = %v", tt.status, got)
			}
		})
	}
}

// TestStatusUnmarshalRejectsUnknown covers the decision not to fall back to the
// zero value: an unrecognised name mapped onto Killed would inflate whatever the
// consumer computes from the report.
func TestStatusUnmarshalRejectsUnknown(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"an unknown name": `"exploded"`,
		"the old integer": `1`,
		"a null":          `null`,
	}

	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got mutate.Status
			if err := json.Unmarshal([]byte(encoded), &got); err == nil {
				t.Errorf("Unmarshal(%s) = %v, want an error", encoded, got)
			}
		})
	}
}

// TestResultJSONRoundTrip is the check that matters for the report file itself:
// a whole Result must survive being written and read back.
func TestResultJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := &mutate.Result{
		Mutants: []mutate.MutantResult{
			{File: "a.go", Line: 3, Operator: "operator/binary", Description: "== -> !=", Status: mutate.Survived, Before: "v < lo", After: "v >= lo"},
			{File: "b.go", Line: 9, Operator: "control/if", Description: "remove if body", Status: mutate.NotViable, Before: "{ x() }", After: "{}"},
		},
		Suppressions: []mutate.SuppressionResult{{File: "a.go", Line: 12, Reason: "flaky"}},
		Equivalents:  []mutate.EquivalentResult{{File: "c.go", Line: 5, Operator: "statement/remover", Description: "remove statement: dead = 1"}},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if !strings.Contains(string(data), `"Status":"survived"`) {
		t.Errorf("report = %s, want the status spelled out", data)
	}

	var got mutate.Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if len(got.Mutants) != len(want.Mutants) || got.Mutants[0].Status != mutate.Survived || got.Mutants[1].Status != mutate.NotViable {
		t.Errorf("round trip = %+v, want %+v", got.Mutants, want.Mutants)
	}

	if got.Mutants[0].Before != "v < lo" || got.Mutants[0].After != "v >= lo" {
		t.Errorf("round trip dropped before/after: %+v", got.Mutants[0])
	}

	if len(got.Suppressions) != 1 || got.Suppressions[0].Reason != "flaky" {
		t.Errorf("round trip dropped suppressions: %+v", got.Suppressions)
	}

	if len(got.Equivalents) != 1 || got.Equivalents[0].Description != "remove statement: dead = 1" {
		t.Errorf("round trip dropped equivalents: %+v", got.Equivalents)
	}
}

// TestResultScore covers Counts and Score together: the per-status tally and
// the ratio derived from it, including the rule that NotViable mutants must
// not dilute the score.
func TestResultScore(t *testing.T) {
	t.Parallel()

	empty := &mutate.Result{}
	if _, ok := empty.Score(); ok {
		t.Error("Score() reported a score for an empty result")
	}

	r := &mutate.Result{Mutants: []mutate.MutantResult{
		{Status: mutate.Killed},
		{Status: mutate.Killed},
		{Status: mutate.Survived},
		{Status: mutate.NotViable},
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

// TestSuppressionRatio covers the number that keeps //nomutant honest,
// including the two ends: nothing suppressed, and nothing but suppressions.
func TestSuppressionRatio(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		result *mutate.Result
		want   float64
		wantOK bool
	}{
		"an empty run has no ratio": {
			result: &mutate.Result{},
		},
		"nothing suppressed": {
			result: &mutate.Result{Mutants: []mutate.MutantResult{{Status: mutate.Killed}, {Status: mutate.Survived}}},
			wantOK: true,
		},
		"one in four": {
			result: &mutate.Result{
				Mutants:      []mutate.MutantResult{{Status: mutate.Killed}, {Status: mutate.Killed}, {Status: mutate.Survived}},
				Suppressions: []mutate.SuppressionResult{{Line: 1}},
			},
			want:   0.25,
			wantOK: true,
		},
		"not-viable mutants are outside the ratio": {
			result: &mutate.Result{
				Mutants:      []mutate.MutantResult{{Status: mutate.Killed}, {Status: mutate.NotViable}, {Status: mutate.NotViable}},
				Suppressions: []mutate.SuppressionResult{{Line: 1}},
			},
			want:   0.5,
			wantOK: true,
		},
		"suppressions with no mutants at all": {
			result: &mutate.Result{Suppressions: []mutate.SuppressionResult{{Line: 1}, {Line: 2}}},
			want:   1,
			wantOK: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.result.SuppressionRatio()
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("SuppressionRatio() = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestSuppressionsStayOutOfTheScore pins the accounting rule: a suppressed node
// never became a mutant, so it must not appear in the counts or move the score
// in either direction.
func TestSuppressionsStayOutOfTheScore(t *testing.T) {
	t.Parallel()

	r := &mutate.Result{
		Mutants: []mutate.MutantResult{
			{Status: mutate.Killed},
			{Status: mutate.Survived},
		},
		Suppressions: []mutate.SuppressionResult{
			{File: "a.go", Line: 3},
			{File: "a.go", Line: 9, Reason: "generated"},
		},
	}

	killed, survived, notViable := r.Counts()
	if killed != 1 || survived != 1 || notViable != 0 {
		t.Errorf("Counts() = %d, %d, %d; want 1, 1, 0", killed, survived, notViable)
	}

	score, ok := r.Score()
	if !ok || score != 0.5 {
		t.Errorf("Score() = %v, %v; want 0.5, true", score, ok)
	}

	if got := r.SuppressedCount(); got != 2 {
		t.Errorf("SuppressedCount() = %d, want 2", got)
	}
}

// TestEquivalentsStayOutOfTheScore pins the same accounting rule
// TestSuppressionsStayOutOfTheScore does, for TCE: a mutation Trivial
// Compiler Equivalence filtered never became a mutant, so it must not
// appear in the counts or move the score in either direction.
func TestEquivalentsStayOutOfTheScore(t *testing.T) {
	t.Parallel()

	r := &mutate.Result{
		Mutants: []mutate.MutantResult{
			{Status: mutate.Killed},
			{Status: mutate.Survived},
		},
		Equivalents: []mutate.EquivalentResult{
			{File: "a.go", Line: 3, Operator: "statement/remover", Description: "remove statement: dead = 1"},
			{File: "a.go", Line: 9, Operator: "literal/number", Description: "1 -> 2"},
		},
	}

	killed, survived, notViable := r.Counts()
	if killed != 1 || survived != 1 || notViable != 0 {
		t.Errorf("Counts() = %d, %d, %d; want 1, 1, 0", killed, survived, notViable)
	}

	score, ok := r.Score()
	if !ok || score != 0.5 {
		t.Errorf("Score() = %v, %v; want 0.5, true", score, ok)
	}

	if got := r.EquivalentCount(); got != 2 {
		t.Errorf("EquivalentCount() = %d, want 2", got)
	}
}

// TestRelativize covers the paths a published report carries, including the ones
// that must be left alone.
func TestRelativize(t *testing.T) {
	t.Parallel()

	base := filepath.Join(string(filepath.Separator), "src", "mod")
	inside := filepath.Join(base, "pkg", "a.go")
	outside := filepath.Join(string(filepath.Separator), "elsewhere", "b.go")

	original := &mutate.Result{
		Mutants:      []mutate.MutantResult{{File: inside}, {File: outside}, {File: "already/relative.go"}},
		Suppressions: []mutate.SuppressionResult{{File: inside, Line: 4}},
		Equivalents:  []mutate.EquivalentResult{{File: inside, Line: 7}},
	}

	got := original.Relativize(base)

	wantMutants := []string{
		filepath.Join("pkg", "a.go"),
		filepath.Join("..", "..", "elsewhere", "b.go"),
		"already/relative.go",
	}

	for i, want := range wantMutants {
		if got.Mutants[i].File != want {
			t.Errorf("Mutants[%d].File = %q, want %q", i, got.Mutants[i].File, want)
		}
	}

	if got.Suppressions[0].File != filepath.Join("pkg", "a.go") {
		t.Errorf("Suppressions[0].File = %q, want the relative path", got.Suppressions[0].File)
	}

	if got.Equivalents[0].File != filepath.Join("pkg", "a.go") {
		t.Errorf("Equivalents[0].File = %q, want the relative path", got.Equivalents[0].File)
	}

	// The engine's own copy must be untouched: it is still being used to run
	// mutants when a partial report is flushed.
	if original.Mutants[0].File != inside || original.Suppressions[0].File != inside || original.Equivalents[0].File != inside {
		t.Errorf("Relativize modified the receiver: %+v", original)
	}

	// An empty base is the "working directory unavailable" path and must leave
	// every path exactly as it was.
	if untouched := original.Relativize(""); untouched.Mutants[0].File != inside {
		t.Errorf("Relativize(\"\") = %q, want the absolute path", untouched.Mutants[0].File)
	}
}

// TestWriteSummary covers the console report: the counts block, the score, the
// suppression ratio, and the survivor list — which is the only part a user is
// expected to act on.
func TestWriteSummary(t *testing.T) {
	t.Parallel()

	base := filepath.Join(string(filepath.Separator), "src", "mod")

	result := &mutate.Result{
		Mutants: []mutate.MutantResult{
			{File: filepath.Join(base, "a.go"), Line: 7, Operator: "operator/binary", Description: "== -> !=", Status: mutate.Killed},
			{File: filepath.Join(base, "a.go"), Line: 9, Operator: "control/if", Description: "remove if body", Status: mutate.Survived, Before: "{ thing() }", After: "{}"},
			{File: filepath.Join(base, "pkg", "b.go"), Line: 42, Operator: "statement/remover", Description: "remove statement: x++", Status: mutate.Survived},
			{File: filepath.Join(base, "pkg", "b.go"), Line: 50, Operator: "operator/unary", Description: "strip !", Status: mutate.NotViable},
		},
		Suppressions: []mutate.SuppressionResult{{File: filepath.Join(base, "a.go"), Line: 3, Reason: "generated"}},
		Equivalents:  []mutate.EquivalentResult{{File: filepath.Join(base, "a.go"), Line: 5, Operator: "statement/remover", Description: "remove statement: dead = 1"}},
	}

	var buf bytes.Buffer

	result.WriteSummary(&buf, base)

	got := buf.String()

	for _, want := range []string{
		"mutants:    4",
		"killed:     1",
		"survived:   2",
		"not-viable: 1",
		"score:      33.3% (1 killed of 3 viable)",
		"suppressed: 1 of 4 nodes (25.0%",
		"equivalent: 1 (filtered by TCE before reaching the test suite)",
		"Surviving mutants (2):",
		"a.go:9",
		filepath.Join("pkg", "b.go") + ":42",
		"remove statement: x++",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}

	// A killed or not-viable mutant needs no follow-up, so neither is listed.
	if strings.Contains(got, "== -> !=") || strings.Contains(got, "strip !") {
		t.Errorf("summary lists a mutant that is not actionable:\n%s", got)
	}

	// Paths are relativised against base, never printed raw.
	if strings.Contains(got, base+string(filepath.Separator)) {
		t.Errorf("summary contains an absolute path:\n%s", got)
	}

	// 3c's decision: the console table stays Description-only. Before/After
	// exist for the JSON report (LLM-prompt-paste, proposal drafting), not
	// for a table whose own doc comment cites "ragged left edge" as the
	// thing it exists to avoid.
	if strings.Contains(got, "{ thing() }") {
		t.Errorf("summary leaked Before text into the console table:\n%s", got)
	}
}

// TestWriteSummaryEmpty covers the two degenerate runs: nothing scored, and
// nothing survived.
func TestWriteSummaryEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	(&mutate.Result{}).WriteSummary(&buf, "")

	got := buf.String()

	if !strings.Contains(got, "n/a (no viable mutants)") {
		t.Errorf("summary = %q, want an unscored run to say so", got)
	}

	if !strings.Contains(got, "suppressed: 0") {
		t.Errorf("summary = %q, want a suppression line", got)
	}

	// Unlike the suppression line, which always prints (even "suppressed:
	// 0"), the equivalent line is omitted entirely when TCE found nothing —
	// a fixed part of the summary's shape would be noise on every run that
	// doesn't use TCE at all.
	if strings.Contains(got, "equivalent:") {
		t.Errorf("summary = %q, want no equivalent line when nothing was filtered", got)
	}

	if strings.Contains(got, "Surviving mutants") {
		t.Errorf("summary = %q, want no survivor list when nothing survived", got)
	}

	buf.Reset()
	(&mutate.Result{Mutants: []mutate.MutantResult{{Status: mutate.Killed}}}).WriteSummary(&buf, "")

	if strings.Contains(buf.String(), "Surviving mutants") {
		t.Errorf("summary = %q, want no survivor list on a clean run", buf.String())
	}
}

// TestWriteEstimate covers the estimate console printer end to end: the
// total, the per-package breakdown, both time predictions labeled
// distinctly (ROADMAP.md gap 11c's "never a single confident number"
// requirement), and the TCE caveat's conditional presence (gap 11d).
func TestWriteEstimate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		result  mutate.EstimateResult
		want    []string
		wantNot []string
	}{
		"multi-package estimate": {
			result: mutate.EstimateResult{
				Total: 7,
				Packages: []mutate.PackageEstimate{
					{Package: "example.com/fixture/app", Mutants: 2, Baseline: 500 * time.Millisecond},
					{Package: "example.com/fixture/mathx", Mutants: 5, Baseline: 750 * time.Millisecond},
				},
				SerialEstimate:   4750 * time.Millisecond,
				Workers:          4,
				ParallelEstimate: 1187500 * time.Microsecond,
			},
			want: []string{
				"estimated mutants: 7",
				"example.com/fixture/app",
				"2 mutants",
				"example.com/fixture/mathx",
				"5 mutants",
				"serial estimate",
				"parallel estimate",
				"/4 workers",
				"rough numbers",
				"sub-linear under CPU/GOCACHE contention",
			},
			// TCE is off (the zero value) in this case: no caveat about it
			// should print at all.
			wantNot: []string{"mutatetce"},
		},
		"TCE requested surfaces its own caveat": {
			result: mutate.EstimateResult{
				Total:   1,
				TCE:     true,
				Workers: 1,
			},
			want: []string{"mutatetce=true is set", "may filter some and finish faster"},
		},
		"zero mutants lists no packages": {
			result:  mutate.EstimateResult{Workers: 1},
			want:    []string{"estimated mutants: 0"},
			wantNot: []string{"mutants  ~"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			tt.result.WriteEstimate(&buf)

			got := buf.String()

			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("WriteEstimate() missing %q:\n%s", want, got)
				}
			}

			for _, notWant := range tt.wantNot {
				if strings.Contains(got, notWant) {
					t.Errorf("WriteEstimate() unexpectedly contains %q:\n%s", notWant, got)
				}
			}
		})
	}
}
