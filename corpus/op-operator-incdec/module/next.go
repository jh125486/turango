// Package opoperatorincdec is a minimal, hand-built integration fixture for
// the operator/inc_dec mutator.
package opoperatorincdec

// Next returns x incremented by one.
func Next(x int) int {
	x++
	return x
}
