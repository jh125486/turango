// Package operator implements the token-level mutation operators: the mutators
// that change a program's meaning by substituting one Go operator for another
// (`+` for `-`, `==` for `!=`, `++` for `--`) or by stripping a unary operator
// off the expression it negates.
//
// Importing the package registers "operator/assignment", "operator/binary",
// "operator/inc_dec" and "operator/unary" with the [mutator] registry.
//
// # Swap tables
//
// The three swapping operators share the table-building machinery below rather
// than each carrying an inline switch. A swap table is a
// map[token.Token]token.Token: a token present as a key is eligible for
// mutation and swaps to exactly the token it maps to, and a token absent from
// the table is not eligible at all. That absence is how `:=` (token.DEFINE) and
// `=` (token.ASSIGN) are excluded from assignment mutation — they are simply
// never added — so eligibility needs no second predicate that could drift out
// of sync with the table itself.
//
// Most swaps are conceptually bidirectional (`+=` becomes `-=` and `-=` becomes
// `+=`), so they are declared once as a pair and expanded into two entries. A
// few are deliberately one-directional, ported as-is from go-turango: `%=`
// becomes `/=` but `/=` does not become `%=`, because `/=` already has the more
// natural partner `*=`. Declaring the two kinds separately keeps that
// asymmetry visible instead of hiding it in a hand-written map literal where a
// missing reverse entry looks like a typo.
package operator

import (
	"fmt"
	"go/token"
)

// Registry names. These are the exact strings a user passes to
// -mutateoperators, so they are declared once and reused by each mutator's
// init and Name method.
const (
	assignmentName = "operator/assignment"
	binaryName     = "operator/binary"
	boundaryName   = "operator/boundary"
	incDecName     = "operator/inc_dec"
	unaryName      = "operator/unary"
)

// assignmentSwaps is the swap table for compound assignment operators, applied
// to an [go/ast.AssignStmt]'s Tok.
//
// token.DEFINE (`:=`) and token.ASSIGN (`=`) are intentionally absent: swapping
// them produces either a compile error or an identical program, never an
// interesting mutant.
var assignmentSwaps = buildSwaps(
	[][2]token.Token{
		{token.ADD_ASSIGN, token.SUB_ASSIGN}, // += <-> -=
		{token.MUL_ASSIGN, token.QUO_ASSIGN}, // *= <-> /=
		{token.AND_ASSIGN, token.OR_ASSIGN},  // &= <-> |=
		{token.SHL_ASSIGN, token.SHR_ASSIGN}, // <<= <-> >>=
	},
	[][2]token.Token{
		{token.REM_ASSIGN, token.QUO_ASSIGN},     // %= -> /=
		{token.XOR_ASSIGN, token.AND_ASSIGN},     // ^= -> &=
		{token.AND_NOT_ASSIGN, token.XOR_ASSIGN}, // &^= -> ^=
	},
)

// binarySwaps is the swap table for binary operators, applied to an
// [go/ast.BinaryExpr]'s Op. It covers the arithmetic, bitwise, logical and
// comparison operators, and mirrors assignmentSwaps for the operators the two
// have in common.
//
// Swapping `&&` for `||` overlaps in spirit with the expression/remove
// operator, which eliminates one operand of a short-circuit expression, but the
// two produce different mutants — a token swap versus an operand elimination —
// so both exist independently.
var binarySwaps = buildSwaps(
	[][2]token.Token{
		{token.ADD, token.SUB},  // + <-> -
		{token.MUL, token.QUO},  // * <-> /
		{token.AND, token.OR},   // & <-> |
		{token.SHL, token.SHR},  // << <-> >>
		{token.LAND, token.LOR}, // && <-> ||
		{token.EQL, token.NEQ},  // == <-> !=
		{token.LSS, token.GEQ},  // < <-> >=
		{token.GTR, token.LEQ},  // > <-> <=
	},
	[][2]token.Token{
		{token.REM, token.QUO},     // % -> /
		{token.XOR, token.AND},     // ^ -> &
		{token.AND_NOT, token.XOR}, // &^ -> ^
	},
)

// boundarySwaps is the swap table for relational-operator boundary shifts,
// applied to an [go/ast.BinaryExpr]'s Op: `<` becomes `<=` and vice versa,
// `>` becomes `>=` and vice versa.
//
// This is a different mutation shape from binarySwaps' `<` <-> `>=` and `>`
// <-> `<=`: binarySwaps negates a comparison (flips which branch runs for
// every input), while a boundary shift moves the threshold by exactly one
// admitted value (`x < n` and `x <= n` differ only for `x == n`) — the
// classic off-by-one mutant, and a distinct enough failure mode that mutation
// testing literature (e.g. PIT's Conditionals Boundary Mutator) treats it as
// its own operator rather than folding it into a general relational swap.
var boundarySwaps = buildSwaps(
	[][2]token.Token{
		{token.LSS, token.LEQ}, // < <-> <=
		{token.GTR, token.GEQ}, // > <-> >=
	},
	nil,
)

// incDecSwaps is the swap table for the increment and decrement statements,
// applied to an [go/ast.IncDecStmt]'s Tok.
var incDecSwaps = buildSwaps(
	[][2]token.Token{
		{token.INC, token.DEC}, // ++ <-> --
	},
	nil,
)

// buildSwaps assembles a swap table from its two declarative halves.
//
// Each pair in bidirectional contributes two entries, one per direction. Each
// pair in oneWay contributes only the forward entry, so the target token keeps
// whatever mapping it has of its own.
//
// It panics on a duplicate key, which can only mean the tables above disagree
// about what some token swaps to. That is a programmer error detectable at
// process start, so it follows the same fail-fast convention as
// [mutator.Register].
func buildSwaps(bidirectional, oneWay [][2]token.Token) map[token.Token]token.Token {
	swaps := make(map[token.Token]token.Token, 2*len(bidirectional)+len(oneWay))

	add := func(from, to token.Token) {
		if existing, dup := swaps[from]; dup {
			panic(fmt.Sprintf("operator: conflicting swap for %s: already %s, also %s", from, existing, to))
		}

		swaps[from] = to
	}

	for _, pair := range bidirectional {
		add(pair[0], pair[1])
		add(pair[1], pair[0])
	}

	for _, pair := range oneWay {
		add(pair[0], pair[1])
	}

	return swaps
}

// describeSwap renders a swap the way reports show it, e.g. "+= -> -=".
func describeSwap(from, to token.Token) string {
	return from.String() + " -> " + to.String()
}
