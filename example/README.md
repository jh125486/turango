# `example/` — turango against a real, imperfect package

This package exists to be mutated. It is ordinary order-pricing code —
conditionals, loops, boolean logic, compound assignment, increments, a `switch`
— written so that every one of turango's nine operators has something to bite
on, with an ordinary test suite that leaves ordinary gaps.

The gaps are the point. A package that scored 100% would demonstrate nothing;
what mutation testing is *for* is finding the assertions a careful reviewer
still misses.

## Running it

From the repository root:

```
turango test -mutate=./example/... -mutatescope=package
```

`-mutatescope=package` matters here and is worth understanding. The default
scope runs the whole module's tests (`go test ./...`) against every mutant,
which for a package living inside the turango repo means re-running turango's
own end-to-end test suite — itself a mutation runner — 66 times. Package scope
runs only `./example`'s tests, which is what actually judges this code, and
finishes in under 30 seconds. On a normal repository the default full scope is
the right one: it is the only scope that catches a mutant killed by a
*neighbouring* package's tests.

## What it prints

Real output, unedited:

```
mutants:    66
killed:     53
survived:   9
not-viable: 4

score:      85.5% (53 killed of 62 viable)
suppressed: 2 of 64 nodes (3.1% excluded from the score by //nomutant)

Surviving mutants (9):
  example/pricing.go:81   control/if         remove if body
  example/pricing.go:116  control/if         remove if body
  example/pricing.go:116  statement/remover  remove statement: discount = subtotal
  example/stats.go:62     control/if         remove if body
  example/stats.go:62     statement/remover  remove statement: low = value
  example/stats.go:83     control/case       remove case body
  example/stats.go:83     statement/remover  remove statement: down++
  example/stats.go:84     operator/inc_dec   ++ -> --
  example/stats.go:92     control/if         remove if body
```

Only survivors are listed, because they are the only actionable result. A killed
mutant is the suite doing its job, and a not-viable one is an operator producing
code that does not compile, which says nothing about the tests either way.

Every one of those nine is a genuine hole in `pricing_test.go` /
`stats_test.go`, and each is fixable with a single test case:

| Survivor | The missing test |
| --- | --- |
| `pricing.go:81` | `DiscountCents` with `CouponMember` and `member == false` — the non-member path is never exercised, so deleting it changes nothing. |
| `pricing.go:116` | `Total`'s defensive clamp of a discount larger than the subtotal. No coupon can currently produce one, so the guard is unreachable — arguably the mutant is telling the truth and the guard should go. |
| `stats.go:62` | `Bounds` with a value *below* the first one. Every fixture rises, so `low` is never updated. |
| `stats.go:83`, `:84` | `Trend` on a falling series. Nothing counts down-steps, so the `down++` case can be deleted, or turned into `down--`, unnoticed. |
| `stats.go:92` | ... and therefore the `"falling"` branch is dead in the tests too. |

## What the `//nomutant` directives demonstrate

Two directives, of the two shapes that behave differently:

- **A single statement** — `RestockingFeeCents` in `pricing.go`. The directive
  sits above one `return`, and suppresses only the arithmetic in it. Mutating a
  contractual percentage just re-asserts the number in a second place; it cannot
  find a missing test.

- **A compound statement, cascading** — the saturating-overflow guard in `Sum`
  in `stats.go`. The directive is anchored to the `if`, and suppression
  *cascades*: the condition, both of its comparisons, the `&&` itself, the
  assignment in the body and the body's own removal are all skipped, from one
  comment. Reaching that branch needs a slice no unit test can afford to build,
  so every mutant in it would survive as noise.

Both show up in the JSON report under `Suppressions`, with the reason attached:

```
turango test -mutate=./example/... -mutatescope=package -mutateoutput=/tmp/turango
```

```json
"Suppressions": [
  {
    "File": "example/pricing.go",
    "Line": 133,
    "Reason": "the fee is a contractual percentage, not logic — mutating the arithmetic only re-asserts the number in a second place"
  },
  {
    "File": "example/stats.go",
    "Line": 14,
    "Reason": "saturating-overflow guard — reaching it needs a slice no unit test can afford to build, so a surviving mutant here would be noise"
  }
]
```

No mutant is reported at either location — the walk stops there rather than
mutating and running, so a suppressed node costs nothing and proves nothing.

### What the two directives actually cost

Delete both `//nomutant` lines and run again:

```
mutants:    76
killed:     58
survived:   14
not-viable: 4

score:      80.6% (58 killed of 72 viable)
suppressed: 0 of 72 nodes (0.0% excluded from the score by //nomutant)
```

Ten more mutants: two from the single suppressed `return`, and eight from the
cascade — the `&&` and both of its comparisons, the `-`, the branch body, the
assignment in it, and the body's removal, all from one comment on the `if`.

And the score goes **up** with the directives in place, 80.6% → 85.5%, without
one line of the package or its tests changing. That is the whole reason the
summary prints the suppression ratio next to the score: suppressed nodes leave
the score's denominator, so a liberal `//nomutant` habit inflates the number the
same way a `// nocoverage` pragma games a coverage report. 3.1% is a rounding
error and the 85.5% means something; at 30% it would not.
