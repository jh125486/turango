// Package opexpressionremove is a minimal, hand-built integration fixture
// for the expression/remove mutator (&&/|| operand elimination).
package opexpressionremove

// InRange reports whether x falls within [lo, hi].
func InRange(x, lo, hi int) bool {
	return x >= lo && x <= hi
}
