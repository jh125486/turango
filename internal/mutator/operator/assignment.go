package operator

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

func init() {
	mutator.Register(assignmentName, func() mutator.Mutator { return assignment{} })
}

// assignment swaps the operator of a compound assignment statement for a
// related one, per assignmentSwaps: `x += 1` becomes `x -= 1`, `x %= n` becomes
// `x /= n`, and so on.
//
// Plain assignment (`=`) and short variable declaration (`:=`) are not
// mutated — they carry no operator to swap.
type assignment struct{}

// Name reports the registry name, "operator/assignment".
func (assignment) Name() string { return assignmentName }

// Applies reports whether node is a compound assignment whose operator has a
// swap. It is one type assertion and one map lookup, allocating nothing.
func (assignment) Applies(node ast.Node) bool {
	stmt, ok := node.(*ast.AssignStmt)
	if !ok {
		return false
	}

	_, ok = assignmentSwaps[stmt.Tok]

	return ok
}

// Mutate returns the single swap available for node's assignment operator, or
// nil if node is not an eligible assignment. It does not touch the AST; only
// the returned Apply does.
func (assignment) Mutate(node ast.Node) []mutator.Mutation {
	stmt, ok := node.(*ast.AssignStmt)
	if !ok {
		return nil
	}

	swapped, ok := assignmentSwaps[stmt.Tok]
	if !ok {
		return nil
	}

	original := stmt.Tok

	return []mutator.Mutation{{
		Description: describeSwap(original, swapped),
		Apply:       func() { stmt.Tok = swapped },
		Revert:      func() { stmt.Tok = original },
	}}
}
