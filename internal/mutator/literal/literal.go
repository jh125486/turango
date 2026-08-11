// Package literal implements mutation operators that change a literal value
// in source rather than swapping an operator or removing a statement: a
// numeric constant shifted — an integer by one in each direction (the
// classic off-by-one — `x < 0` becomes `x < 1`), a float by a small
// relative nudge in each direction (`x < 0.95` becomes `x < 0.95095`) — or
// a boolean literal flipped (`true` becomes `false`).
//
// These are a different failure mode from the token-swap operators in
// package operator: a boundary shift (operator/boundary) moves which
// *comparison* is used, while literal/number moves the *threshold value*
// itself — `x < n` mutating to `x < n+1` rather than to `x <= n`. Go's
// strconv.ParseUint had a real, shipped bug of almost this shape (a wrong
// constant substituted for the correct one), which none of turango's
// token-swap operators can reproduce; literal/number narrows that gap for
// the case where the fix is a numeric shift. A true identifier-swap operator
// (substituting one same-type named constant for another, as the real
// ParseUint bug actually was) is a separate, larger undertaking — it needs
// go/types, not just go/ast, to know which identifiers are substitutable —
// and is not implemented here.
//
// Importing the package registers "literal/number" and "literal/boolean"
// with the [mutator] registry.
package literal
