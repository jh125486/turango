package operator

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

func init() {
	mutator.Register(boundaryName, func() mutator.Mutator { return boundary{} })
}

// boundary shifts a relational operator's boundary by one admitted value, per
// boundarySwaps: `a < b` becomes `a <= b`, `a > b` becomes `a >= b`, and vice
// versa. It is the classic off-by-one mutant — `x < n` and `x <= n` differ
// only when x == n — distinct from binary's `<` <-> `>=` negation.
type boundary struct{}

// Name reports the registry name, "operator/boundary".
func (boundary) Name() string { return boundaryName }

// Applies reports whether node is a binary expression whose operator has a
// boundary shift. It is one type assertion and one map lookup, allocating
// nothing.
func (boundary) Applies(node ast.Node) bool {
	expr, ok := node.(*ast.BinaryExpr)
	if !ok {
		return false
	}

	_, ok = boundarySwaps[expr.Op]

	return ok
}

// Mutate returns the single boundary shift available for node's operator, or
// nil if node is not an eligible binary expression. It does not touch the
// AST; only the returned Apply does.
func (boundary) Mutate(node ast.Node) []mutator.Mutation {
	expr, ok := node.(*ast.BinaryExpr)
	if !ok {
		return nil
	}

	swapped, ok := boundarySwaps[expr.Op]
	if !ok {
		return nil
	}

	original := expr.Op

	return []mutator.Mutation{{
		Description: describeSwap(original, swapped),
		Apply:       func() { expr.Op = swapped },
		Revert:      func() { expr.Op = original },
	}}
}
