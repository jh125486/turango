// Package opoperatorboundary is a minimal, hand-built integration fixture
// for the operator/boundary mutator (the classic off-by-one mutant).
package opoperatorboundary

// BelowLimit reports whether x is strictly below limit.
func BelowLimit(x, limit int) bool {
	return x < limit
}
