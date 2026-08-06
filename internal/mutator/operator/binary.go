package operator

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

func init() {
	mutator.Register(binaryName, func() mutator.Mutator { return binary{} })
}

// binary swaps the operator of a binary expression for a related one, per
// binarySwaps: `a + b` becomes `a - b`, `a == b` becomes `a != b`, `a < b`
// becomes `a >= b`, and so on across the arithmetic, bitwise, logical and
// comparison operators.
type binary struct{}

// Name reports the registry name, "operator/binary".
func (binary) Name() string { return binaryName }

// Applies reports whether node is a binary expression whose operator has a
// swap. It is one type assertion and one map lookup, allocating nothing.
func (binary) Applies(node ast.Node) bool {
	expr, ok := node.(*ast.BinaryExpr)
	if !ok {
		return false
	}

	_, ok = binarySwaps[expr.Op]

	return ok
}

// Mutate returns the single swap available for node's operator, or nil if node
// is not an eligible binary expression. It does not touch the AST; only the
// returned Apply does.
func (binary) Mutate(node ast.Node) []mutator.Mutation {
	expr, ok := node.(*ast.BinaryExpr)
	if !ok {
		return nil
	}

	swapped, ok := binarySwaps[expr.Op]
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
