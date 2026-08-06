package mutate

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusJSON pins the wire form of a status: the names, not the iota. A
// report that encoded 0/1/2 would be unreadable without a copy of report.go and
// would silently change meaning the day a status is inserted in the middle of
// the list.
func TestStatusJSON(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status Status
		want   string
	}{
		"killed":     {status: Killed, want: `"killed"`},
		"survived":   {status: Survived, want: `"survived"`},
		"not viable": {status: NotViable, want: `"not-viable"`},
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

			var got Status
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

			var got Status
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

	want := &Result{
		Mutants: []MutantResult{
			{File: "a.go", Line: 3, Operator: "operator/binary", Description: "== -> !=", Status: Survived},
			{File: "b.go", Line: 9, Operator: "control/if", Description: "remove if body", Status: NotViable},
		},
		Suppressions: []SuppressionResult{{File: "a.go", Line: 12, Reason: "flaky"}},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if !strings.Contains(string(data), `"Status":"survived"`) {
		t.Errorf("report = %s, want the status spelled out", data)
	}

	var got Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if len(got.Mutants) != len(want.Mutants) || got.Mutants[0].Status != Survived || got.Mutants[1].Status != NotViable {
		t.Errorf("round trip = %+v, want %+v", got.Mutants, want.Mutants)
	}

	if len(got.Suppressions) != 1 || got.Suppressions[0].Reason != "flaky" {
		t.Errorf("round trip dropped suppressions: %+v", got.Suppressions)
	}
}

// TestSuppressionRatio covers the number that keeps //nomutant honest,
// including the two ends: nothing suppressed, and nothing but suppressions.
func TestSuppressionRatio(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		result *Result
		want   float64
		wantOK bool
	}{
		"an empty run has no ratio": {
			result: &Result{},
		},
		"nothing suppressed": {
			result: &Result{Mutants: []MutantResult{{Status: Killed}, {Status: Survived}}},
			wantOK: true,
		},
		"one in four": {
			result: &Result{
				Mutants:      []MutantResult{{Status: Killed}, {Status: Killed}, {Status: Survived}},
				Suppressions: []SuppressionResult{{Line: 1}},
			},
			want:   0.25,
			wantOK: true,
		},
		"not-viable mutants are outside the ratio": {
			result: &Result{
				Mutants:      []MutantResult{{Status: Killed}, {Status: NotViable}, {Status: NotViable}},
				Suppressions: []SuppressionResult{{Line: 1}},
			},
			want:   0.5,
			wantOK: true,
		},
		"suppressions with no mutants at all": {
			result: &Result{Suppressions: []SuppressionResult{{Line: 1}, {Line: 2}}},
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

// TestRelativize covers the paths a published report carries, including the ones
// that must be left alone.
func TestRelativize(t *testing.T) {
	t.Parallel()

	base := filepath.Join(string(filepath.Separator), "src", "mod")
	inside := filepath.Join(base, "pkg", "a.go")
	outside := filepath.Join(string(filepath.Separator), "elsewhere", "b.go")

	original := &Result{
		Mutants:      []MutantResult{{File: inside}, {File: outside}, {File: "already/relative.go"}},
		Suppressions: []SuppressionResult{{File: inside, Line: 4}},
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

	// The engine's own copy must be untouched: it is still being used to run
	// mutants when a partial report is flushed.
	if original.Mutants[0].File != inside || original.Suppressions[0].File != inside {
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

	result := &Result{
		Mutants: []MutantResult{
			{File: filepath.Join(base, "a.go"), Line: 7, Operator: "operator/binary", Description: "== -> !=", Status: Killed},
			{File: filepath.Join(base, "a.go"), Line: 9, Operator: "control/if", Description: "remove if body", Status: Survived},
			{File: filepath.Join(base, "pkg", "b.go"), Line: 42, Operator: "statement/remover", Description: "remove statement: x++", Status: Survived},
			{File: filepath.Join(base, "pkg", "b.go"), Line: 50, Operator: "operator/unary", Description: "strip !", Status: NotViable},
		},
		Suppressions: []SuppressionResult{{File: filepath.Join(base, "a.go"), Line: 3, Reason: "generated"}},
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
}

// TestWriteSummaryEmpty covers the two degenerate runs: nothing scored, and
// nothing survived.
func TestWriteSummaryEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	(&Result{}).WriteSummary(&buf, "")

	got := buf.String()

	if !strings.Contains(got, "n/a (no viable mutants)") {
		t.Errorf("summary = %q, want an unscored run to say so", got)
	}

	if !strings.Contains(got, "suppressed: 0") {
		t.Errorf("summary = %q, want a suppression line", got)
	}

	if strings.Contains(got, "Surviving mutants") {
		t.Errorf("summary = %q, want no survivor list when nothing survived", got)
	}

	buf.Reset()
	(&Result{Mutants: []MutantResult{{Status: Killed}}}).WriteSummary(&buf, "")

	if strings.Contains(buf.String(), "Surviving mutants") {
		t.Errorf("summary = %q, want no survivor list on a clean run", buf.String())
	}
}
