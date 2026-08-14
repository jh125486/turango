//go:build integration

// Integration + whitebox: buildImpact shells out to the real Go toolchain
// (`go test -list`, `go test -coverprofile`) for every branch covered below,
// so each needs the "integration" build tag (see engine_integration_test.go's
// header) and needs unexported access, so it's whitebox — mirroring exactly
// how cache_integration_internal_test.go splits resolveToolchain out of
// cache_internal_test.go for the identical reason. buildImpact's
// toolchain-free error returns (a malformed (moduleDir, pkgDir) pair, a
// nonexistent goBin) are covered without the tag by TestBuildImpactEarlyErrors
// in impact_internal_test.go.
package mutate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// impactBuildFixture writes a minimal, real module — go.mod plus one package
// file, and an optional test file — under a fresh temp directory and returns
// its root, ready for buildImpact to run real `go test` commands against. An
// empty testBody means "no test file at all", the fixture
// TestBuildImpactNoTests needs.
func impactBuildFixture(t *testing.T, testBody string) string {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/impactfixture\n\ngo 1.23\n",
		filepath.Join(root, "p.go"):   "package p\n\n// Add adds two ints.\nfunc Add(a, b int) int { return a + b }\n",
	}

	if testBody != "" {
		files[filepath.Join(root, "p_test.go")] = testBody
	}

	writeFiles(t, files)

	return root
}

// TestBuildImpactNoTests covers the len(tests) == 0 early return: a package
// with no test functions is a complete, zero-cost measurement ("nothing
// covers anything"), not a failure, and buildImpact returns without ever
// creating a coverage workspace.
func TestBuildImpactNoTests(t *testing.T) {
	t.Parallel()

	root := impactBuildFixture(t, "")

	m, err := buildImpact(t.Context(), "go", root, root, []string{filepath.Join(root, "p.go")})
	if err != nil {
		t.Fatalf("buildImpact() error = %v", err)
	}

	if len(m.lines) != 0 {
		t.Errorf("buildImpact() on a test-less package = %+v, want an empty map", m.lines)
	}
}

// TestBuildImpactCoverageRunFails covers the real go test invocation itself
// failing: a genuinely failing test makes `go test -coverprofile` exit
// non-zero, which cmd.CombinedOutput surfaces as an error buildImpact wraps
// and returns rather than silently swallowing.
func TestBuildImpactCoverageRunFails(t *testing.T) {
	t.Parallel()

	root := impactBuildFixture(t, `package p

import "testing"

func TestBoom(t *testing.T) {
	t.Fatal("boom")
}
`)

	_, err := buildImpact(t.Context(), "go", root, root, []string{filepath.Join(root, "p.go")})
	if err == nil || !strings.Contains(err.Error(), "measuring coverage of") {
		t.Errorf("buildImpact() error = %v, want it to contain %q", err, "measuring coverage of")
	}
}

// TestBuildImpactMkdirTempError covers the os.MkdirTemp failure path: with
// at least one real test to run (so buildImpact gets past the len(tests) ==
// 0 short-circuit), a TMPDIR pointed at a directory that does not exist
// makes the coverage workspace itself unable to be created.
//
// GOTMPDIR is set to a real, valid directory alongside the broken TMPDIR:
// the go toolchain's own work-dir creation (inside listTests' `go test
// -list`, which must succeed first) honours GOTMPDIR ahead of TMPDIR, while
// plain os.MkdirTemp — what buildImpact itself calls for the coverage
// workspace — only ever consults TMPDIR. Without this split, `go test
// -list` fails on the broken TMPDIR before buildImpact's own MkdirTemp call
// is ever reached.
//
// Not t.Parallel(): TMPDIR/GOTMPDIR are process-global state, and t.Setenv
// forbids combining the two.
func TestBuildImpactMkdirTempError(t *testing.T) {
	root := impactBuildFixture(t, `package p

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("bad")
	}
}
`)

	t.Setenv("GOTMPDIR", t.TempDir())
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := buildImpact(t.Context(), "go", root, root, []string{filepath.Join(root, "p.go")})
	if err == nil || !strings.Contains(err.Error(), "creating coverage workspace") {
		t.Errorf("buildImpact() error = %v, want it to contain %q", err, "creating coverage workspace")
	}
}

// impactCancelAfterFirstErrContext reports ctx.Err() as nil the first time
// buildImpact's per-iteration loop asks, then cancels the underlying context
// and returns its now-real error on every ask after — deterministically
// exercising buildImpact's mid-loop ctx.Err() early exit between test
// iterations without racing real wall-clock timing against a subprocess.
// This works because ctx is an interface buildImpact merely calls Err() on;
// cache.go's consume/newCacheStore counterparts take a concrete *os.File
// instead, which is why their error-injection tests in
// cache_internal_test.go use real files/pipes rather than a wrapper type.
type impactCancelAfterFirstErrContext struct {
	context.Context

	cancel context.CancelFunc
	asked  int
}

func (c *impactCancelAfterFirstErrContext) Err() error {
	c.asked++

	if c.asked > 1 {
		c.cancel()
	}

	return c.Context.Err()
}

// TestBuildImpactContextCancelledMidLoop covers the ctx.Err() check at the
// top of buildImpact's per-test loop: with two real, fast, passing tests,
// the first iteration's real `go test` run completes normally, and the
// second iteration's ctx.Err() check — now reporting Canceled — exits before
// starting a second real subprocess.
func TestBuildImpactContextCancelledMidLoop(t *testing.T) {
	t.Parallel()

	root := impactBuildFixture(t, `package p

import "testing"

func TestA(t *testing.T) {}

func TestB(t *testing.T) {}
`)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wrapped := &impactCancelAfterFirstErrContext{Context: ctx, cancel: cancel}

	if _, err := buildImpact(wrapped, "go", root, root, []string{filepath.Join(root, "p.go")}); err == nil {
		t.Fatal("buildImpact() error = nil, want the injected context cancellation to surface")
	}

	if got := wrapped.Context.Err(); got == nil {
		t.Error("underlying context.Err() = nil after buildImpact returned, want it cancelled")
	}
}
