package control

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

// ElseName is the registry name of the else-body removal operator.
const ElseName = "control/else"

func init() {
	mutator.Register(ElseName, func() mutator.Mutator { return &Else{} })
}

// Else removes the body of a plain else block, turning
//
//	if cond { a() } else { b() }
//
// into
//
//	if cond { a() } else { }
//
// An "else if" chain is skipped: there the Else field holds another
// [ast.IfStmt] rather than a block, and that nested if is itself visited by the
// walk, so the [If] operator already covers it.
type Else struct{}

// Name reports the operator's registry name.
func (*Else) Name() string { return ElseName }

// Applies reports whether node is an if statement whose else branch is a plain
// block with at least one statement in it.
func (*Else) Applies(node ast.Node) bool {
	stmt, ok := node.(*ast.IfStmt)
	if !ok {
		return false
	}

	block, ok := stmt.Else.(*ast.BlockStmt)

	return ok && len(block.List) > 0
}

// Mutate returns the single mutation that empties node's else body.
func (m *Else) Mutate(node ast.Node) []mutator.Mutation {
	if !m.Applies(node) {
		return nil
	}

	stmt, ok := node.(*ast.IfStmt)
	if !ok {
		return nil
	}

	block, ok := stmt.Else.(*ast.BlockStmt)
	if !ok {
		return nil
	}

	return []mutator.Mutation{clearStmts(&block.List, block, "remove else body")}
}
