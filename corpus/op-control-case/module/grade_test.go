package opcontrolcase

import "testing"

// TestUnrelated deliberately never calls Grade: zero coverage means every
// mutation control/case can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
