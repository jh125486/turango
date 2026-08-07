// Package opcontrolelse is a minimal, hand-built integration fixture for the
// control/else mutator.
package opcontrolelse

// Sign reports whether x is non-negative or negative.
func Sign(x int) string {
	if x >= 0 {
		return "non-negative"
	} else {
		return "negative"
	}
}
