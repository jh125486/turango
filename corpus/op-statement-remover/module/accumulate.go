// Package opstatementremover is a minimal, hand-built integration fixture
// for the statement/remover mutator.
package opstatementremover

// Accumulate sums vals.
func Accumulate(vals []int) int {
	total := 0
	for _, v := range vals {
		total += v
	}
	return total
}
