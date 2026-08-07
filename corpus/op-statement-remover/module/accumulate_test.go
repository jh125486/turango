package opstatementremover

import "testing"

// TestUnrelated deliberately never calls Accumulate: zero coverage means
// every mutation statement/remover can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
