package optceequivalent

import "testing"

// TestUnrelated deliberately never calls DeadStore: zero coverage means
// every mutation statement/remover can produce on it survives when TCE is
// off, or gets filtered before ever reaching a test run when TCE is on --
// this fixture exists to demonstrate the latter.
func TestUnrelated(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
