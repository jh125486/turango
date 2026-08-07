package opsuppressnomutant

import "testing"

// TestUnrelated deliberately never calls Guarded: the //nomutant directive
// is what's under test here, not the function's own coverage.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
