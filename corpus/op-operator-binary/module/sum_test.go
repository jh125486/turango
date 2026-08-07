package opoperatorbinary

import "testing"

// TestUnrelated deliberately never calls Sum: zero coverage means every
// mutation operator/binary can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
