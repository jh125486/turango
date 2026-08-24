// Whitebox test: foo and bar are unexported, so a blackbox
// (package legacy_test) test cannot reach them at all — whitebox is the
// only option here, per rule 6 of the testing convention.
//
// TestFoo's single coarse assertion on foo()'s final return value is
// deliberate, not an oversight: this package exists to demonstrate what
// mutation testing catches that a single overall-result assertion misses.
// See legacy.go's package doc comment, corpus/example-legacy/golden.json,
// and example/README.md. Do not add per-branch assertions, table cases, or
// otherwise strengthen this test — that would change which of the 38
// recorded mutants survive.
package legacy

import "testing"

func TestFoo(t *testing.T) {
	t.Parallel()

	if got := foo(); got != 16 {
		t.Errorf("foo() = %d, want 16", got)
	}
}
