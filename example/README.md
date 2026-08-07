# `example/` — turango against a real, imperfect package

This package exists to be mutated. It is ordinary order-pricing code —
conditionals, loops, boolean logic, compound assignment, increments, a `switch`
— written so that most of turango's operators have something to bite on, with
an ordinary test suite that leaves ordinary gaps.

The gaps are the point. A package that scored 100% would demonstrate nothing;
what mutation testing is *for* is finding the assertions a careful reviewer
still misses.

## Running it

From the repository root:

```
turango test -mutate=. -mutatescope=package ./example
```

`-mutate` behaves like `-run`/`-bench`/`-fuzz`: its value is a function-name
regexp, not a package selector — `.` matches every function. `./example` is
the ordinary trailing package argument, exactly as with those three flags.
`./example`, not `./example/...`, matters here too: the `...` wildcard would
also sweep in `example/legacy/`, a separate, unrelated demo package one
directory down.

`-mutatescope=package` matters and is worth understanding. The default scope
runs the whole module's tests (`go test ./...`) against every mutant, which
for a package living inside the turango repo means re-running turango's own
end-to-end test suite — itself a mutation runner — once per mutant. Package
scope runs only `./example`'s own tests, which is what actually judges this
code, and finishes quickly. On a normal repository the default full scope is
the right one: it is the only scope that catches a mutant killed by a
*neighbouring* package's tests.

## What it prints

Real output, unedited:

```
mutants:    160
killed:     117
survived:   31
not-viable: 12

score:      79.1% (117 killed of 148 viable)
suppressed: 2 of 150 nodes (1.3% excluded from the score by //nomutant)

Surviving mutants (31):
  example/pricing.go:9    literal/number     5000 -> 4999
  example/pricing.go:9    literal/number     5000 -> 5001
  example/pricing.go:54   literal/number     0 -> -1
  example/pricing.go:54   literal/number     0 -> 1
  example/pricing.go:54   operator/boundary  <= -> <
  example/pricing.go:74   literal/number     1000 -> 1001
  example/pricing.go:74   operator/boundary  < -> <=
  example/pricing.go:81   control/if         remove if body
  example/pricing.go:82   literal/number     0 -> -1
  example/pricing.go:82   literal/number     0 -> 1
  example/pricing.go:97   operator/boundary  >= -> >
  example/pricing.go:116  control/if         remove if body
  example/pricing.go:116  operator/boundary  > -> >=
  example/pricing.go:116  statement/remover  remove statement: discount = subtotal
  example/pricing.go:128  literal/number     0 -> -1
  example/pricing.go:128  literal/number     0 -> 1
  example/pricing.go:128  operator/boundary  <= -> <
  example/stats.go:6      literal/number     1 -> 2
  example/stats.go:6      literal/number     40 -> 39
  example/stats.go:6      literal/number     40 -> 41
  example/stats.go:61     literal/number     1 -> 0
  example/stats.go:62     control/if         remove if body
  example/stats.go:62     operator/boundary  < -> <=
  example/stats.go:62     statement/remover  remove statement: low = value
  example/stats.go:64     operator/boundary  > -> >=
  example/stats.go:79     literal/number     1 -> 2
  example/stats.go:83     control/case       remove case body
  example/stats.go:83     literal/number     1 -> 0
  example/stats.go:83     statement/remover  remove statement: down++
  example/stats.go:84     operator/inc_dec   ++ -> --
  example/stats.go:92     control/if         remove if body
```

Only survivors are listed, because they are the only actionable result. A killed
mutant is the suite doing its job, and a not-viable one is an operator producing
code that does not compile, which says nothing about the tests either way.

The survivor list has grown since operators were added beyond the original
nine (`operator/boundary`'s off-by-one shifts and `literal/number`'s ±1
literal shifts account for most of the new entries above) — the shape of the
finding is the same either way: each survivor is a boundary or branch nothing
asserts on. A representative sample, each fixable with a single test case:

| Survivor | The missing test |
| --- | --- |
| `pricing.go:81` | `DiscountCents` with `CouponMember` and `member == false` — the non-member path is never exercised, so deleting it changes nothing. |
| `pricing.go:116` | `Total`'s defensive clamp of a discount larger than the subtotal. No coupon can currently produce one, so the guard is unreachable — arguably the mutant is telling the truth and the guard should go. |
| `pricing.go:54`, `:74`, `:97`, `:128` | Boundary values (`<=` vs `<`, `>=` vs `>`) at the exact threshold are never tested — every fixture picks a value clearly on one side. |
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
turango test -mutate=. -mutatescope=package -mutateoutput=/tmp/turango ./example
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
mutants:    179
killed:     127
survived:   40
not-viable: 12

score:      76.0% (127 killed of 167 viable)
suppressed: 0 of 167 nodes (0.0% excluded from the score by //nomutant)
```

Nineteen more mutants: two from the single suppressed `return`
(`RestockingFeeCents`'s arithmetic), and seventeen from the cascade on
`Sum`'s saturating-overflow guard — every operator that can touch an `if`
condition, a comparison, a compound-assignment, or the body's own removal or
individual statements now gets a shot at that one `if` block, since the
directive that used to skip the whole subtree is gone.

And the score goes **up** with the directives in place, 76.0% → 79.1%,
without one line of the package or its tests changing. That is the whole
reason the summary prints the suppression ratio next to the score: suppressed
nodes leave the score's denominator, so a liberal `//nomutant` habit inflates
the number the same way a `// nocoverage` pragma games a coverage report.
1.3% is a rounding error and the 79.1% means something; at 30% it would not.
