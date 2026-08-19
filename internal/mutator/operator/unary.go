package operator

import (
	"go/ast"
	"slices"

	"github.com/jh125486/turango/internal/mutator"
)

func init() {
	mutator.Register(unaryName, func() mutator.Mutator { return unary{} })
}

// unary strips a unary operator off its operand, replacing the whole
// [go/ast.UnaryExpr] with its bare X: `if !ok` becomes `if ok`, `a = -b`
// becomes `a = b`, `x && !y` becomes `x && y`.
//
// Unlike the other operators in this package it substitutes nothing — there is
// no swap table — it removes a node from the tree.
//
// # Why this matches containers, not the unary expression
//
// The mutation is deliberately restricted to three positions, ported as-is from
// go-turango: an operand of a binary expression, the condition of an if
// statement, and the right-hand side of an assignment. Everywhere else a unary
// expression is left alone, because stripping it there is far more likely to
// produce a mutant that does not compile (`&x` passed where a pointer is
// wanted, `<-ch` in a receive, `*p` in a dereference) than one that reveals a
// missing test.
//
// A node does not know its parent, and [mutator.Mutator] hands over one node at
// a time, so this mutator matches on the *container* — [go/ast.BinaryExpr],
// [go/ast.IfStmt], [go/ast.AssignStmt] — and looks down at the slot rather than
// matching the [go/ast.UnaryExpr] and looking up. That keeps the restriction
// expressible without threading parent pointers through the walk, and mirrors
// the statement/remover operator, which likewise matches the enclosing block
// rather than each statement in it.
//
// The upshot for the engine: when a walk visits `if !ok {}`, the mutation is
// offered while visiting the *if statement*, not while visiting `!ok`. Every
// unary expression in one of the three positions is reachable exactly once,
// because each slot belongs to exactly one container.
type unary struct{}

// Name reports the registry name, "operator/unary".
func (unary) Name() string { return unaryName }

// Applies reports whether node is a container holding a unary expression in a
// strippable slot. It is a type switch over three node types and a handful of
// type assertions, allocating nothing.
func (unary) Applies(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.BinaryExpr:
		return isUnaryExpr(n.X) || isUnaryExpr(n.Y)
	case *ast.IfStmt:
		return isUnaryExpr(n.Cond)
	case *ast.AssignStmt:
		return slices.ContainsFunc(n.Rhs, isUnaryExpr)
	default:
		return false
	}
}

// Mutate returns one mutation per strippable slot in node — up to two for a
// binary expression, one per right-hand side for an assignment — or nil if node
// holds no unary expression in a strippable slot. It does not touch the AST;
// only the returned Apply funcs do.
func (unary) Mutate(node ast.Node) []mutator.Mutation {
	switch n := node.(type) {
	case *ast.BinaryExpr:
		return stripMutations("binary operand", &n.X, &n.Y)
	case *ast.IfStmt:
		return stripMutations("if condition", &n.Cond)
	case *ast.AssignStmt:
		slots := make([]*ast.Expr, len(n.Rhs))
		for i := range n.Rhs {
			slots[i] = &n.Rhs[i]
		}

		return stripMutations("assignment right-hand side", slots...)
	default:
		return nil
	}
}

// stripMutations builds a mutation for each slot that currently holds a unary
// expression, describing the slot's role in its container as position.
//
// Each mutation closes over the slot's address, so Revert restores the exact
// original [go/ast.UnaryExpr] pointer rather than an equivalent rebuilt node,
// as [mutator.Mutation] requires.
func stripMutations(position string, slots ...*ast.Expr) []mutator.Mutation {
	var mutations []mutator.Mutation

	for _, slot := range slots {
		expr, ok := (*slot).(*ast.UnaryExpr)
		if !ok {
			continue
		}

		mutations = append(mutations, mutator.Mutation{
			Description: "strip unary " + expr.Op.String() + " from " + position,
			Apply:       func() { *slot = expr.X },
			Revert:      func() { *slot = expr },
		})
	}

	return mutations
}

// isUnaryExpr reports whether expr is a unary expression, tolerating the nil
// slot an [go/ast.IfStmt] or [go/ast.BinaryExpr] can carry in malformed source.
func isUnaryExpr(expr ast.Expr) bool {
	_, ok := expr.(*ast.UnaryExpr)

	return ok
}
