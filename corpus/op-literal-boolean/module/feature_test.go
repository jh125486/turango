package opliteralboolean

import "testing"

// TestUnrelated deliberately never calls FeatureEnabled: zero coverage
// means every mutation literal/boolean can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
