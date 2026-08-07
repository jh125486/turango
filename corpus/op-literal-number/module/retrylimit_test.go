package opliteralnumber

import "testing"

// TestUnrelated deliberately never calls TooManyRetries: zero coverage
// means every mutation literal/number can produce on it survives.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
