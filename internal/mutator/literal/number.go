package literal

import (
	"go/ast"
	"go/constant"
	"go/token"

	"github.com/jh125486/turango/internal/mutator"
)

// NumberName is the registry name of the numeric-literal boundary-shift
// operator.
const NumberName = "literal/number"

func init() {
	mutator.Register(NumberName, func() mutator.Mutator { return &NumberMutator{} })
}

// numberFloatShift is the magnitude used to nudge a float literal, in both
// of the roles described on shiftFloat: as a relative (0.1%) multiplier of
// the literal's own value, and — when that multiplication degenerates to
// zero, i.e. the literal itself is `0` — as the absolute fallback delta.
// 0.1% was chosen empirically to land the shifted value's first differing
// digit within the ~6 significant digits go/constant's Value.String keeps
// for float rendering
// (see shiftFloat), so the mutant reliably differs from the original in
// its printed form, while still being close enough to read as a boundary-
// adjacent nudge rather than the wild swing a flat "+1" would be on a
// literal like `0.95`.
var numberFloatShift = constant.MakeFromLiteral("0.001", token.FLOAT, 0)

// NumberMutator shifts a numeric literal — integer or float — by a small
// amount in each direction. For an integer, `0` offers both `1` and `-1`:
// the classic off-by-one mutant, `x < 0` becoming `x < 1` — distinct from
// operator/boundary's `<` -> `<=`, which changes the comparison rather than
// the threshold. For a float, the shift is a relative nudge rather than a
// flat ±1 (see shiftFloat for why).
//
// It is stateless: the literal it edits is captured by the closures of the
// [mutator.Mutation] values it returns, so a single instance is safe to
// reuse for every node of every file in a walk.
type NumberMutator struct{}

// Name reports the operator's registry name.
func (*NumberMutator) Name() string { return NumberName }

// Applies reports whether node is an integer or float literal. It is a
// type assertion and a field comparison — no allocation on the common
// (non-matching) path.
func (*NumberMutator) Applies(node ast.Node) bool {
	lit, ok := node.(*ast.BasicLit)

	return ok && (lit.Kind == token.INT || lit.Kind == token.FLOAT)
}

// Mutate returns the two shifts available for node — one in each direction
// — or nil for anything else. go/constant parses the literal, so every Go
// numeric syntax (decimal, hex, octal, binary integers; decimal or
// exponent-notation floats; any of them with `_` separators) is handled
// uniformly rather than needing its own case per syntax form. Integers and
// floats still need different shift *logic* — see shiftInt vs shiftFloat —
// so Mutate branches once on val.Kind() and dispatches to whichever
// applies; everything else about the two (the AST node matched, the
// Apply/Revert mechanics, the registry entry) is identical, which is why
// this is one operator rather than a `literal/float` sibling to
// `literal/number`.
func (*NumberMutator) Mutate(node ast.Node) []mutator.Mutation {
	lit, ok := node.(*ast.BasicLit)
	if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT) {
		return nil
	}

	val := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)

	switch val.Kind() {
	case constant.Int:
		return []mutator.Mutation{
			shiftInt(lit, val, token.ADD),
			shiftInt(lit, val, token.SUB),
		}
	case constant.Float:
		return []mutator.Mutation{
			shiftFloat(lit, val, token.ADD),
			shiftFloat(lit, val, token.SUB),
		}
	default:
		// Unreachable given the Applies guard above (token.INT/token.FLOAT
		// always parse to constant.Int/constant.Float), kept only so this
		// switch doesn't need a default-free exhaustiveness argument.
		return nil
	}
}

// shiftInt builds the mutation that replaces lit's value with val shifted
// by one in the direction op names (token.ADD or token.SUB).
//
// Apply/Revert write through lit.Value directly — an *ast.BasicLit is
// mutated in place by rewriting its Value string, the same field
// go/printer reads back out, so no replacement node is needed the way a
// swapped operand or operator elsewhere in this codebase needs one.
//
// The replacement is rendered with ExactString, not String: for an integer
// constant.Value the two agree (String is just ExactString without a
// rounding step to skip), so either would do here — ExactString is used to
// make the choice deliberate rather than incidental, since shiftFloat below
// cannot use it (see that function's doc for why).
func shiftInt(lit *ast.BasicLit, val constant.Value, op token.Token) mutator.Mutation {
	shifted := constant.BinaryOp(val, op, constant.MakeInt64(1))
	replacement := shifted.ExactString()
	original := lit.Value

	return mutator.Mutation{
		Description: original + " -> " + replacement,
		Apply:       func() { lit.Value = replacement },
		Revert:      func() { lit.Value = original },
	}
}

// shiftFloat builds the float-literal counterpart to shiftInt: the
// mutation that replaces lit's value with val nudged by numberFloatShift in
// the direction op names.
//
// Two things differ from the integer case, both confirmed empirically
// while building this operator rather than assumed from go/constant's
// docs alone:
//
//  1. Magnitude. Reusing the integer path's flat "+1" on a float produces
//     an uninteresting mutant — `0.95` -> `1.95` is nowhere near the
//     original value, so almost any test exercising the boundary at all
//     would kill it, the same "avoid trivially-caught mutants" concern
//     operator/boundary's design already follows for comparisons. A flat
//     *absolute* epsilon fixes that for values near 1 but breaks down at
//     other scales — added to `1.5e10`, a `0.001` epsilon vanishes
//     entirely once rendered (confirmed empirically: it round-trips back
//     to the exact same printed text as the original, a silent no-op
//     mutant). A *relative* shift — numberFloatShift as a fraction of val
//     — scales with the literal, so it stays a small-but-visible nudge
//     whether the literal is `0.95` or `1.5e10`. The one relative shift
//     can't reach a literal that is itself exactly `0`, so that case falls
//     back to using numberFloatShift as an absolute delta instead (see its
//     doc comment).
//  2. Rendering. constant.Value.ExactString renders a non-integer float as
//     an exact `numerator/denominator` pair (e.g. "19019/20000" for
//     `0.95`'s shifted value) — confirmed empirically, not assumed from the
//     design sketch that motivated this operator. That is not valid Go
//     float syntax (it isn't even one token), so unlike shiftInt this
//     function renders with String instead, which go/constant documents as
//     a possibly-approximate but always Go-syntax decimal or exponent
//     form. The approximation is a `%.6g`-equivalent 6-significant-digit
//     rounding, which is exactly the "not an absurdly over-precise decimal
//     expansion" rendering the gap-10 writeup asked to confirm, and — given
//     numberFloatShift's magnitude — reliably still differs from the
//     original literal's own text after rounding.
func shiftFloat(lit *ast.BasicLit, val constant.Value, op token.Token) mutator.Mutation {
	delta := constant.BinaryOp(val, token.MUL, numberFloatShift)
	if constant.Sign(delta) == 0 {
		delta = numberFloatShift
	}

	shifted := constant.BinaryOp(val, op, delta)
	replacement := shifted.String()
	original := lit.Value

	return mutator.Mutation{
		Description: original + " -> " + replacement,
		Apply:       func() { lit.Value = replacement },
		Revert:      func() { lit.Value = original },
	}
}
