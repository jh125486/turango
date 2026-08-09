package control

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

// IfName is the registry name of the if-body removal operator.
const IfName = "control/if"

func init() {
	mutator.Register(IfName, func() mutator.Mutator { return &If{} })
}

// If removes the body of an if statement, turning
//
//	if cond { doThing() }
//
// into
//
//	if cond { }
//
// The condition and any else branch are left alone; the else branch is the
// [Else] operator's job.
type If struct{}

// Name reports the operator's registry name.
func (*If) Name() string { return IfName }

// Applies reports whether node is an if statement with a non-empty body. An
// already-empty body has nothing to remove.
func (*If) Applies(node ast.Node) bool {
	stmt, ok := node.(*ast.IfStmt)

	return ok && stmt.Body != nil && len(stmt.Body.List) > 0
}

// Mutate returns the single mutation that empties node's if body.
func (m *If) Mutate(node ast.Node) []mutator.Mutation {
	if !m.Applies(node) {
		return nil
	}

	stmt, ok := node.(*ast.IfStmt)
	if !ok {
		return nil
	}

	return []mutator.Mutation{clearStmts(&stmt.Body.List, stmt.Body, "remove if body")}
}
