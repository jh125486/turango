// Package opsuppressnomutant is a minimal, hand-built integration fixture
// for the //nomutant suppression directive, including cascade into a
// compound statement's body.
package opsuppressnomutant

// Guarded is deliberately, permanently untested code that a //nomutant
// directive marks as intentionally excluded from mutation testing (e.g.
// because it's a defensive fallback path someone judged not worth writing a
// test for). The cascade suppresses every mutation inside the if body too,
// not just the if-statement's own condition.
func Guarded(x int) int {
	//nomutant: defensive fallback, not worth testing
	if x < 0 {
		x = 0
	}
	return x
}
