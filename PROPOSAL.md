# Proposal: mutation testing for `go test`

Author: Jacob Hochstetler

Last updated: 2026-08-06

Discussion at: (not yet filed)

## Abstract

Add mutation testing to `go test` as a new flag, `-mutate`, the same way
fuzzing was added as `-fuzz`: not a separate tool bolted onto the ecosystem,
but a first-class peer of `-run`/`-bench`/`-fuzz` that reuses the existing
test-running machinery and workflow developers already know. A working,
validated prototype — [turango](https://github.com/jh125486/turango) — exists
and is the basis for this proposal. Turango is a reference implementation
with only mild, well-understood cost controls (scope narrowing, a
baseline-derived timeout, worker-pool parallelism, opt-in TCE and
dependency-closure copying) — it argues the *mechanism* belongs in the
standard toolchain, not that this is the most efficient possible
implementation of it; see "Costs and risks" below.

Mutation testing measures test-suite *quality*, not code coverage. It
mechanically introduces small, individually reversible changes ("mutants")
into a package's AST — flip `+` to `-`, flip `==` to `!=`, delete a
statement, empty an `if` body — and reruns the test suite against each one.
A mutant the suite catches ("killed") proves the suite actually asserts on
that behavior. A mutant that survives is a gap: code that runs during tests
but that nothing actually checks.

## Background

### The fuzzing precedent

`-fuzz` spent years as a third-party tool (`go-fuzz`) before landing in
stdlib `go test` in Go 1.18. The [design draft for first-class
fuzzing](https://go.googlesource.com/proposal/+/master/design/draft-fuzzing.md)
stated its guiding principle plainly: *"Fuzz testing shouldn't be any more
complicated, or any less feature-complete, than other types of Go testing."*
This proposal follows the identical path and the identical principle for
mutation testing: prove the design as an installable, `go`-compatible tool
first, propose stdlib inclusion once the design has real usage behind it.
turango exists specifically to walk that path.

### An 8-year research arc

The author previously built an earlier prototype, `go-turango` (2018), and
published an academic evaluation of it. Two findings from that paper are
directly relevant and have now been **reproduced with fresh 2026 data**
(see Evidence, below):

- `crypto/aes`'s mutation score fluctuated wildly across early Go versions:
  98.3% (Go 1.0) → 24.8% (1.1) → **0%** (1.2) → recovered to ~26.8% (1.4) →
  held around 24–27% through 1.6.
- `crypto/x509/pkix` scored **0% in every version tested** — the package had
  no tests at all.

Every item in that paper's "Future Work" section is now implemented in
turango: an in-memory diff engine (no more shelling out to `diff`),
coverage-directed test selection (`-mutatescope=impact`), per-operator
enable/disable (`-mutateoperators`), a code-exclusion mechanism
(`//nomutant`), and a CI-gating score threshold (`-mutatemin`).

### Prior art and a currently-open, narrower proposal

Third-party Go mutation testers exist today (`go-mutesting`, `gomu`,
`ooze`), none integrated into `go test` itself.

More directly relevant: **golang/go#75315** ("proposal: cmd/asm: support
for mutation testing"), filed by Filippo Valsorda in September 2025, is
currently open and under active discussion. It proposes `-mutlist`/`-mut`
flags on `cmd/asm` to mutate individual assembly instructions, motivated by
constant-time cryptographic code: branch coverage is meaningless there,
since constant-time implementations deliberately execute every "branch"
via `CMOV`/`ADC` regardless of input, so standard coverage tools can't
distinguish a tested code path from an untested one.

**This proposal is complementary, not competing.** #75315 reaches the
hand-written assembly hot path of a primitive like AES-NI. This proposal
reaches everything around it: key schedule, mode-of-operation glue (CBC,
CTR, GCM), parsing, and every other pure-Go package in `crypto/...` and
beyond that isn't hand-tuned assembly at all. The evidence below shows
both layers currently have real, measurable gaps.

## Evidence (2026 data)

All runs used the current turango prototype against a real checkout of
`golang/go` at HEAD, package-isolated into standalone modules to run
independently of the full stdlib build. Full JSON reports and fixtures are
available on request; summarized here.

### `crypto/x509/pkix` — unchanged in 8 years

Still zero test files. **118 mutants generated, 0 killed, 110 survived, 8
not-viable — 0.0% score.** The exact same finding as the 2018 paper,
reproduced against current HEAD.

### `crypto/aes` — a layer story, not a single number

Go's FIPS-140 module reorganization moved the real implementation to
`crypto/internal/fips140/aes`; the old `crypto/aes` location is now a thin
dispatcher.

- Scoped to only `fips140/aes`'s own test file: **1.4% score** (8/555
  viable mutants killed). Root cause, confirmed by reading the test: it
  checks only static S-box/lookup-table self-consistency and never calls
  `NewCipher`/`Encrypt`/`Decrypt` at all.
- Scoped to the full module, so the outer `crypto/aes` package's real
  FIPS-197 known-answer test vectors get to exercise the inner package:
  **60.0% score** (333/555 killed) — dramatically better, confirming the
  production test suite does real work. But **222 survivors remain**,
  including core statements: all three `rounds = aes128/192/256Rounds`
  assignments, the `expandKeyGeneric` call, an `encryptBlock` call site,
  and 135 combined survivors across the CBC/CTR mode implementations.
  (Caveat, reported honestly: this fixture excluded the GCM package/tests
  and stubbed all hardware-dispatch CPU-feature flags false, so some
  fraction of these 222 are almost certainly reachable only through paths
  this specific fixture didn't include — the number is real but likely
  overstates the true gap somewhat. A full-module run without those
  exclusions is future work.)

The headline isn't either number alone — it's that mutation testing at the
*wrong architectural layer* is nearly useless (1.4%), while the *right*
layer with real tests attached is much better but still leaves core
cipher-logic gaps (60%, 222 survivors). That's a more actionable finding
than either number in isolation, and it's exactly the kind of thing
mutation testing surfaces that code coverage cannot: every one of those
222 survivors almost certainly shows 100% line coverage today.

### A live, real gap found blind (`encoding/base64`)

Separately from the crypto evidence: this prototype was validated against
several historical, already-fixed stdlib bugs by checking out the commit
immediately before each fix and running turango against the era's real
(pre-fix) test suite. In `encoding/base64`, the historical fix for
[#19406](https://github.com/golang/go/issues/19406) (`Decode` reporting a
wrong error index) touched two `CorruptInputError(si - 1)` call sites. Both
were correctly caught (Killed) by the era's tests — mutation testing
correctly showed those specific lines had coverage; the real bug was
input-shape-specific, not a code-logic gap. But a **third, structurally
identical `CorruptInputError(si - 1)` call, in a different branch the
historical fix never touched, had its mutant survive** — a live weak spot
of the exact same shape as the shipped bug, found with no prior knowledge
of the fix. This is direct evidence the technique generalizes beyond
crypto to ordinary stdlib logic bugs.

## Proposal

Add a `-mutate=<regexp>` flag to `go test`, behaving exactly like
`-run`/`-bench`/`-fuzz`: its value is a regular expression matched against
declared function/method names in the target packages, not a package
selector. Package selection is the ordinary trailing package arguments —
entirely separate from the flag's value, the same separation `-run` and
`-fuzz` already have between "which package(s)" and "which named target
within them":

```
go test -mutate=. ./...
```

`-mutate=.` matches every function, mirroring `-bench=.`'s "run every
benchmark" convention — the same way an unset `-run`/`-bench` means "match
nothing narrower," using the flag at all with a maximally permissive
pattern is how you get "everything." Unlike `-fuzz`, whose regexp must
match *exactly one* fuzz target (continuous fuzzing only runs one target at
a time), `-mutate` is a broad matcher like `-run`/`-bench`: mutation testing
naturally wants to mutate every function that matches, not narrow to a
single one.

Sibling flags, mirroring how `-fuzz` has `-fuzztime`/`-fuzzminimizetime`/
`-parallel`:

- `-mutatescope=full|package|impact` — how much of the test suite reruns
  per mutant. `full` (default) reruns everything; `package` scopes to the
  mutated file's own package; `impact` builds a per-test coverage map once
  and only reruns tests that actually cover the mutated line.
- `-mutateoperators=<comma-list>` — restrict which mutation operators run.
- `-mutateparallel=<n>` — bounded worker-pool size for concurrent mutant
  execution (default `GOMAXPROCS`).
- `-mutatetimeout=<duration>` — per-mutant budget. Defaults to a baseline
  measurement (the real suite timed 3×, averaged, scaled by CPU count) so
  a mutation that produces an infinite loop (a very common outcome — e.g.
  `i++` flipped to `i--`) doesn't stall the run to `go test`'s own
  ~10-minute default; a timeout is itself treated as a kill.
- `-mutateoutput=<dir>` — write a JSON report.
- `-mutatemin=<float>` — non-zero exit if the resulting score falls below
  the threshold, for CI gating.
- `-mutatetce=true|false` — [Trivial Compiler Equivalence][TCE] (TCE):
  filter a mutant whose compiled output exactly matches a baseline before
  it reaches the test suite. Off by default, and worth staying off by
  default for most codebases — see "Costs and risks" below for a real
  measurement showing this optimization is a net *loss* for at least one
  real fixture.

A `//nomutant` (and `//nomutant:reason`) source comment suppresses
mutation of the annotated statement, cascading into the body of a
suppressed compound statement (`if`/`for`/`switch`), for intentionally
untestable or already-known-equivalent code — matching the false-positive
suppression convention every mature mutation tester (PIT, Stryker) and
every mature Go linter (`//nolint`, `//lint:ignore`) already has, adapted
to line-based comment scanning rather than `ast.CommentMap`, whose
documented trailing-comment edge cases ([golang/go#21755],
[golang/go#33451]) make it a poor fit for a suppression directive.

[golang/go#21755]: https://github.com/golang/go/issues/21755
[golang/go#33451]: https://github.com/golang/go/issues/33451

### The mutation operators (14, across six packages)

Nine were ported and modernized from the 2018 prototype: `if`/`else`/`case`
body-removal, `&&`/`||` short-circuit-operand elimination, statement
removal, and four token-swap operators (assignment, binary, increment/
decrement, unary-strip) covering the classic arithmetic/relational/logical
scalar mutations. Five more were added since this set was first drafted:
`operator/boundary`, a relational boundary shift (`<`↔`<=`, `>`↔`>=`) — the
classic off-by-one mutant, distinct from `operator/binary`'s negation swap
(`<`↔`>=`) and named to match PIT's "Conditionals Boundary Mutator" (PIT is
cited in Related Work, below); a pair of literal operators, `literal/number`
(shifts an integer literal by ±1, e.g. `x < 0` → `x < 1`, or a float
literal by a small relative nudge in each direction) and `literal/boolean`
(swaps `true`↔`false`); and the identifier-substitution pair described in
the "Known gap ... now closed" paragraph below, `identifier/constswap` and
`identifier/localconstswap`.

**Known gap, found during validation, worth stating up front — now
closed**: this operator set proves "this code path is
exercised," not "this specific input shape is tested." `literal/number`
and `literal/boolean` cover literal-value mutations like `x < 0` →
`x < 1`, but on their own do not reproduce the historical
[strconv#21278](https://github.com/golang/go/issues/21278) `ParseUint`
overflow bug, whose actual shape was a wrong-*identifier* substitution —
the local variable `maxVal` was used where the package-level constant
`maxUint64` should have been. Producing that mutation requires knowing
which in-scope identifiers are type-compatible substitutes, which needs
`go/types`, not just `go/ast`. Two operators now do this:
`identifier/constswap` (package-level const-for-const, same `const(...)`
block or file) and `identifier/localconstswap` (a function-local variable,
restricted to comparison operands, swapped for a type-compatible
package-level constant declared in the same file) — the latter reproduces
this exact bug shape, confirmed by a unit test that type-checks the frozen
historical `strconv` source and asserts `Mutate` offers `maxVal ->
maxUint64` on the real `n1 > maxVal` comparison.

## Rationale

- **It's the same shape as an already-successful precedent.** `-fuzz`
  proved this exact rollout path works: ship as a third-party tool,
  validate the design against real bugs, propose stdlib inclusion once
  there's evidence.
- **It measures something coverage cannot.** Every one of `crypto/aes`'s
  222 full-scope survivors above almost certainly has 100% line coverage
  today. Coverage answers "did this line run"; mutation testing answers
  "does anything notice if this line is wrong."
- **It complements, not duplicates, #75315.** Assembly-level mutation
  testing and Go-source mutation testing reach different, non-overlapping
  code. A crypto package needs both to have real confidence in its test
  suite; neither alone is sufficient. The `crypto/aes` findings
  above are the concrete evidence for exactly that gap.
- **The historical crypto findings are not hypothetical.** `pkix` at 0%,
  unchanged in 8 years, is not a contrived example — it's a real,
  currently-shipping, security-relevant stdlib package with no tests at
  all, and mutation testing is what makes that fact impossible to miss
  (a coverage report on an untested package is simply absent, not a loud
  0%).
- **No major language ships this in its official toolchain today.** Every
  mutation tester surveyed for this proposal — [PIT] (Java), [mutmut]
  (Python), [cargo-mutants] (Rust), [MutFlow] (Kotlin), [Mull] (C/C++,
  LLVM-based), [Stryker] (JavaScript/TypeScript, .NET) — is a third-party
  tool maintained outside its language's own toolchain, none blessed as an
  official, in-the-box feature. Landing `-mutate` would make Go the first
  mainstream language to ship mutation testing as a first-class part of
  its standard toolchain, the same "first to bring it natively" position
  `-fuzz` itself held for coverage-guided fuzzing when it landed in 1.18.

[PIT]: https://pitest.org/
[mutmut]: https://pypi.org/project/mutmut/
[cargo-mutants]: https://github.com/sourcefrog/cargo-mutants
[MutFlow]: https://github.com/anschnapp/mutflow
[Mull]: https://arxiv.org/pdf/1908.01540
[Stryker]: https://stryker-mutator.io/

## Compatibility

Purely additive, identical to how `-fuzz` was added: a new flag on an
existing command, zero behavior change for any invocation that doesn't use
it. No changes to the `testing` package's public API are proposed at this
stage — the reference implementation runs mutation as an external driver
around `go test`, not as new `testing.T`/`testing.M` machinery, though that
question (does mutation testing eventually want its own `testing.M`-level
integration, the way fuzzing got `testing.F`) is an open one for a later,
more mature stage of this proposal.

## Implementation

A working reference implementation exists today:
[github.com/jh125486/turango](https://github.com/jh125486/turango). It
ships as a transparent shim binary (`turango`) that intercepts
`test ... -mutate=...` and forwards every other invocation verbatim to the
real Go toolchain via process replacement — installable and usable today,
independent of whether/when this proposal is accepted, exactly mirroring
how `go-fuzz` was usable years before `-fuzz` existed. Renaming/symlinking
it to shadow `go` on `PATH` is supported as an explicit opt-in experimental
mode, not a default, given the blast radius of a single unhandled `go`
verb breaking every tool on a machine that shells out to `go`.

turango itself is a from-scratch 2026 rebuild, not a port of the 2018
`go-turango` codebase — written with Claude, applying the lessons the 2018
paper's own "Future Work" section identified as unfinished (see Background,
above): an in-memory diff engine, coverage-directed test selection,
per-operator enable/disable, `//nomutant` suppression, and CI-gating via a
score threshold are all now implemented, none of them present in 2018.

## Costs and risks

- **Runtime cost.** Mutation testing is inherently slower than a normal
  test run — potentially orders of magnitude, since each mutant reruns
  some scope of the suite. Mitigated by `-mutatescope=package|impact`,
  `-mutateparallel`, and the expectation (same as fuzzing) that this runs
  on demand or in a separate CI stage, not on every `go test` invocation.
  See `BENCHMARKS.md` for the mutation-multiplier measurement methodology
  and (once run — deliberately deferred to just before submission, on a
  plugged-in machine rather than mid-development on battery) real
  `go test -bench` numbers backing this claim, rather than an unverified
  qualitative estimate.
- **False positives / equivalent mutants.** A mutation that's semantically
  a no-op (e.g. `i*1` compiling to the same result as `i`) reports as a
  survivor without being a real gap. The reference implementation already
  filters *syntactic* no-ops (byte-identical printed output), and
  `//nomutant` handles the remaining semantic cases, matching how every
  mature mutation tester handles this class of noise.
- **Operator coverage is necessarily non-exhaustive**, as any finite
  operator set is: 14 operators across `control`, `expression`, `literal`,
  `operator`, `statement`, and `identifier` (the last two requiring
  `go/types`, not just `go/ast`) cover the mutation shapes validated
  against this proposal's own case studies, including the
  wrong-identifier-substitution shape from `strconv#21278` (see "Known
  gap ... now closed," above) — but mature mutation testers in other
  languages (e.g. PIT's ~30-mutator set) cover more shapes than turango
  does today. Extending coverage further is ordinary follow-on work, not a
  blocker for this proposal's core claim.
- **Turango is a working example with mild optimizations, not a
  state-of-the-art mutation-testing engine — deliberately.** Everything it
  does to control cost today is a well-understood, conservative technique:
  narrower test scopes, a measured (not guessed) per-mutant timeout,
  file-level parallelism, and two opt-in, correctness-preserving filters
  (TCE, dependency-closure workspace copying). It does *not* attempt
  several techniques the mutation-testing research literature treats as
  standard for reducing cost at scale — [mutant subsumption][subsumption] /
  selective mutation (running a reduced, representative subset of mutants
  instead of every one an operator offers, noted but explicitly deferred
  alongside TCE), higher-order mutation, ML-guided mutant prioritization or
  kill prediction, or diff-scoped incremental mutation testing keyed to what
  actually changed rather than a whole package. None of these are built,
  and none are needed to validate this proposal's core claim: that the
  mutate-rerun-classify mechanism itself belongs in the standard toolchain
  the way `-fuzz` does. A stdlib implementation is free to adopt any of
  them later without revisiting this proposal's mechanism — this
  prototype's job is proving the mechanism is worth having, not being the
  fastest possible realization of it. (Benchmark harness built and run for
  real — see `BENCHMARKS.md` for the full transcript and methodology.)
- **TCE's payoff is real but codebase-dependent — it is not a free
  optimization, and this proposal has a real measurement showing it can be
  a net loss.** Against `corpus/stdlib-strconv-parseuint` (a real, frozen
  stdlib fixture), enabling `-mutatetce=true` filtered 77 of 3523 mutants
  (2.2%) as compiler-equivalent — genuine signal, not noise. But TCE's
  compile-and-compare check (`go build -gcflags=-S`, measured directly:
  0.414s, warm cache) runs *unconditionally* for every mutant, not just the
  ones that turn out equivalent, in exchange for skipping the `go test` run
  (0.605s, same fixture) only on that 2.2%. The arithmetic for N mutants:
  without TCE, `N × 0.605s`; with TCE, `N × 0.414s + N × 0.978 × 0.605s =
  N × 1.006s` — **TCE was about 66% slower overall for this specific
  fixture**, because the per-mutant tax on 100% of mutants outweighs the
  savings on the 2.2% it actually filters. This is exactly why turango
  ships TCE opt-in, not opt-out — a false positive would silently discard
  a real mutant (a correctness risk), and this measurement shows it isn't
  even a reliable win on cost grounds either, arriving at the same default
  from an independent direction. The general lesson, not specific to TCE: turango's
  knobs (`-mutatescope`, `-mutateparallel`, `-mutatetce`, and any future
  ones) are levers whose right setting depends on the target codebase's own
  shape — equivalent-mutant rate, test suite cost, cross-package coupling —
  not a single universally-correct configuration. `BENCHMARKS.md` documents
  this specific measurement in full; a stdlib implementation should expect
  to publish similar guidance rather than a single recommended default for
  every knob.
- **Concurrency bugs are not reliably tested for.** Every mutant runs with
  `go test -parallel=1` (deliberate — it prevents `GOMAXPROCS x
  -mutateparallel` worker multiplication from misclassifying slow-but-fine
  mutants as timeout-killed) and turango never adds `-race` itself. A
  mutation that only breaks under real concurrent execution (e.g. deleting
  a `mutex.Lock()`) can survive a default run: `-parallel=1` reduces the
  scheduling pressure a `t.Parallel()`-heavy suite would otherwise use to
  surface a race, and without `-race`, Go's own race detector never runs
  at all. Both are available today via turango's existing flag passthrough
  (`turango test -race -mutate=... ./...`), but are opt-in, untested by
  default, and unbenchmarked for the cost/detection tradeoff that implies.

[TCE]: https://doi.org/10.1109/ICSE.2015.103 "Papadakis, Jia, Harman & Le Traon, 'Trivial Compiler Equivalence: A Large Scale Empirical Study of a Simple, Fast and Effective Equivalent Mutant Detection Technique,' ICSE 2015"
[subsumption]: https://doi.org/10.1109/ICST.2014.13 "Ammann, Delamaro & Offutt, 'Establishing Theoretical Minimal Sets of Mutants,' ICST 2014"

## Open questions

1. Should `-mutatescope=impact`'s per-test coverage map, or the
   `-mutatetimeout` baseline measurement, have their own tunable flags, or
   are the current defaults good enough for a first stdlib cut (mirroring
   how `-fuzz` shipped with sensible defaults and grew tuning flags over
   time)?
2. Does `//nomutant` want a `go vet`-recognized directive form eventually
   (see `go/ast.ParseDirective`, added Go 1.26), or is a bespoke
   line-comment convention sufficient long-term?
3. Is external-driver architecture (this proposal's approach) the right
   long-term shape, or does mutation testing eventually want first-class
   `testing` package integration analogous to `testing.F`? Proposed
   answer: start external (lower risk, faster iteration, proven pattern),
   revisit only once there's real usage data — the same order fuzzing
   itself followed.

## Related work

- [Design Draft: First Class Fuzzing](https://go.googlesource.com/proposal/+/master/design/draft-fuzzing.md) — the structural precedent this proposal follows.
- [golang/go#75315](https://github.com/golang/go/issues/75315) — complementary, assembly-scoped mutation testing, currently open.
- [go-mutesting](https://github.com/zimmski/go-mutesting), [gomu](https://github.com/sivchari/gomu), [ooze](https://github.com/gtramontina/ooze) — third-party Go mutation testers, none integrated into `go test`.
- PIT, muJava, MutPy — mutation testing prior art in Java/Python, surveyed in the author's 2018 paper.
- [Trivial Compiler Equivalence][TCE] and [mutant subsumption][subsumption]
  — the equivalent-mutant filtering and mutant-selection techniques
  `-mutatetce` implements and (respectively) deliberately does not attempt
  yet; see "Costs and risks" above.
