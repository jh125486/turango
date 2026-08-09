package mutate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jh125486/turango/internal/mutate"
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

		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	return root
}

func TestParseScope(t *testing.T) {
	t.Parallel()

	for _, scope := range []mutate.Scope{mutate.ScopeFull, mutate.ScopePackage, mutate.ScopeImpact} {
		got, err := mutate.ParseScope(scope.String())
		if err != nil {
			t.Fatalf("ParseScope(%q) error = %v", scope, err)
		}

		if got != scope {
			t.Errorf("ParseScope(%q) = %v, want %v", scope.String(), got, scope)
		}
	}

	if _, err := mutate.ParseScope("Package"); err == nil {
		t.Error("ParseScope(\"Package\") error = nil, want an error: the spellings are lower case")
	}

	if _, err := mutate.ParseScope(""); err == nil {
		t.Error("ParseScope(\"\") error = nil, want an error")
	}
}

// TestRunRejectsUnknownPackages checks that a bad pattern is an error rather
// than an empty, apparently-successful run.
func TestRunRejectsUnknownPackages(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	if _, err := mutate.Run(t.Context(), mutate.Options{
		Packages: []string{"./does-not-exist/..."},
		Dir:      root,
	}); err == nil {
		t.Fatal("Run() error = nil, want an error for an unmatched pattern")
	}
}
