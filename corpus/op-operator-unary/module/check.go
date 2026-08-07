// Package opoperatorunary is a minimal, hand-built integration fixture for
// the operator/unary mutator. The `!ok` here is the IfStmt's Cond directly,
// one of the three positions operator/unary recognizes.
package opoperatorunary

// Check reports whether x is positive.
func Check(x int) string {
	ok := x > 0
	if !ok {
		return "not ok"
	}
	return "ok"
}
