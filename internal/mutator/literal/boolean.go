package literal

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

// BooleanName is the registry name of the boolean-literal flip operator.
const BooleanName = "literal/boolean"

// The two predeclared boolean identifiers this operator flips between.
const (
	trueLit  = "true"
	falseLit = "false"
)

func init() {
	mutator.Register(BooleanName, func() mutator.Mutator { return &BooleanMutator{} })
}

// BooleanMutator flips a `true`/`false` literal to its opposite.
//
// true and false are predeclared identifiers rather than a distinct literal
// kind in Go's grammar (there is no boolean *ast.BasicLit), so this matches
// on *ast.Ident by name. That is a purely syntactic check: without type
// information, an *ast.Ident named "true" or "false" is indistinguishable
// from a local variable that happens to shadow the predeclared identifier
// of the same name (legal Go, if unusual) — the same class of limitation
// operator/unary documents for its own position-based matching. In
// practice this is rare enough not to be worth the go/types cost of ruling
// out.
type BooleanMutator struct{}

// Name reports the operator's registry name.
func (*BooleanMutator) Name() string { return BooleanName }

// Applies reports whether node is the identifier "true" or "false". It is a
// type assertion and two string comparisons — no allocation on the common
// (non-matching) path.
func (*BooleanMutator) Applies(node ast.Node) bool {
	ident, ok := node.(*ast.Ident)

	return ok && (ident.Name == trueLit || ident.Name == falseLit)
}

// Mutate returns the single flip available for node, or nil for anything
// else.
func (*BooleanMutator) Mutate(node ast.Node) []mutator.Mutation {
	ident, ok := node.(*ast.Ident)
	if !ok {
		return nil
	}

	var swapped string

	switch ident.Name {
	case trueLit:
		swapped = falseLit
	case falseLit:
		swapped = trueLit
	default:
		return nil
	}

	original := ident.Name

	return []mutator.Mutation{{
		Description: original + " -> " + swapped,
		Apply:       func() { ident.Name = swapped },
		Revert:      func() { ident.Name = original },
	}}
}
