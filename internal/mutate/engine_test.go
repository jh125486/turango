package mutate_test

import (
	"os"
	"path/filepath"
	"strings"
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

func TestParseWorkspace(t *testing.T) {
	t.Parallel()

	for _, workspace := range []mutate.Workspace{mutate.WorkspaceCopy, mutate.WorkspaceWorktree} {
		got, err := mutate.ParseWorkspace(workspace.String())
		if err != nil {
			t.Fatalf("ParseWorkspace(%q) error = %v", workspace, err)
		}

		if got != workspace {
			t.Errorf("ParseWorkspace(%q) = %v, want %v", workspace.String(), got, workspace)
		}
	}

	if _, err := mutate.ParseWorkspace("Worktree"); err == nil {
		t.Error("ParseWorkspace(\"Worktree\") error = nil, want an error: the spellings are lower case")
	}

	if _, err := mutate.ParseWorkspace(""); err == nil {
		t.Error("ParseWorkspace(\"\") error = nil, want an error")
	}
}

// TestScopeStringUnknown covers Scope.String()'s fallback spelling for a
// value outside [ScopeFull]/[ScopePackage]/[ScopeImpact]'s defined range —
// reachable only via an explicit invalid conversion like this one, never
// through [mutate.ParseScope] or the classifier (see unknownSpelling's own
// doc comment in engine.go).
func TestScopeStringUnknown(t *testing.T) {
	t.Parallel()

	if got, want := mutate.Scope(99).String(), "unknown"; got != want {
		t.Errorf("Scope(99).String() = %q, want %q", got, want)
	}
}

// TestWorkspaceStringUnknown is [TestScopeStringUnknown]'s counterpart for
// [mutate.Workspace].
func TestWorkspaceStringUnknown(t *testing.T) {
	t.Parallel()

	if got, want := mutate.Workspace(99).String(), "unknown"; got != want {
		t.Errorf("Workspace(99).String() = %q, want %q", got, want)
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

// TestRunRejectsInvalidOptions covers Run's early validation steps that fail
// before any package is ever loaded: an unknown operator name and an
// unparseable FuncPattern regexp. Both are cheap to check — no fixture
// module or toolchain work is needed, since Run fails before load() is ever
// reached — so this stays out of the integration-tagged file despite being
// exercised through the exported entry point.
func TestRunRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		opts       mutate.Options
		wantSubstr string
	}{
		"unknown operator": {
			opts:       mutate.Options{Operators: []string{"no/such/operator"}},
			wantSubstr: "no/such/operator",
		},
		"invalid FuncPattern": {
			opts:       mutate.Options{FuncPattern: "("},
			wantSubstr: "func pattern",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := mutate.Run(t.Context(), tt.opts)
			if err == nil {
				t.Fatal("Run() error = nil, want an error")
			}

			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("Run() error = %v, want it to contain %q", err, tt.wantSubstr)
			}
		})
	}
}

// TestEstimateRejectsInvalidOptions is [TestRunRejectsInvalidOptions]'s
// counterpart for Estimate: the same early-validation failures (an unknown
// operator, an unparseable FuncPattern) plus an unmatched package pattern,
// which Estimate's own load() step must reject exactly as Run's does.
func TestEstimateRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	root := fixtureModule(t)

	tests := map[string]struct {
		opts       mutate.Options
		wantSubstr string
	}{
		"unknown operator": {
			opts:       mutate.Options{Operators: []string{"no/such/operator"}},
			wantSubstr: "no/such/operator",
		},
		"invalid FuncPattern": {
			opts:       mutate.Options{FuncPattern: "("},
			wantSubstr: "func pattern",
		},
		"unmatched package pattern": {
			opts:       mutate.Options{Packages: []string{"./does-not-exist/..."}, Dir: root},
			wantSubstr: "does-not-exist",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := mutate.Estimate(t.Context(), tt.opts)
			if err == nil {
				t.Fatal("Estimate() error = nil, want an error")
			}

			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("Estimate() error = %v, want it to contain %q", err, tt.wantSubstr)
			}
		})
	}
}
