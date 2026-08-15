# Proposal: mutation testing for `go test`

Author: Jacob Hochstetler

Last updated: 2026-08-13

Status: draft; discussion issue not yet filed

## Abstract

Add mutation testing to `go test` as a new flag, `-mutate`. First-class
fuzzing provides precedent for adding a distinct test mode to the Go
toolchain, though mutation testing has a different execution model and does
not require a new `testing` API in this proposal. A working,
validated prototype — [turango](https://github.com/jh125486/turango) — exists
and is the basis for this proposal. Turango is a reference implementation
with scope narrowing, a baseline-derived timeout, worker-pool parallelism,
opt-in TCE, verdict caching, and configurable workspace construction. It
argues that the *mechanism* belongs in the Go toolchain, not that this is the
most efficient possible implementation; see "Costs and risks" below.

Mutation testing measures test-suite *quality*, not code coverage. It
mechanically introduces small, individually reversible changes ("mutants")
into a package's AST — flip `+` to `-`, flip `==` to `!=`, delete a
statement, empty an `if` body — and reruns the test suite against each one.
A mutant the selected tests catch ("killed") shows that they distinguish the
mutated behavior. A surviving mutant means the selected tests did not
distinguish it; coverage data is needed to separate executed-but-unasserted
code from code those tests never reached.

## Background

### The fuzzing precedent

`-fuzz` spent years as a third-party tool (`go-fuzz`) before landing in
the Go toolchain's `go test` command in Go 1.18. The
[design draft for first-class fuzzing](https://go.googlesource.com/proposal/+/master/design/draft-fuzzing.md)
stated its guiding principle plainly: *"Fuzz testing shouldn't be any more
complicated, or any less feature-complete, than other types of Go testing."*
Mutation testing differs from fuzzing in target selection, instrumentation,
corpus semantics, and its mutate-rerun-classify loop. Fuzzing remains useful
precedent for validating a new test mode in an installable, `go`-compatible
tool before proposing toolchain integration.

### Prototype lineage

An earlier `go-turango` prototype explored mutation testing against historical
Go packages. Its retained source is available under
[`example/legacy/`](example/legacy/); this proposal does not rely on its
measurements.

The prototype includes an in-memory mutation engine, coverage-directed test
selection (`-mutatescope=impact`), operator selection
(`-mutateoperators`), source suppression (`//nomutant`), and CI score gating
(`-mutatemin`).

### Prior art and assembly-scoped mutation testing

Third-party Go mutation testers exist today (`go-mutesting`, `gomu`,
`ooze`), none integrated into `go test` itself.

More directly relevant: **golang/go#75315** ("proposal: cmd/asm: support
for mutation testing"), filed by Filippo Valsorda in September 2025, proposes
`-mutlist`/`-mut`
flags on `cmd/asm` to mutate individual assembly instructions, motivated by
constant-time cryptographic code: branch coverage is meaningless there,
since constant-time implementations deliberately execute every "branch"
via `CMOV`/`ADC` regardless of input, so standard coverage tools can't
distinguish a tested code path from an untested one.

**This proposal is complementary, not competing.** #75315 reaches the
hand-written assembly hot path of a primitive like AES-NI. This proposal
reaches everything around it: key schedule, mode-of-operation glue (CBC,
CTR, GCM), parsing, and every other pure-Go package in `crypto/...` and
beyond that isn't hand-tuned assembly at all. The two approaches cover
different layers and can be used together.

## Prototype evidence

Results below come from checked-in, package-isolated corpus fixtures. They are
useful prototype validation, not reproducible measurements of a pinned Go
source revision: several fixtures lack an exact upstream commit, and original
commands and raw reports were not retained. Quantitative claims are therefore
limited to these fixture snapshots. Checked-in `golden.json` files record
current expected classifications.

Current checked-in corpus expectations are:

| Fixture | Mutants | Killed | Survived | Not viable | Score |
|---|---:|---:|---:|---:|---:|
| `stdlib-x509-pkix` | 291 | 0 | 256 | 35 | 0.0% |
| `stdlib-crypto-aes` | 7,746 | 5,210 | 2,306 | 230 | 69.3% |
| `stdlib-encoding-base64` | 708 | 542 | 127 | 39 | 81.0% |

The `pkix` fixture has no test assertions and killed no mutants. The AES
fixture includes FIPS-140 code and outer known-answer tests, but excludes GCM
and forces hardware-dispatch feature flags false; some survivors may be
unreachable under that configuration. The base64 fixture is heavily adapted
and is not a byte-for-byte snapshot of an upstream commit. These results show
that the engine can generate and classify mutants across fixtures of different
sizes and test strength. They do not establish mutation scores for current Go
packages or reproduce specific historical bugs.

## Proposal

Add a `-mutate=<regexp>` flag to `go test`, using matching semantics similar
to `-run`/`-bench`/`-fuzz`: its value is a regular expression matched against
declared function/method names in the target packages, not a package
selector. Package selection is the ordinary trailing package arguments —
entirely separate from the flag's value, the same separation `-run` and
`-fuzz` already have between "which package(s)" and "which named target
within them." Package-level declarations are always eligible because they do
not belong to a named function or method.

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
  measurement (the unmutated suite timed 3×, averaged, scaled by CPU count)
  so a mutation that produces an infinite loop (a common outcome — e.g.
  `i++` flipped to `i--`) doesn't stall the run to `go test`'s own
  ~10-minute default; a timeout is itself treated as a kill.
- `-mutateoutput=<dir>` — write a JSON report.
- `-mutatemin=<float>` — non-zero exit if the resulting score falls below
  the threshold, for CI gating.
- `-mutatemutant=<id>` — replay one mutant ID from a previous report. IDs are
  stable across reruns of unchanged source; source-position changes can change
  them.
- `-mutatetce=true|false` — [Trivial Compiler Equivalence][TCE] (TCE):
  filter a mutant whose normalized compiler disassembly matches a baseline
  before it reaches the test suite. Off by default; see "Costs and risks."
- `-mutateworkspace=copy|worktree` — build each mutant's isolated workspace
  with a filesystem copy or, when repository state permits, a Git worktree.
- `-mutateestimate=true|false` — count prospective mutants and sample
  baseline cost without executing or classifying them.
- `-mutatecache=<dir>` — persist mutant verdicts. Cache keys include mutant
  ID, dependent-source fingerprint, scope, TCE setting, and toolchain.
  Workspace strategy, parallelism, and timeout are deliberately excluded;
  changing a timeout can therefore reuse an earlier timeout-killed verdict.

### Classification and score

Each executed mutant is classified as killed, survived, or not viable. A test
failure or timeout kills a mutant; a passing selected test set lets it survive;
and code that does not compile is not viable. When the default timeout is used,
failure of the unmutated baseline suite aborts the run because later failures
could not be attributed to mutations.

Mutation score is `killed / (killed + survived)`. Not-viable mutants, TCE
equivalents, syntactic no-ops, and `//nomutant` suppressions are excluded. A run
with no viable mutants reports no score; `-mutatemin` emits a diagnostic but
does not fail that run.

A `//nomutant` (and `//nomutant:reason`) source comment suppresses
mutation of the annotated statement, cascading into the body of a
suppressed compound statement (`if`/`for`/`switch`), for intentionally
untestable or already-known-equivalent code. This follows suppression
conventions used by tools such as PIT, Stryker, and Go linters, adapted
to line-based comment scanning rather than `ast.CommentMap`, whose
documented trailing-comment edge cases ([golang/go#21755],
[golang/go#33451]) make it a poor fit for a suppression directive.

[golang/go#21755]: https://github.com/golang/go/issues/21755
[golang/go#33451]: https://github.com/golang/go/issues/33451

### The mutation operators (14, across six packages)

The operator set includes `if`/`else`/`case` body removal, `&&`/`||`
short-circuit-operand elimination, statement removal, and four token-swap
operators (assignment, binary, increment/
decrement, unary-strip) covering the classic arithmetic/relational/logical
scalar mutations. It also includes `operator/boundary`, a relational boundary
shift (`<`↔`<=`, `>`↔`>=`) — the
classic off-by-one mutant, distinct from `operator/binary`'s negation swap
(`<`↔`>=`) and named to match PIT's "Conditionals Boundary Mutator" (PIT is
cited in Related Work, below); a pair of literal operators, `literal/number`
(shifts an integer literal by ±1, e.g. `x < 0` → `x < 1`, or a float
literal by a small relative nudge in each direction) and `literal/boolean`
(swaps `true`↔`false`); and the identifier-substitution pair
`identifier/constswap` and `identifier/localconstswap`.

`literal/number` and `literal/boolean` cover literal-value mutations like
`x < 0` → `x < 1`, but on their own do not reproduce the historical
[strconv#21278](https://github.com/golang/go/issues/21278) `ParseUint`
overflow bug, whose actual shape was a wrong-*identifier* substitution —
the local variable `maxVal` was used where the package-level constant
`maxUint64` should have been. Producing that mutation requires knowing
which in-scope identifiers are type-compatible substitutes, which needs
`go/types`, not just `go/ast`. Two operators implement this:
`identifier/constswap` (package-level const-for-const, same `const(...)`
block or file) and `identifier/localconstswap` (a function-local variable,
restricted to comparison operands, swapped for a type-compatible
package-level constant declared in the same file) — the latter reproduces
this bug shape. A unit test type-checks the frozen
historical `strconv` source and asserts `Mutate` offers `maxVal ->
maxUint64` on the real `n1 > maxVal` comparison.

## Rationale

- **Fuzzing provides an integration precedent.** `-fuzz` shows that a
  specialized test mode can fit `go test`. Mutation testing has different
  execution and API requirements, described above.
- **It measures something coverage cannot.** Every one of `crypto/aes`'s
  fixture survivors records a mutation the selected tests did not
  distinguish. Coverage answers "did this line run"; mutation testing asks
  "does a selected test notice this change." Using both separates uncovered
  code from executed code whose behavior may be weakly asserted.
- **It complements, not duplicates, #75315.** Assembly-level mutation
  testing and Go-source mutation testing reach different, non-overlapping
  code. Projects containing both layers can use both approaches.
- **Untested fixtures produce an explicit result.** The `pkix` fixture's
  0% score makes the absence of distinguishing tests visible in the same
  report format as tested packages.
- **Existing implementations are generally external tools.** Among tools
  surveyed for this proposal — [PIT] (Java), [mutmut]
  (Python), [cargo-mutants] (Rust), [MutFlow] (Kotlin), [Mull] (C/C++,
  LLVM-based), and [Stryker] (JavaScript/TypeScript, .NET) — mutation testing
  is provided outside the language toolchain. This proposal would integrate
  that workflow into `go test`.
- **It gives AI-assisted development a machine-checkable guardrail.** An LLM
  generating or editing Go code is, in effect, an extremely fast producer of
  candidate mutants of its own — and coverage alone cannot tell a reviewer
  (human or automated) whether the tests around a change would actually catch
  a regression, only whether the changed lines executed. A mutation score
  turns "the tests still pass" into "the tests would notice if this were
  wrong," which is the property an AI coding agent needs to iterate against
  safely without a human re-deriving the failure by hand each time. Dogfooding
  turango against its own suite surfaced this pattern directly: every fix that
  raised the score corrected a test that could not distinguish correct
  behavior from a mutated (wrong) version — a weak assertion, an untested
  success path, a fixture that referenced the same constant it was meant to
  verify — never a defect in the code under test. That is precisely the kind
  of guardrail gap an AI agent, working from coverage alone, would not have
  had any signal to find or fix.

[PIT]: https://pitest.org/
[mutmut]: https://pypi.org/project/mutmut/
[cargo-mutants]: https://github.com/sourcefrog/cargo-mutants
[MutFlow]: https://github.com/anschnapp/mutflow
[Mull]: https://arxiv.org/pdf/1908.01540
[Stryker]: https://stryker-mutator.io/

## Compatibility

Purely additive: a new flag on an existing command, with no behavior change
for any invocation that doesn't use it. No changes to the `testing` package's
public API are proposed at this stage — the reference implementation runs
mutation as an external driver
around `go test`, not as new `testing.T`/`testing.M` machinery, though that
question (does mutation testing eventually want its own `testing.M`-level
integration, the way fuzzing got `testing.F`) is an open one for a later,
more mature stage of this proposal.

## Implementation

A working reference implementation exists today:
[github.com/jh125486/turango](https://github.com/jh125486/turango). It
ships as a transparent shim binary (`turango`) that intercepts
`test ... -mutate=...` and forwards every other invocation verbatim to the
Go toolchain, using process replacement on Unix and a child process on
Windows. It is installable and usable today, independent of whether/when this
proposal is accepted. Renaming/symlinking
it to shadow `go` on `PATH` is supported as an explicit opt-in experimental
mode, not a default, given the blast radius of a single unhandled `go`
verb breaking every tool on a machine that shells out to `go`.

The implementation uses an in-memory mutation engine, coverage-directed test
selection, operator selection, `//nomutant` suppression, JSON reporting, and
CI score gating. Its architecture and current CLI are documented in the
repository README.

## Costs and risks

- **Runtime cost.** Mutation testing is inherently slower than a normal
  test run — potentially orders of magnitude, since each mutant reruns
  some scope of the suite. Mitigated by `-mutatescope=package|impact`,
  `-mutateparallel`, caching, and the expectation that this runs on demand or
  in a separate CI stage, not on every `go test` invocation. See
  [`BENCHMARKS.md`](BENCHMARKS.md) for exploratory measurements and their
  limitations.
- **False positives / equivalent mutants.** A mutation that's semantically
  a no-op (e.g. `i*1` compiling to the same result as `i`) reports as a
  survivor without being a real gap. The reference implementation already
  filters *syntactic* no-ops (byte-identical printed output), and
  `//nomutant` can suppress remaining known semantic cases.
- **Operator coverage is necessarily non-exhaustive**, as any finite
  operator set is: 14 operators across `control`, `expression`, `literal`,
  `operator`, `statement`, and `identifier`. Identifier substitution requires
  `go/types`; the other operator packages work from syntax. These cover
  mutation shapes validated against this proposal's case studies, including
  the wrong-identifier-substitution shape from `strconv#21278`, but mutation
  testers in other languages (e.g. PIT's ~30-mutator set) cover more shapes
  than turango does today. Extending coverage further is ordinary follow-on
  work, not a blocker for this proposal's core claim.
- **Turango is a reference implementation, not a state-of-the-art
  mutation-testing engine.** Current cost controls include narrower test
  scopes, a baseline-derived per-mutant timeout, file-level parallelism,
  verdict caching, and configurable workspace construction. TCE is a separate
  opt-in filter with both cost and false-positive tradeoffs. Turango does not
  attempt several techniques the mutation-testing research literature treats as
  standard for reducing cost at scale — [mutant subsumption][subsumption] /
  selective mutation (running a reduced, representative subset of mutants
  instead of every one an operator offers), higher-order mutation, ML-guided
  mutant prioritization or kill prediction, or diff-scoped incremental
  mutation testing keyed to what actually changed rather than a whole package.
  None of these are built, and none are required by the core
  mutate-rerun-classify mechanism.
- **TCE has codebase-dependent cost and correctness tradeoffs.** In the
  `stdlib-strconv-parseuint` fixture, TCE classified 77 of 3,523 full-scope
  mutations (2.2%) as equivalent by comparing normalized compiler
  disassembly. Spot measurements predict a per-mutant cost increase, while
  the one-shot benchmark matrix shows lower elapsed times in every
  TCE-enabled full-scope row. These conflicting exploratory observations do
  not support a performance conclusion. TCE remains opt-in because its cost
  depends on compile/test ratios and a false equivalence can discard a real
  mutant. [`BENCHMARKS.md`](BENCHMARKS.md) documents methodology limitations.
- **Default execution reduces some concurrency coverage.** Every mutant runs
  with `go test -parallel=1` (deliberate — it prevents `GOMAXPROCS ×
  -mutateparallel` worker multiplication from misclassifying slow-but-fine
  mutants as timeout-killed). A mutation that depends on concurrent behavior
  (e.g. deleting a
  `mutex.Lock()`) can survive a default run: `-parallel=1` reduces the
  scheduling pressure a `t.Parallel()`-heavy suite would otherwise use to
  surface a race, but does not prevent goroutines within tests from running
  concurrently. Turango does not add `-race`, and mutation mode currently
  rejects non-turango flags, so race-detector execution is not available in a
  mutation run.

[TCE]: https://doi.org/10.1109/ICSE.2015.103 "Papadakis, Jia, Harman & Le Traon, 'Trivial Compiler Equivalence: A Large Scale Empirical Study of a Simple, Fast and Effective Equivalent Mutant Detection Technique,' ICSE 2015"
[subsumption]: https://doi.org/10.1109/ICST.2014.13 "Ammann, Delamaro & Offutt, 'Establishing Theoretical Minimal Sets of Mutants,' ICST 2014"

## Open questions

1. Should `-mutatescope=impact`'s per-test coverage map, or the
   `-mutatetimeout` baseline measurement, have their own tunable flags, or
   are the defaults suitable for an initial toolchain implementation?
2. Should `//nomutant` use a standard `//tool:name` form parseable by
   `go/ast.ParseDirective` (for example, `//turango:nomutant reason`), or is
   the bespoke line-comment convention sufficient long-term?
3. Is external-driver architecture (this proposal's approach) the right
   long-term shape, or does mutation testing eventually want first-class
   `testing` package integration analogous to `testing.F`? Proposed
   answer: start external to reduce integration surface, then revisit with
   usage data.

## Related work

- [Design Draft: First Class Fuzzing](https://go.googlesource.com/proposal/+/master/design/draft-fuzzing.md) — the structural precedent this proposal follows.
- [golang/go#75315](https://github.com/golang/go/issues/75315) — complementary, assembly-scoped mutation testing proposal.
- [go-mutesting](https://github.com/zimmski/go-mutesting), [gomu](https://github.com/sivchari/gomu), [ooze](https://github.com/gtramontina/ooze) — third-party Go mutation testers, none integrated into `go test`.
- PIT, muJava, and MutPy — mutation-testing prior art in Java and Python.
- [Trivial Compiler Equivalence][TCE] and [mutant subsumption][subsumption]
  — the equivalent-mutant filtering and mutant-selection techniques
  `-mutatetce` implements and (respectively) deliberately does not attempt
  yet; see "Costs and risks" above.
