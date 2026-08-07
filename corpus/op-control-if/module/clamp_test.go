package opcontrolif

import "testing"

// TestUnrelated deliberately never calls Clamp: the whole point of this
// fixture is a function with zero test coverage, so every mutation
// control/if can produce on it survives, unambiguously.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
