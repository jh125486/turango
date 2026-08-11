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
// # v2: local-variable-to-package-constant swap (identifier/localconstswap)
//
// [localConstSwap], also in this file, is the v2 extension ROADMAP.md's gap 1
// flags as follow-on: it closes the strconv gap directly by allowing a
// *local* variable's use — not just another package-level constant's use —
// to be swapped for a same-type package-level constant. It is a separate
// registered operator (a separate node shape needs a separate Applies/Mutate
// pair, following this codebase's existing precedent of splitting related-
// but-distinct mutation shapes into distinct operators — see how
// control/if and control/else are two operators despite both editing parts
// of the same *ast.IfStmt), not a change to [constSwap] above.
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
	"go/constant"
	"go/token"
	"go/types"
	"math"
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

// LocalConstSwapName is the registry name of the v2, local-variable-to-
// package-constant swap operator (ROADMAP.md gap 1's follow-on).
const LocalConstSwapName = "identifier/localconstswap"

func init() {
	mutator.Register(LocalConstSwapName, func() mutator.Mutator { return &localConstSwap{} })
}

// comparisonOps is the set of relational and equality operators
// [localConstSwap] restricts its scan to: the same "boundary-relevant"
// family operator/boundary targets (<, <=, >, >=), plus == and != per
// ROADMAP.md's v2 sketch. This is where a wrong-constant substitution
// actually changes observable behaviour — the strconv ParseUint bug this
// operator is modelled on was exactly a boundary check, `n1 > maxVal`
// silently becoming `n1 > maxUint64`. Restricting to comparison operands
// (rather than, say, every identifier use of matching type) is also the
// operator's primary defence against combinatorial explosion: only
// identifiers that are themselves operands of a *specific* comparison node
// are ever considered, one AST node at a time, so a function with many
// unrelated identifier references never contributes candidates it isn't
// directly comparing against something.
var comparisonOps = map[token.Token]bool{
	token.LSS: true,
	token.LEQ: true,
	token.GTR: true,
	token.GEQ: true,
	token.EQL: true,
	token.NEQ: true,
}

