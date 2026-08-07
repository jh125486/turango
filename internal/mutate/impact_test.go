package mutate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeProfile writes a coverage profile and returns its path.
func writeProfile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	return path
}

// TestImpactMapMerge is the heart of impact scope tested without a toolchain:
// real `go test -coverprofile` output, folded into the line-to-tests map the
// engine consults per mutant.
//
// The profile below is the standard format — importpath/file.go:
// startLine.startCol,endLine.endCol numStmt count — with one executed block
// (count 1) and one that compiled but never ran (count 0).
func TestImpactMapMerge(t *testing.T) {
	t.Parallel()

	goFiles := []string{
		filepath.FromSlash("/abs/mathx/mathx.go"),
		filepath.FromSlash("/abs/mathx/other.go"),
	}

	clamp := writeProfile(t, `mode: set
example.com/fixture/mathx/mathx.go:4.32,5.13 1 1
example.com/fixture/mathx/mathx.go:8.20,10.2 1 0
example.com/fixture/other/elsewhere.go:1.1,2.2 1 1
`)

	describe := writeProfile(t, `mode: set
example.com/fixture/mathx/mathx.go:5.13,5.20 1 1
example.com/fixture/mathx/other.go:3.1,4.2 1 1
`)

	m := &impactMap{lines: make(map[string]map[int][]string)}

	if err := m.merge(clamp, "TestClamp", goFiles); err != nil {
		t.Fatalf("merge() error = %v", err)
	}

	if err := m.merge(describe, "TestDescribe", goFiles); err != nil {
		t.Fatalf("merge() error = %v", err)
	}

	mathx := goFiles[0]

	tests := map[string]struct {
		file string
		line int
		want []string
	}{
		"executed block, first line":    {file: mathx, line: 4, want: []string{"TestClamp"}},
		"line covered by two tests":     {file: mathx, line: 5, want: []string{"TestClamp", "TestDescribe"}},
		"block that never ran":          {file: mathx, line: 9, want: nil},
		"line no block mentions":        {file: mathx, line: 99, want: nil},
		"second file of the package":    {file: goFiles[1], line: 3, want: []string{"TestDescribe"}},
		"file outside the package":      {file: filepath.FromSlash("/abs/other/elsewhere.go"), line: 1, want: nil},
		"unknown file, unknown package": {file: "nope.go", line: 1, want: nil},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := m.covering(tt.file, tt.line); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("covering(%s, %d) = %v, want %v", tt.file, tt.line, got, tt.want)
			}
		})
	}
}

// TestImpactMapCoveringNil pins the fallback contract: jobs hold a *impactMap
// unconditionally and it is nil for every scope but impact.
func TestImpactMapCoveringNil(t *testing.T) {
	t.Parallel()

	var m *impactMap

	if got := m.covering("anything.go", 1); got != nil {
		t.Errorf("(*impactMap)(nil).covering() = %v, want nil", got)
	}
}

// TestImpactMapMergeDeduplicates covers the one way a test name can repeat for a
// line: two blocks of the same test overlapping the same source line.
func TestImpactMapMergeDeduplicates(t *testing.T) {
	t.Parallel()

	goFiles := []string{filepath.FromSlash("/abs/p/p.go")}

	profile := writeProfile(t, `mode: set
example.com/p/p.go:3.1,6.2 1 4
example.com/p/p.go:4.1,5.2 1 2
`)

	m := &impactMap{lines: make(map[string]map[int][]string)}
	if err := m.merge(profile, "TestOnce", goFiles); err != nil {
		t.Fatalf("merge() error = %v", err)
	}

	if got := m.covering(goFiles[0], 4); !reflect.DeepEqual(got, []string{"TestOnce"}) {
		t.Errorf("covering() = %v, want one entry", got)
	}
}

func TestImpactMapMergeRejectsGarbage(t *testing.T) {
	t.Parallel()

	m := &impactMap{lines: make(map[string]map[int][]string)}

	err := m.merge(writeProfile(t, "this is not a coverage profile\n"), "TestX", []string{"/abs/p/p.go"})
	if err == nil {
		t.Fatal("merge() error = nil, want an error for an unparseable profile")
	}
}

// TestParseTestList covers the filtering of `go test -list` output: names only,
// and only names -run can actually start.
func TestParseTestList(t *testing.T) {
	t.Parallel()

	const out = `TestClamp
TestDescribe
BenchmarkClamp
FuzzParse
ExampleClamp
TestMain
ok  	example.com/fixture/mathx	0.198s
`

	want := []string{"TestClamp", "TestDescribe", "FuzzParse", "ExampleClamp", "TestMain"}

	if got := parseTestList(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseTestList() = %v, want %v", got, want)
	}

	if got := parseTestList(""); got != nil {
		t.Errorf("parseTestList(\"\") = %v, want nil", got)
	}
}

func TestMatchProfileFile(t *testing.T) {
	t.Parallel()

	goFiles := []string{
		filepath.FromSlash("/abs/mathx/mathx.go"),
		filepath.FromSlash("/abs/mathx/clamp.go"),
	}

	got, ok := matchProfileFile("example.com/fixture/mathx/clamp.go", goFiles)
	if !ok || got != goFiles[1] {
		t.Errorf("matchProfileFile() = %q, %v; want %q, true", got, ok, goFiles[1])
	}

	if _, ok := matchProfileFile("example.com/other/nope.go", goFiles); ok {
		t.Error("matchProfileFile() matched a file outside the package")
	}
}
