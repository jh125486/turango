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
mutants:    197
killed:     144
survived:   40
not-viable: 13

score:      78.3% (144 killed of 184 viable)
suppressed: 2 of 186 nodes (1.1% excluded from the score by //nomutant)

Surviving mutants (40):
  0b5b7cc3061b  example/pricing.go:9    literal/number             5000 -> 4999
  63f5430e8c03  example/pricing.go:9    literal/number             5000 -> 5001
  983b6a64d303  example/pricing.go:54   literal/number             0 -> -1
  04f3d8ba7fc5  example/pricing.go:54   literal/number             0 -> 1
  b7845c7a68ad  example/pricing.go:54   operator/boundary          <= -> <
  265f9701cdc6  example/pricing.go:74   literal/number             1000 -> 1001
  db13758d0873  example/pricing.go:74   operator/boundary          < -> <=
  3d8204453987  example/pricing.go:81   control/if                 remove if body
  45665faa5423  example/pricing.go:82   literal/number             0 -> -1
  0da641aff1cc  example/pricing.go:82   literal/number             0 -> 1
  584e0fd73fe1  example/pricing.go:97   operator/boundary          >= -> >
  231d5d93b248  example/pricing.go:116  control/if                 remove if body
  7c1f728ca35e  example/pricing.go:116  identifier/localconstswap  discount -> expeditedSurchargeCents
  93f11246c004  example/pricing.go:116  identifier/localconstswap  discount -> freeShippingCents
  433865f29289  example/pricing.go:116  identifier/localconstswap  discount -> restockingFeePercent
  507a886d1251  example/pricing.go:116  identifier/localconstswap  discount -> standardShippingCents
  bc6db5820c80  example/pricing.go:116  identifier/localconstswap  subtotal -> freeShippingCents
  4979c1438f04  example/pricing.go:116  operator/boundary          > -> >=
  0eb817c6aace  example/pricing.go:116  statement/remover          remove statement: discount = subtotal
  ce375fb68906  example/pricing.go:128  literal/number             0 -> -1
  6aac755ee5bb  example/pricing.go:128  literal/number             0 -> 1
  75072c30c97a  example/pricing.go:128  operator/boundary          <= -> <
  a30d11dbb9bd  example/stats.go:6      literal/number             1 -> 2
  5204d96f8f05  example/stats.go:6      literal/number             40 -> 39
  615922a2beec  example/stats.go:6      literal/number             40 -> 41
  2dcf383d58fc  example/stats.go:61     literal/number             1 -> 0
  0649693698dc  example/stats.go:62     control/if                 remove if body
  416d7b1dd928  example/stats.go:62     identifier/localconstswap  value -> maxSafeTotal
  5e2ba056cb5e  example/stats.go:62     operator/boundary          < -> <=
  38604501f172  example/stats.go:62     statement/remover          remove statement: low = value
  0b82070b125f  example/stats.go:64     operator/boundary          > -> >=
  e7871e70947a  example/stats.go:86     literal/number             1 -> 2
  a11490b4af0c  example/stats.go:90     control/case               remove case body
  e4f071faf731  example/stats.go:90     literal/number             1 -> 0
  e3f7b0204c2e  example/stats.go:90     statement/remover          remove statement: down++
  dcab9b515e13  example/stats.go:91     operator/inc_dec           ++ -> --
  8fef4d093cbb  example/stats.go:99     control/if                 remove if body
  d598ec3be6ab  example/stats.go:99     identifier/localconstswap  up -> maxSafeTotal
  7c9e7c75e671  example/stats.go:100    identifier/constswap       TrendFalling -> TrendFlat
  f3907e7d2e33  example/stats.go:100    identifier/constswap       TrendFalling -> TrendRising
```

Only survivors are listed, because they are the only actionable result. A killed
mutant is the suite doing its job, and a not-viable one is an operator producing
code that does not compile, which says nothing about the tests either way. Each
row's leading hex string is the mutant's stable, content-hashed ID — reproduce
one exactly with `-mutatemutant=<ID>`.

The survivor list has grown since operators were added beyond the original
nine (`operator/boundary`'s off-by-one shifts, `literal/number`'s ±1 literal
shifts, and the `identifier/constswap`/`identifier/localconstswap` pair's
const-for-const and local-var-for-const swaps account for most of the new
entries above) — the shape of the finding is the same either way: each
survivor is a boundary, branch, or constant choice nothing asserts on. A
representative sample, each fixable with a single test case:

| Survivor | The missing test |
| --- | --- |
| `pricing.go:81` | `DiscountCents` with `CouponMember` and `member == false` — the non-member path is never exercised, so deleting it changes nothing. |
| `pricing.go:116` | `Total`'s defensive clamp of a discount larger than the subtotal. No coupon can currently produce one, so the guard is unreachable — arguably the mutant is telling the truth and the guard should go. `identifier/localconstswap` finds the same gap five different ways, swapping `discount` for each sibling cents/percent constant in scope. |
| `pricing.go:54`, `:74`, `:97`, `:128` | Boundary values (`<=` vs `<`, `>=` vs `>`) at the exact threshold are never tested — every fixture picks a value clearly on one side. |
| `stats.go:62` | `Bounds` with a value *below* the first one. Every fixture rises, so `low` is never updated; `identifier/localconstswap` also offers `value -> maxSafeTotal` at the same comparison. |
| `stats.go:90`, `:91` | `Trend` on a falling series. Nothing counts down-steps, so the `down++` case can be deleted, or turned into `down--`, unnoticed. |
| `stats.go:99`, `:100` | ... and therefore the `TrendFalling` branch, and its constant, are dead in the tests too — `identifier/localconstswap` (`up -> maxSafeTotal`) and `identifier/constswap` (`TrendFalling -> TrendFlat`/`TrendRising`) both land here. |

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
mutants:    218
killed:     155
survived:   50
not-viable: 13

score:      75.6% (155 killed of 205 viable)
suppressed: 0 of 205 nodes (0.0% excluded from the score by //nomutant)
```

Twenty-one more mutants: seven from the single suppressed `return`
(`RestockingFeeCents`'s arithmetic — `identifier/constswap` alone now offers
three sibling-constant swaps there, on top of the literal and token-swap
mutations the original two-mutant estimate accounted for), and fourteen from
the cascade on `Sum`'s saturating-overflow guard — every operator that can
touch an `if` condition, a comparison, a compound-assignment, or the body's
own removal or individual statements now gets a shot at that one `if` block,
since the directive that used to skip the whole subtree is gone.

And the score goes **up** with the directives in place, 75.6% → 78.3%,
without one line of the package or its tests changing. That is the whole
reason the summary prints the suppression ratio next to the score: suppressed
nodes leave the score's denominator, so a liberal `//nomutant` habit inflates
the number the same way a `// nocoverage` pragma games a coverage report.
1.1% is a rounding error and the 78.3% means something; at 30% it would not.