// localConstSwap swaps a use of a function-local variable, when it appears
// as an operand of a comparison, for a package-level constant whose type is
// compatible with the variable's declared type. It is the v2 extension
// ROADMAP.md's gap 1 documents as a follow-on to [constSwap]: v1 only swaps
// one package-level constant for another; this operator additionally
// reaches into the historical strconv#21278 ParseUint shape, where the bug
// was a bitSize-scoped local (`maxVal`) used where the package-level
// constant `maxUint64` should have been.
//
// # Why "compatible" is not always types.Identical
//
// v1's package-const-for-const swap requires an exact [types.Identical]
// match, and the same rule is used here whenever the candidate constant was
// given an explicit type in its declaration (e.g. `const limit uint64 =
// 100`). But the strconv fixture this operator is validated against declares
// its constant as `const maxUint64 = (1<<64 - 1)` — no explicit type — and
// for an untyped constant declaration, go/types records [types.Const.Type]
// as one of the untyped basic kinds (here, untyped int), never the type the
// constant happens to fit into at any particular use site. types.Identical
// between "untyped int" and "uint64" is false, so a literal port of v1's
// exact-match rule would silently fail to reproduce the one bug shape this
// whole operator exists to close.
//
// The fix is not to loosen this to [types.AssignableTo]: that function
// reports whether V's *type* is assignable to T without ever consulting the
// constant's actual value, so it treats every untyped integer constant as
// assignable to every integer-kinded variable regardless of magnitude or
// sign — verified empirically (see the constswap_test.go case built around
// it): types.AssignableTo(negative-untyped-int, uint64) reports true, but
// `var x uint64 = -1` is a real compile error ("constant -1 overflows
// uint64"). Offering that swap would be a guaranteed-uncompilable mutant
// this operator could have avoided, contradicting the "avoid
// guaranteed-uncompilable mutants where the operator can tell in advance"
// standard v1 already holds itself to (see this file's package doc).
//
// Instead, for an untyped candidate constant, [representable] replicates
// (a deliberately narrow slice of) the compiler's own constant-overflow
// check directly against the constant's actual value and the variable's
// underlying basic kind — see representable's own doc for exactly what it
// does and does not cover.
//
// # Why the candidate set is restricted to the local's own file
//
// An earlier version of this operator had no such restriction — every
// package-level constant whose type was compatible with the local's
// declared type was a candidate, on the theory that a local has no natural
// "block" to share with a package constant the way two package constants
// might. That theory was validated against the real, frozen
// corpus/stdlib-strconv-parseuint fixture (a 4-file slice of real strconv
// source) during ROADMAP.md gap 1's own flagged validation pass, and it
// failed: mutant count on that fixture went up roughly 24x, and the large
// majority of the new mutants were nonsense, not "wrong identifier" bugs —
// e.g. quote.go's loop-index locals (`r`, `i`, `j`, `rr`) being offered
// against `intSize`/`IntSize` (declared in atoi.go) and `nSmalls` (declared
// in itoa.go), three unrelated files with no conceptual relationship to a
// rune-quoting loop's index variables. Package-wide, untyped integer
// constants are common enough, and integer comparisons common enough, that
// "type-compatible" alone is nowhere near "plausibly confusable" once a
// package spans more than one file.
//
// The fix restricts candidates to package-level constants declared in the
// *same file* as the local variable's use — the same file-scoped fallback
// v1's own same-block-or-file rule already uses for a constant that isn't
// part of a parenthesized block. This keeps the strconv reproduction intact
// (`maxUint64`, `maxVal`, and the `n1 > maxVal` comparison are all declared
// in atoi.go) while eliminating the cross-file noise entirely — quote.go's
// locals no longer see atoi.go's or itoa.go's constants as candidates at
// all, since [fileOf] resolves file identity from position ranges (this
// operator has no *token.FileSet to consult directly — TypedMutator's
// WithScope signature deliberately keeps to type information only — so
// identity comes from each *ast.File's own [ast.File.Pos]/[ast.File.End]
// range instead, which info.Scopes' *ast.File keys already provide).
//
// This is a real, load-bearing restriction, not a defensive nicety: without
// it, this operator would ship default-on and measurably degrade the
// mutant-to-signal ratio on any real multi-file package, which is exactly
// the risk this section originally flagged as unvalidated and is now
// validated to be real.
//
// # Why "local variable" excludes package-level and file-scope vars
//
// The strconv shape is specifically a *local* mistakenly standing in for a
// constant; a package-level variable of the same type swapped for a
// same-type package-level constant is a different (and already-questionable
// — why would you compare a var against itself via a differently-named
// constant?) mutation shape this operator does not attempt. [localVarUse]
// excludes any *types.Var whose Parent scope is the package scope, and
// excludes struct fields (accessed through a selector, not a bare
// identifier use, so they are already excluded by node shape — the
// IsField() check is defence in depth against a future info.Uses edge
// case, not something the current node-shape restriction alone strictly
// requires).
//
// Like [constSwap], the zero value is inert: Applies always reports false
// until [localConstSwap.WithScope] returns a package-bound instance.
type localConstSwap struct {
	info *types.Info
	pkg  *types.Package

	// constsByFile maps each *ast.File to the package-level constants it
	// declares, sorted by name for a deterministic Mutate order — see "Why
	// the candidate set is restricted to the local's own file" above for why
	// this is keyed by file rather than being one flat, package-wide slice.
	constsByFile map[*ast.File][]*types.Const
}

// Name reports the operator's registry name.
func (*localConstSwap) Name() string { return LocalConstSwapName }

// Applies reports whether node is a comparison whose left or right operand
// is a local variable with at least one type-compatible package-level
// constant to swap in.
func (l *localConstSwap) Applies(node ast.Node) bool {
	if l.info == nil {
		return false
	}

	expr, ok := node.(*ast.BinaryExpr)
	if !ok || !comparisonOps[expr.Op] {
		return false
	}

	return len(l.candidatesFor(expr.X)) > 0 || len(l.candidatesFor(expr.Y)) > 0
}

// Mutate returns one mutation per (eligible operand, candidate constant)
// pair found on node's two sides. Both operands are considered
// independently — a comparison between two locals that are each eligible
// (e.g. `n1 > maxVal` where both n1 and maxVal happen to share a
// package-constant-compatible type) yields mutations for both, since each
// is, on its own, the same "wrong identifier used at a boundary" shape this
// operator targets.
func (l *localConstSwap) Mutate(node ast.Node) []mutator.Mutation {
	if l.info == nil {
		return nil
	}

	expr, ok := node.(*ast.BinaryExpr)
	if !ok || !comparisonOps[expr.Op] {
		return nil
	}

	mutations := l.operandMutations(expr.X)
	mutations = append(mutations, l.operandMutations(expr.Y)...)

	return mutations
}

