// Package identifier implements mutation operators whose eligibility depends
// on static type information, answering questions purely syntactic operators
// (control, expression, literal, operator, statement) cannot: "is this
// identifier interchangeable with that one?"
//
// # Why the scope is restricted so hard
//
// The strconv ParseUint bug this operator is modelled on (see PROPOSAL.md's
// "Known gap" section) was a wrong-identifier substitution: a bitSize-scoped
// local swapped for a package-level named constant. The natural generalisation
// — "any type-compatible identifier in scope" — is deliberately not what this
// operator does. Walking every *ast.Ident use against every same-type
// identifier Go's scoping rules make visible at that point (locals, params,
// package vars, package consts, dot-imports) would make Applies true for a
// large fraction of identifier references in an ordinary file and offer one
// mutation per same-type sibling for each — on a function with a dozen
// int-typed locals, that one function alone could dwarf what the other twelve
// operators produce combined.
//
// v1's restriction is package-level const-for-const only, with an exact type
// match (types.Identical, not just a shared underlying kind — a looser match
// would mostly inflate the NotViable count with mutants Go's type system was
// always going to reject) and a same-declaration-group requirement: two
// constants are only offered as swaps for each other when they are declared
// in the same parenthesized `const ( ... )` block, or — for a constant
// declared on its own — the same file. This targets the common "picked the
// wrong sibling constant" bug shape (adjacent error codes, size limits,
// enum-like values) while keeping the candidate set per identifier small and
// bounded by how many constants a block or file realistically holds, rather
// than by total package size.
//
// This does not reproduce the strconv bug exactly — that swap was local-to-
// package-const, not const-to-const — and is a known, deliberate v1
// limitation (see ROADMAP.md, gap 1).
//
// # Why this operator opts into TypedMutator
//
// Every other operator in this codebase implements only [mutator.Mutator] and
// is purely syntactic: Applies/Mutate see an *ast.Node and nothing else. This
// operator additionally needs to know what an identifier resolves to and what
// it is typed as, which only [go/types] can answer. It gets that by
// implementing [mutator.TypedMutator]: the registry-held instance constructed
// by init is inert (its Applies always reports false, since it has no type
// information to consult), and the engine calls WithScope once per package,
// after type-checking, to obtain a package-bound instance that does the real
// work. WithScope always returns a new value rather than mutating the
// receiver, so the registry-held instance stays stateless and shareable
// exactly as [mutator.Mutator]'s contract requires, and the bound instance's
// state (the type info, the package, and the const groupings derived from
// them) is set once, before the value is ever used by more than one file's
// walk, and never written to afterwards — safe for the concurrently-mutated
// files of one package to share.
package identifier

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/jh125486/turango/internal/mutator"
)

// ConstSwapName is the registry name of the package-level const-swap operator.
const ConstSwapName = "identifier/constswap"

func init() {
	mutator.Register(ConstSwapName, func() mutator.Mutator { return &constSwap{} })
}

// constSwap swaps a use of a package-level constant for another package-level
// constant of the exact same type, declared in the same const ( ... ) block
// (or, for a constant declared on its own, the same file).
//
// The zero value — what the registry constructs — has no type information and
// is inert: Applies always reports false. A package-bound, working instance
// is obtained by calling [constSwap.WithScope], which returns a new value
// rather than mutating this one, satisfying both [mutator.Mutator] (stateless,
// shareable) and [mutator.TypedMutator] (new value per package).
type constSwap struct {
	info *types.Info
	pkg  *types.Package

	// groups maps a package-level constant to the other constants declared in
	// the same const ( ... ) block or, if it was not block-declared, the same
	// file. It is built once, in WithScope, from the syntax trees reachable
	// through info.Scopes' *ast.File keys — WithScope itself is not handed a
	// syntax tree, since [mutator.TypedMutator]'s signature deliberately keeps
	// to type information only.
	groups map[*types.Const][]*types.Const
}

// Name reports the operator's registry name.
func (*constSwap) Name() string { return ConstSwapName }

// Applies reports whether node is a use of a package-level constant that has
// at least one exact-type-matching, same-block-or-file sibling to swap in.
//
// It is false, unconditionally, for the zero-value (unbound) instance: info
// is nil, so there is nothing to consult.
func (c *constSwap) Applies(node ast.Node) bool {
	if c.info == nil {
		return false
	}

	obj, ok := c.constUse(node)

	return ok && len(c.candidates(obj)) > 0
}

