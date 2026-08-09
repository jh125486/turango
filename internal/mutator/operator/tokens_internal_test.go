// Whitebox: tokens.go's swap tables (assignmentSwaps, binarySwaps,
// incDecSwaps) and the buildSwaps helper that assembles them are unexported
// implementation details with no exported accessor — every operator's public
// behavior already flows through its Mutator interface, which is what
// tokens_test.go exercises blackbox. The tests here instead verify the
// tables' own invariants (no eligible token maps to itself, `=`/`:=` are
// absent by omission rather than by a separate predicate, buildSwaps rejects
// a conflicting declaration), which requires reaching those unexported
// values directly.
package operator

import (
	"go/token"
	"testing"
)

// TestSwapTablesAreSingleValued guards the property the swap tables rely on:
// every eligible token maps to exactly one other token, and never to itself.
func TestSwapTablesAreSingleValued(t *testing.T) {
	t.Parallel()

	tables := map[string]map[token.Token]token.Token{
		"assignment": assignmentSwaps,
		"binary":     binarySwaps,
		"incDec":     incDecSwaps,
	}

	for name, table := range tables {
		for from, to := range table {
			if from == to {
				t.Errorf("%s: %s swaps to itself", name, from)
			}
		}
	}
}

// TestAssignmentTableExcludesPlainAssignment documents that `=` and `:=` are
// excluded by absence from the table, not by a separate predicate.
func TestAssignmentTableExcludesPlainAssignment(t *testing.T) {
	t.Parallel()

	for _, tok := range []token.Token{token.ASSIGN, token.DEFINE} {
		if swapped, ok := assignmentSwaps[tok]; ok {
			t.Errorf("%s must not be eligible, but swaps to %s", tok, swapped)
		}
	}
}

// TestBuildSwapsPanicsOnConflict covers the fail-fast guard that would catch a
// future edit declaring two different swaps for one token.
func TestBuildSwapsPanicsOnConflict(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("buildSwaps did not panic on a conflicting swap")
		}
	}()

	buildSwaps(
		[][2]token.Token{{token.ADD, token.SUB}},
		[][2]token.Token{{token.ADD, token.MUL}},
	)
}
