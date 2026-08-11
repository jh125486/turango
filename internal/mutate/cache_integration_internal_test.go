//go:build integration

// Integration + whitebox: resolveToolchain shells out to the real Go
// toolchain (`go version`), so it needs the "integration" build tag (see
// engine_integration_test.go's header), and it needs unexported access, so
// it's whitebox — mirroring exactly how runner_integration_internal_test.go
// splits compileDisassembly/isTCEEquivalent out of runner_internal_test.go
// for the identical reason.
package mutate

import (
	"strings"
	"testing"
)

// TestResolveToolchain covers both the real-binary success path and the
// unusable-binary failure path: a real "go" produces a non-empty string
// naming the resolved GOOS/GOARCH, and a nonexistent binary is a real
// error rather than a silently empty toolchain identifier — which would
// make every mutant in a run key its cache entry identically regardless of
// which toolchain actually produced the verdict.
func TestResolveToolchain(t *testing.T) {
	t.Parallel()

	got, err := resolveToolchain(t.Context(), "go")
	if err != nil {
		t.Fatalf("resolveToolchain() error = %v", err)
	}

	if got == "" {
		t.Fatal("resolveToolchain() returned an empty string")
	}

	if !strings.Contains(got, "go version") {
		t.Errorf("resolveToolchain() = %q, want it to contain go version's own output", got)
	}

	if _, err := resolveToolchain(t.Context(), "/nonexistent/go"); err == nil {
		t.Error("resolveToolchain() error = nil for a nonexistent go binary, want an error")
	}
}
