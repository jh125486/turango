package opoperatorboundary

import "testing"

// TestUnrelated deliberately never calls BelowLimit: zero coverage means
// every mutation operator/boundary can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
