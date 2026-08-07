// Package opcontrolif is a minimal, hand-built integration fixture for the
// control/if mutator: smallest possible example of turango catching a real
// gap, not extracted from any external source.
package opcontrolif

// Clamp floors x at zero.
func Clamp(x int) int {
	if x < 0 {
		x = 0
	}
	return x
}
