package opoperatorassignment

import "testing"

// TestUnrelated deliberately never calls Double: zero coverage means every
// mutation operator/assignment can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