// WithScope returns a new localConstSwap bound to pkg's type information.
// The receiver is not modified, matching [constSwap.WithScope]'s contract.
func (*localConstSwap) WithScope(info *types.Info, pkg *types.Package) mutator.Mutator {
	return &localConstSwap{
		info:         info,
		pkg:          pkg,
		constsByFile: constsByFile(info),
	}
}

// candidatesFor returns the package-level constants, declared in the same
// file as operand's use (see "Why the candidate set is restricted to the
// local's own file" above), compatible with operand's type — or nil if
// operand is not an eligible local variable use, or its file can't be
// resolved.
func (l *localConstSwap) candidatesFor(operand ast.Expr) []*types.Const {
	ident, v, ok := localVarUse(l.info, l.pkg, operand)
	if !ok {
		return nil
	}

	file := fileOf(l.info, ident.Pos())
	if file == nil {
		return nil
	}

	var out []*types.Const

	for _, c := range l.constsByFile[file] {
		if typeMatches(v.Type(), c) {
			out = append(out, c)
		}
	}

	return out
}

// operandMutations returns one mutation per candidate constant for operand,
// each rewriting the operand identifier's Name field directly — the same
// in-place-field-edit idiom [constSwap.Mutate] uses. Node is set explicitly
// to the operand's *ast.Ident, since the node Applies/Mutate were called on
// is the enclosing *ast.BinaryExpr, not the identifier the edit actually
// targets (see mutator.Mutation.Node's doc and, e.g., statement/remover.go
// for the established precedent of a narrower mutation target than the
// walk's outer node).
func (l *localConstSwap) operandMutations(operand ast.Expr) []mutator.Mutation {
	ident, _, ok := localVarUse(l.info, l.pkg, operand)
	if !ok {
		return nil
	}

	candidates := l.candidatesFor(operand)
	if len(candidates) == 0 {
		return nil
	}

	original := ident.Name

	mutations := make([]mutator.Mutation, 0, len(candidates))

	for _, c := range candidates {
		replacement := c.Name()

		mutations = append(mutations, mutator.Mutation{
			Description: original + " -> " + replacement,
			Apply:       func() { ident.Name = replacement },
			Revert:      func() { ident.Name = original },
			Node:        ident,
		})
	}

	return mutations
}

// localVarUse reports whether expr is a use of a function-local variable —
// a parameter, named result, or a variable declared inside a function body
// — as opposed to a package-level variable, a struct field, or anything
// that is not a variable at all (a constant, a function, a type name).
func localVarUse(info *types.Info, pkg *types.Package, expr ast.Expr) (*ast.Ident, *types.Var, bool) {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return nil, nil, false
	}

	v, ok := info.Uses[ident].(*types.Var)
	if !ok || v.IsField() {
		return nil, nil, false
	}

	if v.Parent() == nil || v.Parent() == pkg.Scope() {
		return nil, nil, false
	}

	return ident, v, true
}

// constsByFile groups every package-level constant declared in any file
// reachable through info.Scopes (the same *ast.File-keyed walk
// [buildGroups] uses for v1, minus the block-vs-file distinction v1 needs
// and this operator does not — see [localConstSwap]'s doc for why file
// alone is the right granularity here), keyed by the declaring *ast.File
// and sorted by name within each file for a deterministic Mutate order.
func constsByFile(info *types.Info) map[*ast.File][]*types.Const {
	out := map[*ast.File][]*types.Const{}

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

			for _, spec := range gen.Specs {
				val, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				for _, name := range val.Names {
					if c, ok := info.Defs[name].(*types.Const); ok {
						out[file] = append(out[file], c)
					}
				}
			}
		}
	}

	for file := range out {
		sort.Slice(out[file], func(i, j int) bool {
			return out[file][i].Name() < out[file][j].Name()
		})
	}

	return out
}

