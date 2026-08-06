// Package expression implements the mutation operators that rewrite Go
// expressions.
//
// It provides one operator, expression/remove, which performs short-circuit
// term elimination: in a && or || expression it replaces one operand with the
// identity element of that operator (true for &&, false for ||) so the
// remaining operand alone decides the result. A test suite that still passes
// once a term has been neutralised never depended on that term — the classic
// symptom of a compound condition whose branches are only half covered.
package expression

import (
	"go/ast"
	"go/token"

	"github.com/jh125486/turango/internal/mutator"
)

// Name is the registry name of the short-circuit term elimination operator.
const Name = "expression/remove"

func init() {
	mutator.Register(Name, func() mutator.Mutator { return &RemoveMutator{} })
}

// RemoveMutator neutralises the operands of short-circuit boolean expressions.
//
// It is stateless: each operand it edits is captured by the closures of the
// [mutator.Mutation] values it returns, so a single instance is safe to reuse
// for every node of every file in a walk.
type RemoveMutator struct{}

// Name reports the operator's registry name.
func (*RemoveMutator) Name() string { return Name }

// Applies reports whether node is a && or || expression. It is a type switch
// and two comparisons — no allocation on any path.
func (*RemoveMutator) Applies(node ast.Node) bool {
	binary, ok := node.(*ast.BinaryExpr)

	return ok && (binary.Op == token.LAND || binary.Op == token.LOR)
}

// Mutate returns two mutations for a && or || node — one replacing the left
// operand, one replacing the right — and nil for anything else.
//
// Each mutation is independently applicable and revertible; they are not meant
// to be combined, since neutralising both operands would leave a constant. Only
// the node passed in is considered: an operand that is itself a && or || is
// replaced wholesale rather than recursed into, because the engine walks the
// tree and calls Mutate for that nested node separately.
func (m *RemoveMutator) Mutate(node ast.Node) []mutator.Mutation {
	binary, ok := node.(*ast.BinaryExpr)
	if !ok || (binary.Op != token.LAND && binary.Op != token.LOR) {
		return nil
	}

	// The identity element of the operator: x && true == x and x || false == x,
	// so substituting it for one operand hands the whole result to the other.
	literal := "false"
	if binary.Op == token.LAND {
		literal = "true"
	}

	return []mutator.Mutation{
		replaceOperand(binary, &binary.X, "left", literal),
		replaceOperand(binary, &binary.Y, "right", literal),
	}
}

// replaceOperand builds the mutation that swaps the operand stored at operand
// for the boolean literal named literal.
//
// operand is the address of the [ast.BinaryExpr] field itself, so Apply writes
// through it and Revert restores the exact ast.Expr value the field held rather
// than a reconstructed equivalent, as [mutator.Mutation] requires.
//
// true and false are predeclared identifiers rather than literals in the Go
// grammar, so the replacement is an [ast.Ident], not an [ast.BasicLit].
// Anchoring it at the original operand's position keeps go/printer's line
// breaking of the surrounding expression unchanged.
func replaceOperand(binary *ast.BinaryExpr, operand *ast.Expr, side, literal string) mutator.Mutation {
	original := *operand
	replacement := &ast.Ident{NamePos: original.Pos(), Name: literal}

	return mutator.Mutation{
		Description: "replace " + side + " operand of " + binary.Op.String() + " with " + literal,
		Apply:       func() { *operand = replacement },
		Revert:      func() { *operand = original },
	}
}
