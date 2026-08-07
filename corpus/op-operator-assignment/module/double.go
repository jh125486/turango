// Package opoperatorassignment is a minimal, hand-built integration fixture
// for the operator/assignment mutator.
package opoperatorassignment

// Double returns x doubled.
func Double(x int) int {
	x *= 2
	return x
}
