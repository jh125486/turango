package legacy

import "testing"

func TestFoo(t *testing.T) {
	if got := foo(); got != 16 {
		t.Errorf("foo() = %d, want 16", got)
	}
}
