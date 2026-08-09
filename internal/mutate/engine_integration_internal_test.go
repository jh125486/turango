//go:build integration

// Integration + whitebox: this test needs both the "integration" build tag
// (it shells out to the real Go toolchain, see engine_integration_test.go's
// header) and unexported access (the loadTypedCalls counter has no exported
// equivalent). fixtureModule comes from engine_internal_test.go: that file
// has no build tag, so it always compiles alongside this one, and the two
// share one package (mutate) — redeclaring the helper here would collide
// with it rather than being shadowed, the way the blackbox/whitebox split
// allows elsewhere.
package mutate

import (
	"testing"
	"time"
)

// TestRunWithoutConstSwapNeverLoadsTypes is the concrete regression test for
// the "a run that does not select a TypedMutator operator pays nothing for
// type information" claim: it asserts loadTyped is never invoked during the
// run, rather than only inferring the claim from the run's output looking
// otherwise normal.
//
// It deliberately does not call t.Parallel(). loadTypedCalls is a
// process-wide counter shared by every test in this compiled binary, which
// would otherwise inflate the counter for reasons having nothing to do with
// this test. Go's test runner runs every non-parallel top-level test to
// completion before any t.Parallel() test's body resumes past its own
// Parallel() call, so running this one non-parallel — and first, textually,
// though order is not what makes this safe — is what makes the delta
// assertion reliable rather than a race against its siblings.
func TestRunWithoutConstSwapNeverLoadsTypes(t *testing.T) {
	root := fixtureModule(t)

	before := loadTypedCalls.Load()

	_, err := Run(t.Context(), Options{
		Packages:    []string{"./..."},
		Dir:         root,
		Operators:   []string{"operator/binary"},
		Scope:       ScopePackage,
		TestTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if after := loadTypedCalls.Load(); after != before {
		t.Errorf("loadTyped was called %d time(s) during a run selecting no TypedMutator operator, want 0", after-before)
	}
}
