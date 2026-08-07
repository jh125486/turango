package opoperatorincdec

import "testing"

// TestUnrelated deliberately never calls Next: zero coverage means every
// mutation operator/inc_dec can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
