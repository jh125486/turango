package operator

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

func init() {
	mutator.Register(incDecName, func() mutator.Mutator { return incDec{} })
}

// incDec swaps an increment statement for a decrement and vice versa: `i++`
// becomes `i--`, `i--` becomes `i++`.
type incDec struct{}

// Name reports the registry name, "operator/inc_dec".
func (incDec) Name() string { return incDecName }

// Applies reports whether node is an increment or decrement statement. It is
// one type assertion and one map lookup, allocating nothing.
func (incDec) Applies(node ast.Node) bool {
	stmt, ok := node.(*ast.IncDecStmt)
	if !ok {
		return false
	}

	_, ok = incDecSwaps[stmt.Tok]

	return ok
}

// Mutate returns the single swap available for node, or nil if node is not an
// increment or decrement statement. It does not touch the AST; only the
// returned Apply does.
func (incDec) Mutate(node ast.Node) []mutator.Mutation {
	stmt, ok := node.(*ast.IncDecStmt)
	if !ok {
		return nil
	}

	swapped, ok := incDecSwaps[stmt.Tok]
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
