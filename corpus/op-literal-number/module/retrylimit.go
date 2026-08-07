// Package opliteralnumber is a minimal, hand-built integration fixture for
// the literal/number mutator.
package opliteralnumber

// TooManyRetries reports whether count has exceeded the retry budget.
func TooManyRetries(count int) bool {
	return count > 3
}
