// Package optceequivalent is a minimal, hand-built fixture demonstrating a
// real Trivial Compiler Equivalence (TCE) filter, not just TCE's cost. See
// ROADMAP.md gap 2's own spike, which validated exactly this shape.
package optceequivalent

// DeadStore is a textbook dead-store-elimination case: total is assigned
// 999, then immediately overwritten by 0 before anything ever reads the
// first value. The compiler discards the first assignment entirely, so
// deleting it (statement/remover's "remove statement: total = 999")
// produces byte-identical compiled output to the original -- a mutant TCE
// should filter as compiler-equivalent rather than report as a real
// survivor.
func DeadStore(n int) int {
	total := 999
	total = 0

	for i := range n {
		total += i
	}

	return total
}
