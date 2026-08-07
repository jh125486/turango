package opexpressionremove

import "testing"

// TestUnrelated deliberately never calls InRange: zero coverage means every
// mutation expression/remove can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
