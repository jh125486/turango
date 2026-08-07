// Package opcontrolcase is a minimal, hand-built integration fixture for the
// control/case mutator.
package opcontrolcase

// Grade converts a numeric score to a letter grade.
func Grade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	default:
		return "F"
	}
}
