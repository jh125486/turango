package opcontrolelse

import "testing"

// TestUnrelated deliberately never calls Sign: zero coverage means every
// mutation control/else can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
