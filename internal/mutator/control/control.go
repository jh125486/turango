// Package control implements the body-removal mutation operators.
//
// Each operator empties the statement list of one control-flow construct — an
// if body, a plain else body, or a switch/type-switch case clause — leaving the
// construct itself (and its condition) intact. A test suite that still passes
// with a branch's body deleted never asserted on anything that branch does.
//
// The operators are registered from this package's init functions, so importing
// the package for its side effect is all that is needed to enable them:
//
//	control/if      remove the body of an if statement
//	control/else    remove the body of a plain else block
//	control/case    remove the body of a case clause
//
// None of them rewrite the condition, the else-if chain, or the case
// expression list: those shapes belong to other operators, and mutating them
// here would produce duplicate mutants.
package control

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

// clearStmts returns a Mutation that empties the statement list addressed by
// list.
//
// The original slice header is captured now, before Apply runs, and restored
// verbatim by Revert — the statements themselves are never copied or rebuilt,
// so the reverted AST is the exact one the engine started with and can be
// reused for the rest of the walk.
//
// node is the container the list belongs to — an *ast.BlockStmt for If/Else,
// an *ast.CaseClause for Case — reported as the mutation's [mutator.Mutation.Node].
// It is coarser than "just the body" (an *ast.CaseClause's printed form
// includes its "case x:" header, since Go's AST has no standalone node for a
// case body), an honest limitation rather than a synthesized node not
// actually in the tree.
func clearStmts(list *[]ast.Stmt, node ast.Node, description string) mutator.Mutation {
	original := *list

	return mutator.Mutation{
		Description: description,
		Apply:       func() { *list = nil },
		Revert:      func() { *list = original },
		Node:        node,
	}
}
