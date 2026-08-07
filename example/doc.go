// Package example is a small, deliberately imperfect package used to
// demonstrate turango end to end.
//
// Nothing in turango imports it. It exists to be mutated:
//
//	turango test -mutate=. -mutatescope=package ./example
//
// (-mutate is a function-name regexp, like -run/-bench/-fuzz — "." matches
// every function; ./example is the ordinary trailing package argument.)
//
// The code is ordinary order-pricing arithmetic — conditionals, loops, boolean
// logic, compound assignment, increments — chosen so that most of turango's
// operators have something to bite on. The tests are ordinary
// too, and that is the point: they are the kind of tests a careful person
// writes and they still leave gaps, which turango reports as surviving mutants.
// A package that scored 100% would demonstrate nothing.
//
// Two //nomutant directives are included as living documentation of the
// suppression feature:
//
//   - [RestockingFeeCents] suppresses a single statement, whose arithmetic
//     encodes a contractual constant rather than logic a test could meaningfully
//     pin down; and
//   - [Sum] suppresses a compound statement, and with it everything nested
//     inside — the condition, the branch and the assignment in its body — which
//     is the cascading behaviour a directive on an `if`, `for` or `switch` has.
//
// See README.md in this directory for a run and its output.
package example