// fileOf returns the *ast.File whose source range contains pos, resolved
// from info.Scopes' *ast.File keys' own [ast.File.Pos]/[ast.File.End]
// ranges rather than a *token.FileSet — this operator is never handed one
// directly (TypedMutator.WithScope's signature deliberately keeps to type
// information only), and a FileSet's positions never overlap between
// files, so bounding pos against each candidate file's own range is
// sufficient to identify the right one. Returns nil if pos falls outside
// every known file (should not happen for a position obtained from an
// identifier this same info actually resolved, but Applies/Mutate treat
// nil as "no eligible candidates" rather than panicking).
func fileOf(info *types.Info, pos token.Pos) *ast.File {
	for node := range info.Scopes {
		file, ok := node.(*ast.File)
		if ok && file.Pos() <= pos && pos < file.End() {
			return file
		}
	}

	return nil
}

// typeMatches reports whether c is a viable swap-in for a variable of type
// varType. An explicitly-typed constant must match exactly
// ([types.Identical]), the same standard [constSwap] holds package-level
// const-for-const swaps to. An untyped constant — which has no fixed
// declared type to compare identically against — is instead checked for
// [representable]ility against varType's underlying basic kind, which is
// the closest available proxy for "would the compiler accept this constant
// where this variable currently is."
func typeMatches(varType types.Type, c *types.Const) bool {
	constType := c.Type()

	basic, untyped := constType.(*types.Basic)
	if untyped && basic.Info()&types.IsUntyped != 0 {
		varBasic, ok := varType.Underlying().(*types.Basic)

		return ok && representable(c.Val(), varBasic)
	}

	return types.Identical(varType, constType)
}

// representable reports whether val — an untyped constant's value — fits in
// basic without the compiler rejecting the assignment as a constant
// overflow, replicating (a deliberately narrow slice of) go/types' own
// constant-conversion rules well enough for the integer case this operator
// cares about.
//
// Only integer constants (constant.Int) are handled. Floats, complexes,
// strings, bools, and any basic kind this switch does not list are
// conservatively reported as not representable — this operator simply never
// offers those pairings, rather than risking a mutation that turns out to be
// a guaranteed compile failure. The strconv ParseUint case study this whole
// operator is modelled on is itself all-integer (`maxVal`, `maxUint64` are
// both integer-kinded), so this scope covers the motivating case without
// reimplementing go/types' full, much larger constant-conversion surface.
//
// Int and Uint are assumed to be 64-bit. This matches every mainstream
// build target Go currently ships for, but is not universally true (Go has,
// historically, supported 32-bit-int platforms) — on such a platform this
// heuristic could offer a swap that overflows there but not on the
// (assumed) 64-bit platform it was evaluated against. The engine's existing
// fail-soft NotViable classification (a mutant that doesn't compile is
// recorded, not fatal) is exactly the safety net this residual imprecision
// relies on, the same way v1 relies on it for the cases its own
// exact-type-match rule doesn't fully preempt.
func representable(val constant.Value, basic *types.Basic) bool {
	if val.Kind() != constant.Int {
		return false
	}

	switch basic.Kind() {
	case types.Int, types.Int64:
		return inSignedRange(val, math.MinInt64, math.MaxInt64)
	case types.Int8:
		return inSignedRange(val, math.MinInt8, math.MaxInt8)
	case types.Int16:
		return inSignedRange(val, math.MinInt16, math.MaxInt16)
	case types.Int32:
		return inSignedRange(val, math.MinInt32, math.MaxInt32)
	case types.Uint, types.Uint64, types.Uintptr:
		return inUnsignedRange(val, math.MaxUint64)
	case types.Uint8:
		return inUnsignedRange(val, math.MaxUint8)
	case types.Uint16:
		return inUnsignedRange(val, math.MaxUint16)
	case types.Uint32:
		return inUnsignedRange(val, math.MaxUint32)
	default:
		return false
	}
}

// inSignedRange reports whether val fits in a signed integer type whose
// range is [minimum, maximum]. It exists to keep [representable]'s switch to
// one call per case instead of an inline "convert, then bounds-check" pair
// per case, which is what tipped gocyclo over this codebase's complexity
// budget the first time this was written.
func inSignedRange(val constant.Value, minimum, maximum int64) bool {
	v, ok := constant.Int64Val(val)

	return ok && v >= minimum && v <= maximum
}

// inUnsignedRange is [inSignedRange] for unsigned integer types, whose range
// is always [0, maximum] — constant.Uint64Val itself already reports false
// for a negative val, so there is no lower bound to check here.
func inUnsignedRange(val constant.Value, maximum uint64) bool {
	v, ok := constant.Uint64Val(val)

	return ok && v <= maximum
}
