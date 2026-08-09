package control

import (
	"go/ast"

	"github.com/jh125486/turango/internal/mutator"
)

// CaseName is the registry name of the case-body removal operator.
const CaseName = "control/case"

// Case removes the body of a case clause, turning
//
//	case x: doThing()
//
// into
//
//	case x:
//
// One clause type covers both expression switches and type switches, so this
// operator handles both. The default clause is an [ast.CaseClause] with a nil
// List and is mutated exactly like any other clause.
type Case struct{}

func init() {
	mutator.Register(CaseName, func() mutator.Mutator { return &Case{} })
}

// Name reports the operator's registry name.
func (*Case) Name() string { return CaseName }

// Applies reports whether node is a case clause with a non-empty body. A
// fallthrough-only or empty clause has nothing to remove.
func (*Case) Applies(node ast.Node) bool {
	clause, ok := node.(*ast.CaseClause)

	return ok && len(clause.Body) > 0
}

// Mutate returns the single mutation that empties node's case body.
func (m *Case) Mutate(node ast.Node) []mutator.Mutation {
	if !m.Applies(node) {
		return nil
	}

	clause, ok := node.(*ast.CaseClause)
	if !ok {
		return nil
	}

	return []mutator.Mutation{clearStmts(&clause.Body, clause, "remove case body")}
}
