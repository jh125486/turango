// Package statement implements mutation operators that delete whole statements
// from a function body, answering the question "does the test suite notice when
// this line never runs?".
//
// # Why the operator matches blocks, not statements
//
// The natural reading of "delete a statement" is a mutator whose Applies matches
// *ast.ExprStmt, *ast.IncDecStmt and *ast.AssignStmt directly. That shape cannot
// be implemented against a generic [go/ast.Walk]: replacing a statement requires
// writing to the slot it occupies in its parent's statement list, and a walk
// hands a visitor the node alone — no parent, no slot. Recovering the parent
// would mean threading a parent stack through the engine's walk purely for this
// one operator.
//
// So the operator matches the *container* instead: an [go/ast.BlockStmt] or an
// [go/ast.CaseClause], both of which own a []ast.Stmt the operator can index
// into. One call to Mutate on a container yields one independent [mutator.Mutation]
// per eligible statement it holds, each closing over the index it replaces, which
// preserves the engine's one-mutation-at-a-time contract without any extra walk
// machinery.
package statement

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"slices"
	"strconv"
	"strings"

	"github.com/jh125486/turango/internal/mutator"
)

// RemoverName is the registry name of the statement removal operator.
const RemoverName = "statement/remover"

// descriptionLimit caps the rendered statement in a Mutation description so a
// long line does not swamp a report row.
const descriptionLimit = 60

func init() {
	mutator.Register(RemoverName, func() mutator.Mutator { return &Remover{} })
}

// Remover replaces a single statement with an empty statement, simulating the
// line having been deleted.
//
// It is stateless: every per-node value lives in the closures returned by
// [Remover.Mutate], so one instance may be walked over many files.
type Remover struct{}

// Name reports the operator's registry name.
func (*Remover) Name() string { return RemoverName }

// Applies reports whether node is a statement container holding at least one
// removable statement.
//
// The scan is O(len(list)), but blocks and case clauses are a small minority of
// the nodes in a file, and the loop allocates nothing on either path.
func (*Remover) Applies(node ast.Node) bool {
	return slices.ContainsFunc(statements(node), removable)
}

// Mutate returns one mutation per removable statement in node's statement list.
//
// Each mutation swaps its statement for an [go/ast.EmptyStmt] on Apply and puts
// the original ast.Stmt value back on Revert. Mutate itself leaves the AST
// untouched.
func (*Remover) Mutate(node ast.Node) []mutator.Mutation {
	list := statements(node)

	var mutations []mutator.Mutation

	for i, stmt := range list {
		if !removable(stmt) {
			continue
		}

		if mutations == nil {
			mutations = make([]mutator.Mutation, 0, len(list)-i)
		}

		// The empty statement is built once, here, rather than inside Apply, so
		// that repeated Apply/Revert cycles reuse the same node. Its Semicolon
		// position is the deleted statement's, which keeps go/printer emitting
		// the surrounding statements on their original lines.
		blank := &ast.EmptyStmt{Semicolon: stmt.Pos(), Implicit: true}

		mutations = append(mutations, mutator.Mutation{
			Description: describe(i, stmt),
			Apply:       func() { list[i] = blank },
			Revert:      func() { list[i] = stmt },
			// Node is the removed statement itself, not node (the
			// container) — Mutate offers one mutation per removable
			// statement, and a container can hold many, so reporting the
			// whole container's before/after text per mutation would be
			// far more verbose than Description's single-statement
			// granularity. stmt's own text never changes (Apply replaces
			// list's slot, not stmt's fields), which is exactly the signal
			// the engine's before/after capture uses to report an empty
			// "after": printing the same node before and after Apply and
			// getting identical text means the node itself wasn't edited
			// in place, so the only place its removal is visible is by
			// its absence, not by any diff in its own printed form.
			Node: stmt,
		})
	}

	return mutations
}

// statements returns the statement list node owns, or nil if node is not a
// statement container. The returned slice aliases the node's own backing array,
// so assigning to an element edits the AST in place.
func statements(node ast.Node) []ast.Stmt {
	switch n := node.(type) {
	case *ast.BlockStmt:
		return n.List
	case *ast.CaseClause:
		return n.Body
	default:
		return nil
	}
}

// removable reports whether deleting stmt on its own yields a viable mutant.
//
// Expression statements, increments/decrements and plain assignments all leave
// every surrounding identifier declared and in scope when removed. A short
// variable declaration (:=) does not: deleting it un-declares the variable and
// breaks every later reference, so the mutant fails to compile rather than
// telling us anything about the test suite.
func removable(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ExprStmt, *ast.IncDecStmt:
		return true
	case *ast.AssignStmt:
		return s.Tok != token.DEFINE
	default:
		return false
	}
}

// describe renders a one-line summary of the statement being removed, falling
// back to the index if the statement cannot be printed.
func describe(index int, stmt ast.Stmt) string {
	var buf bytes.Buffer

	if err := printer.Fprint(&buf, token.NewFileSet(), stmt); err != nil {
		return "remove statement at index " + strconv.Itoa(index)
	}

	// Collapse the printer's newlines and indentation so a multi-line statement
	// still occupies a single report row.
	src := strings.Join(strings.Fields(buf.String()), " ")
	if len(src) > descriptionLimit {
		src = src[:descriptionLimit] + "..."
	}

	return "remove statement: " + src
}
