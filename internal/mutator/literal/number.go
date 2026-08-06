package literal

import (
	"go/ast"
	"go/constant"
	"go/token"

	"github.com/jh125486/turango/internal/mutator"
)

// NumberName is the registry name of the integer-literal boundary-shift
// operator.
const NumberName = "literal/number"

func init() {
	mutator.Register(NumberName, func() mutator.Mutator { return &NumberMutator{} })
}

// NumberMutator shifts an integer literal by one in each direction: `0`
// offers both `1` and `-1`. This is the classic off-by-one mutant — `x < 0`
// becomes `x < 1` — distinct from operator/boundary's `<` -> `<=`, which
// changes the comparison rather than the threshold.
//
// It is stateless: the literal it edits is captured by the closures of the
// [mutator.Mutation] values it returns, so a single instance is safe to
// reuse for every node of every file in a walk.
type NumberMutator struct{}

// Name reports the operator's registry name.
func (*NumberMutator) Name() string { return NumberName }

// Applies reports whether node is an integer literal. It is a type
// assertion and a field comparison — no allocation on the common
// (non-matching) path.
func (*NumberMutator) Applies(node ast.Node) bool {
	lit, ok := node.(*ast.BasicLit)

	return ok && lit.Kind == token.INT
}

// Mutate returns the two shifts available for node — value+1 and value-1 —
// or nil for anything else. go/constant parses the literal, so every Go
// integer syntax (decimal, hex, octal, binary, with or without `_`
// separators) is handled uniformly rather than needing its own case; the
// mutated value is rendered back out as a plain decimal, which need not
// match the original literal's base to be a valid, meaningfully different
// mutant.
func (*NumberMutator) Mutate(node ast.Node) []mutator.Mutation {
	lit, ok := node.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return nil
	}

	val := constant.MakeFromLiteral(lit.Value, token.INT, 0)
	if val.Kind() != constant.Int {
		return nil
	}

	return []mutator.Mutation{
		shiftBy(lit, val, token.ADD),
		shiftBy(lit, val, token.SUB),
	}
}

// shiftBy builds the mutation that replaces lit's value with val shifted by
// one in the direction op names (token.ADD or token.SUB).
//
// Apply/Revert write through lit.Value directly — an *ast.BasicLit is
// mutated in place by rewriting its Value string, the same field
// go/printer reads back out, so no replacement node is needed the way a
// swapped operand or operator elsewhere in this codebase needs one.
func shiftBy(lit *ast.BasicLit, val constant.Value, op token.Token) mutator.Mutation {
	shifted := constant.BinaryOp(val, op, constant.MakeInt64(1))
	replacement := shifted.ExactString()
	original := lit.Value

	return mutator.Mutation{
		Description: original + " -> " + replacement,
		Apply:       func() { lit.Value = replacement },
		Revert:      func() { lit.Value = original },
	}
}