// Mutate returns one mutation per eligible sibling constant, each rewriting
// the identifier's Name field directly — the same in-place-field-edit idiom
// literal/number.go's shiftBy uses for *ast.BasicLit.Value, since go/printer
// only ever reads Name back out.
func (c *constSwap) Mutate(node ast.Node) []mutator.Mutation {
	if c.info == nil {
		return nil
	}

	ident, obj, ok := c.constUseIdent(node)
	if !ok {
		return nil
	}

	candidates := c.candidates(obj)
	if len(candidates) == 0 {
		return nil
	}

	original := ident.Name

	mutations := make([]mutator.Mutation, 0, len(candidates))

	for _, cand := range candidates {
		replacement := cand.Name()

		mutations = append(mutations, mutator.Mutation{
			Description: original + " -> " + replacement,
			Apply:       func() { ident.Name = replacement },
			Revert:      func() { ident.Name = original },
		})
	}

	return mutations
}

// WithScope returns a new constSwap bound to pkg's type information. The
// receiver is not modified, so the value WithScope was called on stays the
// stateless, inert placeholder the registry constructed.
func (*constSwap) WithScope(info *types.Info, pkg *types.Package) mutator.Mutator {
	return &constSwap{
		info:   info,
		pkg:    pkg,
		groups: buildGroups(info),
	}
}

// constUse reports the *types.Const node is a use of, per info.Uses — not
// info.Defs, which would resolve the identifier at its own declaration rather
// than at a reference to it.
func (c *constSwap) constUse(node ast.Node) (*types.Const, bool) {
	_, obj, ok := c.constUseIdent(node)

	return obj, ok
}

// constUseIdent is constUse plus the *ast.Ident itself, which Mutate needs to
// build its closures around.
func (c *constSwap) constUseIdent(node ast.Node) (*ast.Ident, *types.Const, bool) {
	ident, ok := node.(*ast.Ident)
	if !ok {
		return nil, nil, false
	}

	obj, ok := c.info.Uses[ident].(*types.Const)
	if !ok {
		return nil, nil, false
	}

	return ident, obj, true
}

// candidates returns obj's same-group siblings whose type is exactly
// (types.Identical) obj's own type.
func (c *constSwap) candidates(obj *types.Const) []*types.Const {
	var out []*types.Const

	for _, sibling := range c.groups[obj] {
		if types.Identical(obj.Type(), sibling.Type()) {
			out = append(out, sibling)
		}
	}

	return out
}

// buildGroups walks every file reachable through info.Scopes (the *ast.File
// keys types.Config.Check always populates one for, per file) and groups
// every package-level constant it finds by its declaration: constants in the
// same parenthesized const ( ... ) block share a group keyed by that
// *ast.GenDecl; a constant declared on its own is grouped with the other
// constants of its file, keyed by the *ast.File.
//
// This is how the operator learns block/file grouping without WithScope being
// handed a syntax tree directly: info.Scopes' keys are themselves AST nodes,
// which is enough to walk from.
func buildGroups(info *types.Info) map[*types.Const][]*types.Const {
	members := map[ast.Node][]*types.Const{}
	keyOf := map[*types.Const]ast.Node{}

	for node := range info.Scopes {
		file, ok := node.(*ast.File)
		if !ok {
			continue
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}

			var key ast.Node = file
			if gen.Lparen.IsValid() {
				key = gen
			}

			for _, spec := range gen.Specs {
				val, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				for _, name := range val.Names {
					obj, ok := info.Defs[name].(*types.Const)
					if !ok {
						continue
					}

					members[key] = append(members[key], obj)
					keyOf[obj] = key
				}
			}
		}
	}

	groups := make(map[*types.Const][]*types.Const, len(keyOf))

	for obj, key := range keyOf {
		for _, sibling := range members[key] {
			if sibling != obj {
				groups[obj] = append(groups[obj], sibling)
			}
		}

		// Sorted by name for a deterministic Mutate order: members[key] is
		// built from a map iteration over info.Scopes, whose order is not
		// stable across runs.
		sort.Slice(groups[obj], func(i, j int) bool {
			return groups[obj][i].Name() < groups[obj][j].Name()
		})
	}

	return groups
}
