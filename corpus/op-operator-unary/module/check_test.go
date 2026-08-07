package opoperatorunary

import "testing"

// TestUnrelated deliberately never calls Check: zero coverage means every
// mutation operator/unary can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
