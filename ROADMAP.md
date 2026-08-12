# Roadmap: closing turango's validation-identified gaps

This is a planning document, not a changelog. Gaps 1-3 were identified during
the historical-bug validation work described in `PROPOSAL.md` (the
strconv/base64/crypto case studies); gaps 4-6 surfaced later, during the
corpus-harness and dogfooding work. In priority order, since the first two
are the load-bearing evidence for the eventual stdlib pitch:

1. **Done, v1 and v2 both.** An identifier/constant-swap mutation operator
   (v1: const-for-const; v2, landed 2026-08-11: local-var-to-package-const,
   the exact strconv `ParseUint` shape) — `internal/mutator/identifier/constswap.go`.
   v2's first version had a real 24x mutant-count blowup (no file-scoping
   restriction), fixed by restricting candidates to package-level constants
   in the same file as the local variable's use — verified stable (81
   mutants) via direct binary testing, and confirmed against the real
   strconv `ParseUint` historical source by a dedicated unit test.
2. **Done.** Trivial Compiler Equivalence (TCE) — filtering equivalent
   mutants by normalized `-S` disassembly comparison (the spike found raw
   archive comparison, the original plan, unreliable — see below),
   opt-in via `-mutatetce=true` (`Options.TCE`) — `internal/mutate/runner.go`'s
   `compileDisassembly`/`isTCEEquivalent`, `Result.Equivalents`.
3. **Done.** Before/after source snippets on `MutantResult` (`.Before`/
   `.After`), so a JSON report is usable without hand-deriving the diff
   from `Description` and a line number — console output unchanged (3c).
   `mutator.Mutation.Node` is the mechanism; most operators leave it nil
   (the walk's own node is already precise enough), `control/{if,else,case}`
   and `statement/remover` are the ones that set it explicitly.
4. **Done.** Deterministic mutant IDs — a `-fuzz`-style content hash per
   mutant, so a specific mutant can be referenced in a comment, a
   regression test, or replayed directly via `-mutatemutant=<id>`.
5. **Done.** Dependency-closure workspace copy — `copyModule` used to copy
   the whole module per mutant regardless of scope; under `ScopePackage`/
   `ScopeImpact`, `runner.workspaceFor` now calls `copyClosure` with a
   per-package closure resolved once by `engine.planClosure`/
   `resolveClosure` (loaded test-aware via the new `engine.loadClosures`),
   falling back to `copyModule`/`copyWorktree` automatically on any
   uncertainty (a `vendor/` directory, a `//go:embed` directive, an unsafe
   replace target, or `ScopeFull` itself, where a forward closure is
   provably wrong — see the section below for why). Two real bugs an
   independent review found before activation (a blackbox `_test.go`
   package's imports being invisible to the closure walk; an in-module
   `replace` target outside the closure not triggering fallback) were fixed
   first — see the section below's "Independent review before activation."
   Verified end to end: `TestRunWithImpactScope` (an existing integration
   test, already running under `ScopeImpact`) now exercises this path for
   real against a real module and passes; `TestWorkspaceForPrefersClosure
   OverWorktree` proves the closure-over-worktree precedence directly.
6. **Done.** Git-worktree-based execution, strictly opt-in (`-mutateworkspace=worktree`,
   `Options.Workspace`) — a cheaper *mechanism* for
   the same copy step gap 5 addresses the *scope* of, only for users already
   inside a git repo; lower priority than gap 5 because it only benefits git
   users, whereas gap 5 benefits everyone the same way `-fuzz` does (no git
   dependency at all).
7. **Done.** Channel-based collector, replacing the mutex-guarded one — not a
   correctness fix (the mutex is race-safe, verified under `-race`), but a
   testability one: `testing/synctest`'s "durably blocked" detection covers
   channel send/recv, `select`, `sync.Cond.Wait`, `sync.WaitGroup.Wait`, and
   `time.Sleep` — `sync.Mutex.Lock` is explicitly not on that list (`go doc
   testing/synctest`), so a goroutine blocked acquiring the collector's
   mutex is invisible to a synctest bubble. Lowest priority of the seven:
   nothing in the collector is timing-dependent today, so there is nothing
   to synctest-test yet — this is a door to leave open, not a bug to fix.
8. **Done.** `BenchmarkMutate` (`internal/mutate/mutate_bench_test.go`) is
   built, verified, and its full `-count=1` overnight run is complete —
   both targets x all 3 scopes x TCE x all 3 parallel levels, every
   subtest has a real captured number. `BENCHMARKS.md` holds the full
   transcript, methodology, and two honest data-quality caveats about how
   this specific run was captured (a metric gap from process-splitting,
   real CPU contention with concurrent corpus verification). Confirmed
   3523 mutants (not 3517) is `stdlib-strconv-parseuint`'s stable count —
   the earlier "3517 vs 3523" discrepancy was an artifact of an
   interrupted capture, not a real alternate count.
9. **Done, all five sub-tasks.** Comment cleanup pass across the codebase
   and its docs — five distinct sub-tasks (staleness audit, a
   verbosity-trim style question, dead-code/stray-marker removal, format
   consistency, and proper academic citations for named techniques like
   TCE) that should not be treated as one mechanical task — see below for
   why. 9a/9c/9d/9e: stale ROADMAP.md line-number citations in gaps 1 and
   3 were corrected or converted to function-name-only references,
   several stale doc claims in
   `README.md`/`PROPOSAL.md`/`internal/mutator/literal/literal.go` (an
   operator count, a "known gap" that had since closed, a captured
   example run predating mutant IDs) were fixed, no `TODO`/`FIXME`/`XXX`
   or dead commented-out code was found, and the TCE/mutant-subsumption
   citations now have real DOI links. 9b (verbosity trim): explicit
   go-ahead given, scoped to the exact criterion this section's own text
   suggested — trim comments that don't cite a rejected alternative or a
   concurrency/correctness hazard, leave everything that does. A full
   read of every 4+-line comment in `internal/`, `cmd/`, `example/` found
   the codebase's rationale-heavy convention genuinely held up in
   practice: only 5 comments (across `internal/corpus`, `internal/mutate`
   x2, `internal/mutator/identifier`) were pure restatement/narration
   with no rationale attached, and only those were trimmed.
10. **Done.** `literal/number` now mutates float literals too (a relative
    0.1% nudge, not a flat ±1 — see below for why). Found via an
    independent fable-model review, built the same night.
11. **Done.** `-mutateestimate=true` — a walk-only preview of a real run's
    mutant count and a rough, honestly-hedged time estimate, per package,
    before committing to it. See below for the full design.
12. **Done.** A persistent mutant-verdict cache (`-mutatecache=<dir>`),
    invalidated on real code change via a compound key
    (`{Toolchain, Scope, TCE, Fingerprint, MutantID}`) — mutant IDs alone
    proved unsafe as a cache key (see gap 4's own stability caveat and
    gap 13 below for a concrete collision this surfaced), so cache
    correctness rests on the fingerprint + scope + TCE components too, not
    the ID in isolation.
13. **Done.** `mutantID` collisions on left-associative binary-expression
    chains (`a^b^c^d^e`) — Go's `ast.BinaryExpr.Pos()` returns the same
    leftmost-operand position for every nested sub-expression in such a
    chain, so multiple distinct mutations on one chain used to hash to the
    same ID. Fixed via a per-file `idDeduper` that counts how many times a
    given (line, col, operator, index) tuple has already been seen during
    the walk, appending that rank to the hash only when it's nonzero — the
    common, non-colliding case is byte-for-byte unchanged (pinned by a
    regression test against the pre-fix hash). Verified non-vacuous: the
    added `TestRunBinaryChainMutantsHaveDistinctIDs` fails with real
    collisions when the fix is reverted, and a real run against the exact
    `corpus/stdlib-crypto-aes` line that surfaced this bug now gives
    `f1dd58acbb56` (the originally-colliding ID) exactly once.
14. **Built, not fully live yet.** All 7 badges are in README.md and
    every workflow behind them exists (`codeql-analysis.yml`,
    Codecov/SonarCloud steps added to `ci.yml`, the new scheduled
    `mutation-badge.yml`). `Go Reference`/`Tests`/`CodeQL` work the moment
    the repo is public — no further action. `Codecov` and both SonarCloud
    badges are wired but self-gated (no-op until their respective
    external accounts exist and `CODECOV_TOKEN`/`SONAR_TOKEN` secrets are
    set — an account-linking step only the user can do). The
    mutation-score badge's workflow has never been run for real (it needs
    GitHub Actions, not local `go test`) — verify its first real run once
    the repo is public before trusting the published number.

Each section below states the problem, the design decisions and their
rationale, the exact files and functions touched, a build order, and how to
verify the result — matching the precision of the original build plan
(`~/.claude/plans/wondrous-crunching-avalanche.md`). Genuinely open questions
are called out as such, not resolved by assertion.

Any remaining file/line references below were current only as of the
commit each section was originally written against; re-check line numbers
before implementing if the tree has moved on. Gaps 1, 3, and 5 have had
their line-number citations converted to function-name-only (`[Xxx]`-style)
references or verified/corrected as of the 9a staleness-audit pass (see gap
9, below) — the remaining gaps have not all had the same treatment yet.

---

## 1. Identifier/constant-swap operator

### Problem

`PROPOSAL.md`'s own "Known gap" section states it plainly: the strconv
`ParseUint` bug's actual shape was a wrong-*identifier* substitution — the
bitSize-scoped local `maxVal` used where the wider-scope named constant
`maxUint64` should have been (or vice versa; the exact direction is less
important than the shape). None of the 11 operators produce this. They fall
into three families — token swap (`operator/*`), literal-value shift
(`literal/*`), and structural removal (`control/*`, `expression/remove`,
`statement/remover`) — and all three only ever look at a node's own syntax.
Producing "swap this identifier for a different, type-compatible, in-scope
identifier" requires knowing what's in scope and what it's typed as, which is
`go/types`, not `go/ast`.

This is a real architectural step up. `internal/mutate/engine.go`'s `[load]`
is explicit about avoiding this today:

```go
// The load mode asks only for names, files and module metadata: the engine
// parses the files itself (with comments, which phase 4's //nomutant scan
// needs) rather than reusing packages' syntax trees, and no operator needs type
// information. That keeps the load cheap and avoids type-checking a module that
// may not even type-check cleanly.
```

`packages.Load` is called with `Mode: packages.NeedName | packages.NeedFiles
| packages.NeedModule` — no `NeedTypes`/`NeedTypesInfo`/`NeedSyntax`. Adding
an operator that needs type info means this comment's claim ("no operator
needs type information") stops being globally true. The plan below keeps it
true *by default* — the cost is paid only when this operator is actually
selected — rather than making it false for every run.

### Design decisions

**1a. Scope the swap set hard; don't do "any type-compatible identifier in
scope."** The task framing is right to worry about this: an unbounded
version — every `*ast.Ident` use, matched against every same-type identifier
visible at that point per Go's scoping rules — would make `Applies` true for
a huge fraction of identifier references in a typical file, and for each one
offer one mutation per same-type sibling in scope (locals, params, package
vars, package consts, even same-type identifiers from dot-imports). On a
function with a dozen `int`-typed locals in scope, that's potentially
dozens of mutations from *one* function, dwarfing what the other 11
operators combined produce on the same code. This isn't a hypothetical risk
to hedge against later — it needs a hard scope restriction from the first
line of code.

Chosen v1 restriction: **package-level `const`-for-`const` only**, with:
- exact type match (`types.Identical(a.Type(), b.Type())`, not just
  matching `Underlying()` — see rationale below), and
- both constants declared in the same `const ( ... )` block, or (if not
  block-declared) the same file — not "anywhere in the package."

Rationale for exact-type rather than underlying-type matching: a looser
match (same underlying basic kind, e.g. two different named `int` types)
produces more candidate swaps, but a large fraction would be trivially
`NotViable` (Go's type system rejects an untyped-const-to-named-type
assignment mismatch, or a named-type mismatch across an interface
boundary) — inflating the `NotViable` count without adding proposal-relevant
signal. `operator/binary` and `literal/number` are both engineered to avoid
guaranteed-uncompilable mutants where the operator can tell in advance
(see e.g. `literal/number.go`'s reliance on `go/constant` to always produce
a syntactically valid literal); this operator should hold itself to the same
standard.

Rationale for same-block/same-file restriction: it directly targets the
"named constants that mean similar things and are easy to transpose" case —
sibling error codes, sibling size/limit constants, sibling enum-like values
— which is both a common real bug shape and keeps the candidate set per
identifier small (typically single digits, bounded by how many consts a
`const (...)` block or file realistically holds), rather than growing with
total package size.

**Honest limitation, stated up front rather than glossed over**: this v1
restriction does *not* reproduce the strconv shape exactly. Per the
description in `PROPOSAL.md`, the actual bug swapped a **local variable**
(`maxVal`, computed per-call from `bitSize`) for a **package-level named
constant** (`maxUint64`) — not const-for-const. A v1 that's
package-const-only won't produce that specific mutant. This is a deliberate
trade: it ships a real, useful, combinatorially tame operator now (many
real-world "picked the wrong sibling constant" bugs *are* this shape),
while being explicit that closing the strconv case study fully needs a v2
extension:

**v2 — done, landed 2026-08-11 (`internal/mutator/identifier/constswap.go`,
committed `84fe29b`).** Extends the candidate set to local variables whose
declared type exactly matches a package-level constant's type, restricted
to identifiers used as an operand of a comparison (`<`, `<=`, `>`, `>=`,
`==`, `!=` — the same "boundary-relevant" spirit as `operator/boundary`).
The heuristic below turned out insufficient on its own: the first version
had no file-scoping restriction and produced a 24x mutant-count blowup
against real code; fixed by further restricting candidates to
package-level constants declared in the *same file* as the local
variable's use. Verified stable (81 mutants, not an unbounded blowup) via
direct binary testing against a real fixture — see PROGRESS.md's
2026-08-11 entry.

**1b. Getting type info without making every run pay for it.** Two options:

- (Rejected) Always add `NeedTypes | NeedTypesInfo | NeedSyntax | NeedDeps`
  to the one `packages.Load` call in `load()`. Rejected because it changes
  the cost and failure profile of *every* run, including ones that never
  select this operator — contradicting `load()`'s own stated rationale — and
  because a module that doesn't fully type-check (common mid-refactor, or a
  module that's intentionally excluded from `go vet` for now) would then
  fail package resolution for the *whole* engine, not just this operator.
  Today only a `packages.Load`-visible syntax/name error fails the run.

- (Chosen) Type-check lazily, gated on whether the resolved operator set
  actually needs it. Add:

  ```go
  // needsTypes reports whether any mutator in the set requires static type
  // information, i.e. implements mutator.TypedMutator.
  func needsTypes(mutators []mutator.Mutator) bool

  // loadTyped re-resolves opts.patterns() with full type information,
  // keyed by PkgPath, for TypedMutator operators to consult. It is only
  // ever called when needsTypes reports true — the common run pays nothing
  // for this.
  func loadTyped(ctx context.Context, opts Options) (map[string]*packages.Package, error)
  ```

  `[Run]` (engine.go) calls `loadTyped` right after the existing
  `load(ctx, opts)` call, only if `needsTypes(mutators)`, and threads the
  result into `plan()`. `loadTyped` uses
  `packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax |
  packages.NeedImports | packages.NeedDeps` — the mode `load()`'s comment
  explicitly avoids today, now scoped to only the callers who ask for it.

  A package that fails to type-check under `loadTyped` is demoted for that
  operator only — the exact fail-soft precedent `[planScope]` already uses
  for `ScopeImpact` (engine.go: *"Demoting this one package costs time and
  nothing else, whereas failing the run would throw away every other
  package's work"*). Here, the demotion is "run this package's mutants
  without the identifier/const-swap operator," not a scope change.

**1c. Interface shape — do not touch `Mutator`.** `[Mutator]` (mutator.go) is
a two-method interface every one of the 11 operators implements statelessly.
Adding scope/type context to it — e.g. `Applies(node ast.Node, info
*types.Info) bool` — would force all 11 existing, purely-syntactic operators
to accept a parameter they ignore. That directly contradicts the package
doc's stated design goal (mutator.go's package doc comment): keep the
interface to exactly what an operator needs, materialized as a slice of
`Mutation`, nothing more.

There is also a concurrency hazard to solve, not just an ergonomics one. A
single `Mutator` instance is shared and reused across the whole run
(`[All]`, mutator.go), and files — including multiple files of the *same*
package — are mutated concurrently up to `Options.Parallel` (engine.go's
`fileJob`/`[execute]`). If a typed operator's scope/type info were set as a mutable field
on the shared instance (e.g. an `Init(info, pkg)` method called once per
package), two files of the same package running concurrently would race on
that field.

Chosen shape: a second, optional interface, additive to `Mutator`, that
returns a **new, package-bound value** rather than mutating the shared one:

```go
// TypedMutator is implemented by an operator whose eligibility depends on
// static type information the plain Mutator interface has no way to
// receive. It is checked once per run (engine.needsTypes) and, when
// present, invoked once per package.
type TypedMutator interface {
    // WithScope returns a Mutator bound to one package's type information.
    // The returned value is used for every file of that package; it is
    // never the same value WithScope was called on, so the original,
    // registry-held instance stays stateless and safe to reuse — including
    // concurrently across the other packages in the same run — exactly as
    // Mutator's contract already requires.
    WithScope(info *types.Info, pkg *types.Package) Mutator
}
```

In `[plan]` (engine.go), per package: if `needsTypes` was
true and this package resolved under `loadTyped`, build a **per-package**
mutator slice — for each mutator in the run's shared set, if it implements
`TypedMutator`, replace it with `.WithScope(info, pkg)`'s return value;
otherwise keep it as-is (identical instance, zero cost). Store this slice on
`fileJob` (currently `mutators []mutator.Mutator`, shared across every job)
instead of the run-wide shared slice — most jobs' slices are unchanged
(same underlying instances for the other 10+ operators); only the jobs for
packages where the const-swap operator is active get a bound replacement.

**1d. Node shape and mutation mechanics.** `Applies(node)` on the
package-bound instance: `node` is an `*ast.Ident` that is a *use* (not a
declaration — `info.Defs[node]` would be nil, `info.Uses[node]` resolves it)
of a `*types.Const` satisfying the 1a scope restriction. `Mutate(node)`
walks `pkg.Scope()` for sibling consts in the same block/file with
`types.Identical` types, and returns one `Mutation` per candidate, each
directly rewriting the `*ast.Ident.Name` field — the same in-place-field-edit
idiom `literal/number.go`'s `shiftBy` already uses for `*ast.BasicLit.Value`
(no replacement node needed, since `go/printer` only reads `Name`):

```go
return mutator.Mutation{
    Description: original + " -> " + candidate.Name(),
    Apply:       func() { ident.Name = candidate.Name() },
    Revert:      func() { ident.Name = original },
}
```

**1e. Default operator set.** Registering via `init()` and `mutator.Register`
(the same pattern every existing operator uses) automatically makes this
operator part of `mutator.All()`, and therefore part of the *default*
(no `-mutateoperators` flag) run. The type-checking cost is paid once per
run via `loadTyped`, not once per mutant, so it's small relative to the
`go test` cost that already dominates a run — recommend keeping the existing
plugin-registration precedent (mutator.go: *"an operator package is enabled
purely by being imported"*) rather than special-casing this one operator out
of `All()`, which would start coupling the registry to a cost tier it
doesn't otherwise track.

**Open question, not resolved here**: is default-on actually right, given
the task's own framing that untamed identifier swapping "could explode
mutant counts badly"? The 1a scoping keeps counts tame *by construction*, so
this plan leans default-on — but this should be revisited once real mutant
counts from a run against, say, `crypto/x509/pkix` or `strconv` are in hand,
the same way the other operators were validated.

### Files/functions touched

- `internal/mutator/mutator.go` — add `TypedMutator` interface (additive;
  zero change to existing operators or `Register`/`New`/`List`/`All`).
- `internal/mutator/identifier/constswap.go` (new package, new file) —
  `ConstSwapName = "identifier/constswap"`, the `constSwap` type
  implementing `Mutator` + `TypedMutator`, package doc comment explaining
  the scope restriction and the TypedMutator opt-in, matching the
  explain-the-why-up-front style of `statement/remover.go`'s package doc.
- `internal/mutate/engine.go` — `needsTypes`, `loadTyped`, wiring into
  `[Run]` and `[plan]`; `fileJob.mutators`
  becomes per-package-derived instead of always the run-wide shared slice;
  blank import `_ "github.com/jh125486/turango/internal/mutator/identifier"`
  added to the existing side-effect import block.
- `cmd/turango/main.go` — no change needed; `-mutateoperators=` already
  forwards arbitrary names to `mutator.New`, and `mutator.List()`
  automatically includes the new registry name.

### Build order

1. `mutator.TypedMutator` in mutator.go — pure addition, no behavior change,
   safe to land alone.
2. `internal/mutator/identifier/constswap.go` implementing the v1 (const-only,
   same-block/file, exact-type) scope, unit-testable in isolation (see
   Verification) independent of the engine.
3. `engine.go`: `needsTypes`, `loadTyped`, `plan()`'s per-package bound-slice
   construction, fail-soft demotion on a per-package type-check failure.
4. Wire the blank import to register the operator.
5. Engine-level integration test (see Verification) proving the default-run
   cost claim: a run that doesn't select the operator triggers zero calls to
   `loadTyped`.

### Verification

- `internal/mutator/identifier/constswap_test.go`, table-driven, following
  the existing operator-test convention
  (`internal/mutator/operator/tokens_test.go`'s `parseFunc`/`render`
  helpers) — except `parseFunc`'s own doc comment states
  *"snippets are never type-checked, so they need only parse"*, which is
  exactly the shortcut this operator's tests cannot take. Add a
  package-local helper, e.g.
  `typeCheckFunc(t, src) (*token.FileSet, *ast.File, *types.Info, *types.Package)`,
  built directly on `go/types.Config.Check` against a small synthetic
  package (no `packages.Load`/module needed for a unit test — fast,
  self-contained, matching the existing operator tests' sub-millisecond,
  no-toolchain-shell-out character).
  - Cases: two same-type consts in one block → swap offered, both
    directions; different types → no swap; same-type consts in *different*
    blocks/files → no swap (this specifically asserts the scope
    *restriction* is enforced, not just documented — the easiest way for
    this operator to regress is silently dropping the block/file check);
    a const with no same-type sibling → `Applies` false.
- `internal/mutate/engine_test.go`: extend `fixtureModule` (or add a sibling
  fixture) with two package-level constants of the same type where swapping
  one for the other is compilable and behaviorally different. Assert:
  - `Run(ctx, Options{Packages: []string{"./..."}, Dir: root, Operators:
    []string{"identifier/constswap"}})` produces the expected mutant(s);
  - a run with the default operator set *excluding* this name (or, if 1e's
    default-on recommendation stands, a run under an older operator list)
    does not invoke `loadTyped` — this is the concrete regression test for
    the "default run pays nothing" claim, not just an assertion about
    output shape.
- Explicitly flagged as follow-on, not a v1-blocking test: reconstructing
  the strconv `ParseUint` shape as a fixture and confirming the *v2* local-var-
  to-const extension (not built in this plan) reproduces that exact mutant —
  the honest state after v1 is "this operator does not yet close that
  specific case study," and a test claiming otherwise would be dishonest.

---

## 2. TCE (Trivial Compiler Equivalence)

**Status: done**, with two decisions made during implementation worth
recording since they revise this section's plan:

- **2a's design changed** from raw archive comparison to normalized `-S`
  disassembly comparison — see the spike result below 2b; the original
  archive-comparison plan is superseded and kept in place (struck through in
  spirit, not literally) only so the "what we tried and why it didn't work"
  reasoning survives.
- **Default-off, not default-on.** Section 2c's open question leaned toward
  shipping TCE opt-out (on by default, `-mutatetce=false` to disable). The
  actual implementation ships opt-in instead (`Options.TCE` zero value is
  `false`; `-mutatetce=true` to enable): every other `Options` field's zero
  value is already the safe default in this codebase (`ScopeFull` catches
  the most kills, `Parallel: 0` means serial, not "guess a concurrency
  level"), and the risk direction here is asymmetric in a way scope isn't —
  a narrower scope can only *under*-report kills, never mis-report one,
  while a false positive in TCE's compiled-output comparison would silently
  discard a real mutant. That's exactly the class of risk the project's own
  "ship the simple default, add a flag only once proven necessary" instinct
  (cited in the original open question, re: why `Scope` and not
  `TestTimeout` got a flag first) argues for defaulting *off* until the
  design has more real-world runs behind it, not defaulting on with an
  escape hatch. Flipping the default is a one-line change once that
  confidence exists.

### Problem

`runner.run` (runner.go lines 96–178) already filters one class of no-op
mutation — a *syntactic* one:

```go
var mutated bytes.Buffer
if err := printer.Fprint(&mutated, m.fset, m.file); err != nil { ... }

src := mutated.Bytes()
if bytes.Equal(src, m.baseline) {
    return nil, nil
}
```

This catches a mutation `go/printer` renders identically to the original
(e.g. an implicit empty statement replacing an already-empty one). It does
**not** catch a mutation that prints as genuinely *different* source text but
compiles to semantically identical code — a classic equivalent mutant (e.g.
`i*1` vs `i`, or deleting a dead store nothing downstream reads). A
`Survived` verdict on one of these is noise: the suite didn't miss a real
behavioral gap, because there was no behavioral difference to notice.
`PROPOSAL.md`'s "Costs and risks" section already documents this as a known,
only-partially-solved limitation.

TCE ([Papadakis, Jia, Harman & Le Traon, "Trivial Compiler Equivalence: A
Large Scale Empirical Study of a Simple, Fast and Effective Equivalent
Mutant Detection Technique," ICSE'15][tce]) is deliberately unsophisticated:
compile both versions, compare the resulting object code, declare
equivalent on a match. Per this project's own prior
research pass (referenced in the task framing), it should be **always-on**,
unlike mutant subsumption ([Ammann, Delamaro & Offutt, "Establishing
Theoretical Minimal Sets of Mutants," ICST'14][subsumption]; a *selection*
technique that trades completeness for speed and should stay opt-in) — TCE
is a *filtering* technique with no completeness trade-off, assuming the
compiled-equivalence check itself is correct.

### Pipeline placement

Today, `run.run`'s sequence is: apply mutation → print → compare to baseline
(syntactic no-op check, early return) → build `MutantResult` skeleton →
(under `ScopeImpact`) early-return if uncovered → resolve `go test` args →
`os.MkdirTemp` → `copyModule` → write mutated file → `r.goTest` → `classify`.

TCE's insertion point is **after the syntactic check, after the workspace
copy and mutated-file write (both are still needed — compiling requires the
whole module graph to resolve imports, exactly per `copyModule`'s own doc
comment about vendor/replace/embed), and before `r.goTest`**. Concretely, a
new step compiles the mutated package alone (no test files, no link) and
compares it against a **precomputed-once-per-package** baseline compile —
mirroring the `ScopeImpact`/`buildImpact` pattern exactly: `plan()` already
builds a per-package artifact once, before any mutant of that package runs,
because the thing being computed (a coverage map; here, a compiled baseline)
doesn't change across that package's mutants.

The baseline half needs no throwaway copy at all: nothing is mutated yet, so
building directly against `pkg.Module.Dir` is safe — the same reasoning
`goTestSuite`'s baseline-timing run already uses (runner.go lines 293–303:
*"It runs against the original sources, not a workspace copy: nothing is
mutated, so there is nothing to isolate"*).

### Design decisions

**2a. Compare normalized `-S` disassembly, not raw archive bytes.**
Superseded by the spike below: the original plan ("compare compiled package
archives") produces false "different" results for genuinely equivalent
mutants, because `.a` archives embed source-line-position export data that
shifts whenever the source's line count changes — unrelated to codegen.
The validated design instead: `go build -trimpath -gcflags="-S
-buildid=<const>" -o /dev/null <pattern>` (no `all=` prefix — that would
also dump every dependency's assembly, confirmed painfully in the spike: a
one-package build of `internal/mutator/literal` produced a 1.6M-line dump
including the entire stdlib closure) captures the target package's own
assembly listing on stderr. Normalize by stripping the trailing
`(file.go:N)` position comment from each line (a single regexp,
`\([^)]*\.go:\d+\)`), then compare the normalized text. This is *more*
expensive than a linked binary comparison would suggest (`-S` disassembly
text, not raw bytes) but still cheaper than `go test`'s build path (no test
files, no link), and it's the granularity that actually distinguishes
"no code difference" from "line count changed."

**2b. Reproducibility is the real risk, and it needs two specific flags, not
one.** Go does not guarantee byte-identical output across two builds of the
same source unless specific conditions are controlled for:

- **`-trimpath`**: without it, the compiler records each build's *absolute
  file path* into debug/embed metadata. The baseline compiles from
  `pkg.Module.Dir`; every mutant compiles from a fresh `os.MkdirTemp` copy at
  a different path. Without `-trimpath` these two builds would differ purely
  because of *where* they ran, which would make every mutant "not
  equivalent" by construction — the opposite failure mode from a false
  equivalence, but just as useless. `-trimpath` rewrites recorded paths to a
  module-relative form (`<module>@<version>/…` under modules), which is
  exactly what's needed for two different temp directories holding
  byte-identical *content* to produce byte-identical output. Go 1.21+
  toolchains are documented as achieving genuinely reproducible builds under
  this flag (see [go.dev/blog/rebuild], ["Reproducing Go binaries
  byte-by-byte"][filippo-repro] — the latter by Filippo Valsorda, who not
  coincidentally is also the author of golang/go#75315, the related open
  proposal `PROPOSAL.md` already cites).
- **A fixed `-buildid`**: separately from paths, the Go toolchain embeds a
  build ID (an action/content hash) that the go command computes itself,
  which is deliberately sensitive to inputs including — depending on
  toolchain version and cache state — data this comparison does not want
  varying, and which will *correctly but unhelpfully* differ between
  baseline and mutant simply because the source changed (which is not the
  signal being measured; the signal is "does the **rest** of the compiled
  output differ"). The compiler accepts an explicit `-buildid` value
  (`go tool compile -buildid <id>`, reachable through `go build
  -gcflags=-buildid=<id>`); pass the same fixed literal string to both the
  once-per-package baseline compile and every per-mutant compile, so this
  field can never be the source of a spurious diff in either direction.

[go.dev/blog/rebuild]: https://go.dev/blog/rebuild
[filippo-repro]: https://words.filippo.io/reproducing-go-binaries-byte-by-byte/
[tce]: https://doi.org/10.1109/ICSE.2015.103
[subsumption]: https://doi.org/10.1109/ICST.2014.13

**Genuinely unresolved — flagged, not papered over**: this project has not
empirically verified that `go build -o` with `-trimpath` and a fixed
`-buildid` produces byte-identical archives for identical (module-relative)
source built from two different temp directories, on a real,
moderately-complex package, under turango's specific execution model
(different `GOCACHE` state per run; whatever incidental non-content
variance the toolchain might carry that the published reproducibility
guarantees — demonstrated at the level of `go build` reproducing the
*toolchain's own binaries* across machines — don't directly cover for an
arbitrary target package built from scratch temp dirs repeatedly in one
process). **Before wiring TCE into the default pipeline, run a standalone
spike**: build the same package twice, from two different `os.MkdirTemp`
copies, with `-trimpath -gcflags=-buildid=x`, and diff the two `.a` files
byte-for-byte on something nontrivial (this repo's own `internal/mutator`,
or `crypto/aes`) to confirm zero incidental diffs. This spike is cheap
(minutes) and is the single highest-leverage thing to do before committing
to this design — if it fails, the fallback below is the plan B, not raw
byte comparison.

**Fallback if the spike fails**: compare `go tool compile -S` output
(textual Go assembly listing) instead of raw archive bytes — closer to what
some other-language TCE implementations compare in practice (a
disassembled/normalized form rather than raw bytes), trading "trivial byte
compare" for "textual diff," at some extra cost and complexity. Not the
preferred path; stated here so a spike failure has a documented next step
rather than stalling the whole gap.

**Spike result (run, not hypothetical): 2a's raw-archive design fails; the
fallback is required, and it works.** Two builds:

1. Identical source, two different temp-dir copies, `-trimpath
   -gcflags=all=-buildid=x`, `go build -o out.a`: the resulting `.a` files
   were byte-identical (confirmed via `cmp` and SHA-256) — the trimpath/
   fixed-buildid mechanics themselves are sound, path/buildid noise is not
   the problem.
2. A genuine dead-store-elimination case (`total = 999` immediately
   overwritten by `total = 0` before any read, then the dead line deleted)
   — the textbook equivalent mutant this whole gap exists to filter — was
   built both ways and the two `.a` files **differed**, `cmp` reporting a
   mismatch 57 bytes in. That offset is the `ar` archive's member-size
   field for the `__.PKGDEF` member (604 vs 578 bytes): Go's export data
   encodes source line positions for the package's declarations, and
   deleting one line shifts every position after it — a difference with
   nothing to do with the generated machine code. **Raw archive comparison
   produces a false "different" for a textbook equivalent mutant purely
   from unrelated line-shift noise, for any mutation that changes the
   source's line count** (which is most of them) — this isn't a corner
   case, it would make 2a's design detect almost nothing in practice.
3. The same two builds compared instead via `go tool compile -S` (assembly
   listing) and normalized by stripping the trailing `(file.go:N)` position
   comment from each line: **every opcode, register, and address was
   byte-identical** — the compiler genuinely eliminates the dead store, and
   the fallback comparison correctly says so.
4. Sanity check against a false positive in the other direction: the same
   fixture with a real behavioral change (`total = 0` → `total = 1`)
   produced a real, position-independent diff (`MOVD ZR, R3` vs `MOVD $1,
   R3`, and the differing instruction bytes) even after the same
   line-number normalization — confirming the method isn't just
   "always equal."

**Decision: adopt the fallback (`go tool compile -S`, normalized) as the
actual design for 2a**, not raw archive comparison. Section 2a above
("Compare compiled package archives, not linked binaries") is superseded by
this result and should be rewritten before implementation proceeds — the
granularity that actually works is a normalized disassembly diff, not a
byte-for-byte archive compare. 2b's trimpath/fixed-buildid mechanics are
still correct and still needed (validated in step 1 above); only the
comparison target changes. The spike's throwaway fixtures are not
committed (per the build order's own instruction) — reproducible from the
description above if re-verification is ever needed.

**2c. Reporting: exclude, don't add a fourth `Status`.** Two shapes
considered:

- A new `Status` value (`Equivalent`), alongside `Killed`/`Survived`/
  `NotViable`, added to `Counts()` and excluded from `Score()`'s
  denominator the same way `NotViable` already is.
- Exclude from `Result.Mutants` entirely; track separately, mirroring how
  `SuppressionResult` already works.

`report.go` has already drawn this exact line once, for suppression, with a
rationale that generalizes word-for-word to TCE (report.go lines 123–130):

```go
// SuppressionResult ... is not a mutant and never becomes one ... That is
// why suppressions are tracked separately from [MutantResult] rather than
// as a fourth [Status] — there is no verdict to record, only the fact that
// nothing was attempted.
```

TCE's finding — "there is no code difference to test" — is the identical
shape of claim as "nothing was attempted": neither is a verdict about
whether the test suite caught something, because there was nothing
behavioral to catch. **Chosen: follow the existing precedent.** Add:

```go
// EquivalentResult records one mutation the compiled-output comparison
// (TCE) found semantically identical to the unmutated package: the mutant
// was never run against the test suite because there was nothing for the
// suite to have a chance of catching.
type EquivalentResult struct {
    File        string
    Line        int
    Operator    string
    Description string
    // no Status, no Output: nothing ran.
}
```

`Result.Equivalents []EquivalentResult`, `Result.EquivalentCount() int`,
excluded from `Score()`'s denominator exactly like suppression and
`NotViable` already are, and reported in `WriteSummary` as a sibling line to
the existing suppression-ratio line — with a cross-reference in the doc
comment to `SuppressionRatio`'s *"the two numbers are only trustworthy
together"* framing, but with an important difference worth stating in the
comment: a suppression ratio being high is a legitimate (if worth
scrutinizing) human choice; a high equivalent-filtered count driven by a
*false positive* in the compiled-equivalence check would be a tool
correctness bug, not a scoring-gaming concern — which is exactly why the 2b
spike matters so much before this ships as always-on.

### Files/functions touched

- `internal/mutate/report.go` — `EquivalentResult`, `Result.Equivalents`,
  `EquivalentCount()`, `Score()`/`WriteSummary` doc updates, a
  `writeEquivalents` sibling to `writeSuppressions`.
- `internal/mutate/runner.go` — new `compile(ctx, dir, pattern string)
  ([]byte, error)` helper (shells `go build -trimpath
  -gcflags=-buildid=<const> -o <tmp>.a <pattern>`, reads the archive back);
  `run.run` (lines 96–178) gains the short-circuit branch between the
  syntactic no-op check and the existing `testArgs`/`goTest` call.
- `internal/mutate/engine.go` — `plan()` (lines 460–524) gains a
  per-package once-only baseline compile, paralleling `buildImpact`;
  `fileJob` gains `tceBaseline []byte`; `Options` gains a TCE toggle (see
  open question below on whether it's a flag at all).
- `cmd/turango/main.go` — only if an opt-out flag is added: a new
  `flagTCE` following the existing `set()`/`splitMutateFlag` pattern (a
  boolean parse via `strconv.ParseBool`, the same shape as `flagParallel`'s
  `strconv.Atoi`).

### Build order

1. **Spike first** (throwaway, not committed): validate 2b's
   trimpath+fixed-buildid archive comparison on a real package. This gates
   whether to proceed with 2a/2b as designed or fall back to the 2b
   fallback.
2. `report.go`: `EquivalentResult`/`Result.Equivalents`/`EquivalentCount()`/
   scoring exclusion — pure data-shape addition, testable in isolation the
   same way `report_test.go`'s existing `TestSuppressionRatio` and
   `TestResultJSONRoundTrip` test the analogous suppression shape.
3. `runner.go`: `compile()` helper, unit-tested directly (see Verification)
   — this is the load-bearing test that would have caught a broken
   reproducibility assumption, and must be written and passing *before*
   wiring TCE into the run loop, not after.
4. `engine.go`: `plan()`'s per-package baseline compile, `fileJob
   .tceBaseline`, the `Options` toggle.
5. `runner.go`: wire the short-circuit into `run.run`.
6. `cmd/turango/main.go`: flag wiring, only if the toggle is kept as a flag.
7. Engine-level integration test (see Verification) with a deliberately
   compiler-equivalent mutation fixture.

### Verification

- `compile()` unit test: build the same tiny fixture package from two
  different `t.TempDir()` copies of *textually identical* source — archives
  must compare equal; build from two copies where one line differs in a
  behaviorally meaningful way — archives must compare unequal. This is the
  test that stands in for the spike once the design is trusted; both must
  pass before anything downstream is wired up.
- `internal/mutate/engine_test.go`: extend `fixtureModule` (or add a
  sibling) with a function containing a demonstrably dead store — e.g. an
  assignment to a local variable no later statement reads — so that
  `statement/remover` deleting it is a textbook compiler-equivalent mutant
  (dead-store elimination). Assert:
  - with TCE enabled, `Run()` reports it under `Result.Equivalents`, *not*
    in `Result.Mutants`, and `EquivalentCount() == 1` (or whatever count the
    fixture implies);
  - with TCE disabled (if an opt-out flag exists), the same mutation shows
    up as an ordinary `Survived` `MutantResult` — proving the toggle
    actually reverts to today's behavior, not just that the new path exists.
    This dual-mode assertion pattern already exists in this codebase for an
    analogous "prove both branches" property (`TestRunWithMultipleWorkers
    MatchesSingleWorker`-style tests, per `engine_test.go`'s single-worker-
    vs-parallel comparison around line 202).

### Open question

Should TCE ship with an opt-out flag (`-mutatetce=off`) at all, or be
unconditionally on with no escape hatch for v1 — matching this project's
general "ship the simple default, add a flag only once proven necessary"
instinct (visible in how `Scope`, not `TestTimeout`, got flags first)? This
plan leans toward including the opt-out from the start specifically
*because* of the 2b reproducibility risk: if the spike's guarantees turn out
to hold only in most cases, not all, an escape hatch matters more here than
it did for e.g. `-mutatescope`, where a wrong answer is conservative
(reports more survivors, not fewer) rather than a potential false negative
(silently discarding a real mutant as "equivalent"). Not resolved here;
worth deciding after the spike, not before.

---

## 3. Before/after source snippet on `MutantResult`

**Status: done**, with one correction to the plan found during
implementation: the "Files/functions touched" section below says all 11 (now
13) operator files need a `Mutation.Node` addition. That turned out to be
wrong — only operators whose `Apply` *replaces a pointer* (repointing a
container's field or list slot at a different node) need it, because
printing the *container* already reflects the change for every operator
whose `Apply` edits a field *in place* (a token, a literal's `Value`, an
`Ident`'s `Name`) on the exact node the walk already handed `Mutate`. That
covers `expression/remove`, `identifier/constswap`, `literal/{boolean,
number}`, and every `operator/*` token-swap operator, including `unary`
(its `Apply` repoints a slot, but that slot is a *field of the container*,
so printing the container — the nil-fallback — still shows the diff
correctly). Only `control/{if,else,case}` (narrower than the nil-fallback:
excludes the condition, matching "remove if body"'s own scope) and
`statement/remover` (one container can hold many statements; the
nil-fallback would print the whole block for every single-statement
mutation) actually needed `Node` set. A second wrinkle the plan didn't
anticipate: for `statement/remover`, printing the same `Node` after `Apply`
naturally shows *unchanged* text, since `Apply` repoints the list's slot
rather than editing `stmt` itself — `MutantResult.After` resolves this by
reporting empty when the pre/post render is identical, rather than a stale
duplicate of `Before`.

### Problem

`MutantResult.Description` (report.go) is a terse
operator-authored summary, e.g. `"== -> !="` or (from `statement/remover`'s
`describe`) a truncated rendering of the deleted statement. It is not the
actual before/after source text of the mutated node(s), so reconstructing
what changed — to paste into an LLM prompt or a proposal write-up, per the
task's own framing — means re-deriving it by hand from `File`/`Line` and a
checkout of the source at that point. Lower risk than gaps 1–2: this is a
data-shape addition plus a mechanical touch of every operator, not a new
architectural capability.

### Design decisions

**3a. Capture the mutated *node's* printed text, not a whole line.** A
mutation can span multiple lines (`statement/remover` deleting a multi-line
statement; `control/if` emptying a multi-statement body), so "the line" is
either ill-defined or would need multi-line handling anyway. The mutated
node's own printed span is well-defined for every operator and reuses
`go/printer`, already in use throughout this package (`mutateFile`'s
baseline print, `run.run`'s mutated print, and — precedent for printing a
sub-node rather than a whole file — `tokens_test.go`'s `render()` test
helper, which already does exactly `printer.Fprint(&buf, fset, node)` for a
single node rather than a file).

**3b. Which node to print is not always the walk's outer node.**
`MutantResult.Before`/`.After` need to be captured around `run.run`'s
existing `m.mutation.Apply()`/`defer m.mutation.Revert()` pair (runner.go).
The walk (`[mutateFile]`, engine.go) does carry a `node ast.Node` per
iteration, but for `statement/remover` specifically, that outer node is the
*container* (`*ast.BlockStmt`/`*ast.CaseClause`), not the individual
statement being deleted — per that operator's own package doc comment
(statement/remover.go's package doc): `Applies`/`Mutate` are called on the
container because *"a walk hands a visitor the node alone — no parent, no
slot"* to replace a single element of its parent's list. Printing the walk's
outer `node` for this operator would render the whole block, not the
deleted line — wrong granularity, and a real discrepancy from what
`Description` already narrows to (a single statement, per `describe(i,
stmt)`).

Chosen fix: add an optional field to `mutator.Mutation` itself:

```go
// Node is the specific AST node this mutation edits, used only for
// reporting the mutated source text before and after Apply. Most operators
// mutate exactly the node their Applies/Mutate were called on and can set
// this trivially; statement/remover is the one exception in this codebase
// — it is called on a statement *container* but edits one element of its
// list — so it sets Node to that specific ast.Stmt instead. Nil falls back
// to the node the engine's walk was visiting, which is only ever wrong for
// an operator with this same container/element split.
Node ast.Node
```

`statement/remover.go`'s `Mutate` loop already has the exact
right value in scope as a captured local — `stmt`, the loop variable per
`for i, stmt := range list` — so setting `Node: stmt` there is a one-line
addition, not a restructuring.

The alternative — leave `statement/remover`'s snippet as "whole block, not
just the deleted line" for v1, and skip the `Mutation.Node` field entirely —
is the pragmatic shortcut if touching all 11 operator files isn't worth it
for this gap. Flagged as a real trade-off, not silently resolved: the
`Mutation.Node` approach is recommended because it's a small, mechanical,
low-risk change per operator (most already have the exact right node in
scope in their `Mutation{}` literal, e.g. `operator/binary`'s `expr`), and
because a wrong/misleading snippet for one specific operator seems worse
than the small mechanical cost of getting it right everywhere.

**3c. Console vs. JSON.** `[Result.writeSurvivors]` (report.go) is
explicitly designed as a scannable table — its own doc comment cites a
"ragged left edge... is what makes it hard to scan" concern. Dumping full
before/after source into every row would break that design goal for a use
case (LLM-prompt-paste, proposal-doc drafting) this table was never meant to
serve. Recommend: `Before`/`After` populate on `MutantResult` unconditionally
(so they're always in the JSON report), but the console `writeSurvivors`
table is left unchanged — `Description` remains the only per-row text
shown. A future opt-in verbose console mode is a reasonable follow-up, out
of scope here.

### Files/functions touched

- `internal/mutator/mutator.go` — `Mutation.Node ast.Node` (additive; nil is
  a valid, already-correct zero value, so no existing operator breaks by
  not setting it — though runner.go will use the walk's own node as a
  fallback rather than emitting an empty snippet).
- All 11 operator files under `internal/mutator/{control,expression,literal,
  operator,statement}/*.go` — each `Mutation{}` literal gains `Node: <the
  node already in scope>`. Mechanical; most files need one line added per
  returned `Mutation`.
- `internal/mutate/report.go` — `MutantResult.Before`, `.After string`
  fields, doc comment cross-referencing `Description`.
- `internal/mutate/engine.go` — `[mutateFile]` already tracks
  `spec.line`/`spec.operator` per node in its `ast.Inspect` callback; add
  `spec.node = node` alongside those as the fallback the runner uses when
  `Mutation.Node` is nil.
- `internal/mutate/runner.go` — `mutant` struct gains a `node
  ast.Node` field (the walk-level fallback, set by `mutateFile` above); a
  new small `renderNode(fset *token.FileSet, node ast.Node) (string,
  error)` helper (parallels the printer calls already inline in this file
  rather than introducing a shared abstraction, consistent with how
  `mutateFile` and `run.run` already both call `printer.Fprint` directly);
  `run.run` calls it once before `Apply()` and once after `Apply()`/before
  `Revert()`, using `m.mutation.Node` if non-nil else `m.node`, and
  populates `result.Before`/`result.After`.

### Build order

1. `mutator.Mutation.Node` field — additive, inert until read, safe to land
   alone.
2. Update all 11 operators to set `Node`, one file at a time, re-running
   that operator's existing table-driven test after each — the field is
   unread by anything yet, so this step cannot regress behavior, only add
   data.
3. `report.go`: `MutantResult.Before`/`.After` fields.
4. `engine.go`: `spec.node` fallback in `mutateFile`.
5. `runner.go`: `mutant.node`, `renderNode`, before/after capture in
   `run.run`, populate the new result fields.
6. Confirm JSON round-trip: `MutantResult` has no explicit `json:` tags
   today (fields serialize under their Go names — `"File"`, `"Line"`, etc.,
   per the existing shape `report_test.go`'s `TestResultJSONRoundTrip`
   exercises), so `Before`/`After` need no tag handling of their own.

### Verification

- Extend each operator's existing table-driven test (e.g.
  `internal/mutator/operator/binary_test.go`'s `TestBinaryMutate`) to assert
  `mutations[0].Node != nil` and that printing it (via the same `render()`
  helper the test already imports) matches the "before" text the test case
  already encodes as `tt.src` — a natural, mechanical extension of the
  existing shape, not a new test pattern.
- `internal/mutate/report_test.go`: extend the existing `MutantResult`
  fixtures used by `[TestResultJSONRoundTrip]` and `[TestWriteSummary]`
  with `Before`/`After` values; assert the JSON round-trips them
  and that `WriteSummary`'s console output is byte-for-byte unaffected (the
  3c decision: console stays `Description`-only).
- `internal/mutate/engine_test.go`: add a small, targeted test (not
  necessarily reusing the slow `TestRunAgainstRealModule`) asserting a
  known mutation on the `fixtureModule` fixture — e.g. an `operator/binary`
  swap on `mathx.Clamp`'s `v < lo` — produces a `MutantResult` whose
  `Before` contains `"v < lo"` and whose `After` contains `"v >= lo"`. A
  concrete line-level assertion, not merely "both fields are non-empty" —
  non-emptiness wouldn't catch Before and After accidentally being swapped
  or both capturing the same (post- or pre-mutation) text.

---

## 4. Deterministic mutant IDs

**Status: done.** SHA-256-hashed, 12-hex-char IDs (`internal/mutate/engine.go`'s
`mutantID`), a `MutantResult.ID` field, and the `-mutatemutant=<id>` replay
flag are all implemented and tested, matching this section's design and
build order below.

### Problem

Mutants are currently identified only by `File:Line` + `Operator` +
`Description` — enough for a human reading one report, but not stable
enough to reference a *specific* mutant across runs: in a code comment
(`//nomutant` referencing why a particular mutant is excluded), in a
regression test asserting "this exact mutant must stay Killed," or in a
bug report saying "mutant X started surviving after commit Y." `-fuzz`
solves the analogous problem with content-hashed corpus filenames
(`testdata/fuzz/FuzzXxx/<hash>`) and `go test -run=FuzzXxx/<hash>` to
replay one specific case. Mutation testing wants the same thing: a stable
ID plus a way to replay exactly one mutant by it.

### Design decisions

**4a. Content hash, not a counter.** A sequential/positional ID (e.g. "the
14th mutant discovered") is unstable across any source edit upstream of a
given mutant, across `-mutateparallel` reordering, and across operator-set
changes (adding a 12th operator shifts every subsequent index). Instead,
hash a canonical tuple: `file path relative to module root, node's
Pos().Line:Pos().Column, operator Name(), and the mutation's index within
that node's Mutate() slice` (the index is necessary because one
node/operator pair can produce multiple mutations — `expression/remove`
returns two, one for each operand — and those need distinct IDs even
though they share every other coordinate). SHA-256 the tuple, truncate to
a short hex prefix (8–12 chars, matching the length precedent of a git
short SHA) for something a human can read in a report or paste into a
comment without it dominating the line.

**4b. Stability trade-off, stated honestly.** This ID is stable across
*re-runs of unchanged source* (the whole point), but not across *edits* to
the file above the mutated line (line numbers shift) — same limitation
`-fuzz`'s corpus hashing doesn't have (it hashes input bytes, not
position), but unavoidable here since the "input" to a mutation is a
position in a specific AST, not a self-contained byte string. Worth
documenting plainly rather than overselling the guarantee: the ID is for
"reproduce this exact mutant in this exact source," not "track this
logical mutation across refactors."

**4c. Where it's computed.** `internal/mutate/engine.go`'s `mutateFile`
already has `fset`, `path`, the current node, `m.Name()`, and the
mutation's slice index all in scope inside the `ast.Inspect` callback
(the same place `spec.operator`/`spec.line` are set today) — the hash
computes there, gets threaded onto `mutant.id` (a new field on the
existing `mutant` struct in `runner.go`) and surfaces as a new
`MutantResult.ID` field in `report.go`. Additive to both structs, no
existing field changes shape.

**4d. Replay flag.** A new CLI flag, `-mutant=<id>` (sibling to
`-mutate`/`-mutatescope`/etc., same `=`-only parsing `main.go` already
enforces for every `-mutateXXX` flag — this one likely wants the same
`-mutateXXX` prefix for consistency, i.e. `-mutatemutant=<id>`, even
though the user's own framing used the shorter `-mutant`; worth a quick
naming call before implementation, not a blocker to the design). When set,
the engine still walks every file/node (cheap — it's just an AST walk) but
skips calling `run.run` for any mutation whose computed ID doesn't match,
so exactly one mutant executes. This is the mechanism a CI failure
message or a `//nomutant`-adjacent comment can point at: "mutant
`a1b2c3d4` survives — reproduce with `turango test
-mutatemutant=a1b2c3d4 -mutate=. ./pkg/...`" (`-mutate` still needs a
value — see `main.go`'s `-mutate` semantics, a regexp matched against
function names, not a package pattern — `-mutatemutant` narrows further
by exact mutant identity once mutation mode is already active; `./pkg/...`
supplies which package(s), as an ordinary trailing argument).

### Files/functions touched

- `internal/mutate/engine.go`: `mutateFile`'s `ast.Inspect` callback (ID
  computation), `Options` (new `MutantID string` field).
- `internal/mutate/runner.go`: `mutant` struct (new `id` field), `run.run`
  (early-return skip when `Options.MutantID` set and doesn't match).
- `internal/mutate/report.go`: `MutantResult` (new `ID` field).
- `cmd/turango/main.go`: new `-mutatemutant=<id>` flag, same parsing
  pattern as the other six.

### Build order

1. ID computation + `MutantResult.ID` (no replay filter yet) — land this
   alone first, verify IDs are stable across two identical runs and
   distinct for every mutant in a run (a collision would silently merge
   two different mutants' reports together, worth a dedicated test).
2. `-mutatemutant` replay filter, once IDs are proven stable.

### Verification

- Run the engine twice against an unchanged fixture, assert every
  `MutantResult.ID` matches its counterpart from the other run
  (position/order may differ under `-mutateparallel`, so match by
  File+Line+Operator+Description, not slice index, then compare IDs).
- Assert no two `MutantResult`s in a single run share an ID (a corpus
  fixture with 500+ mutants, like `stdlib-crypto-aes`, is a good stress
  case for this once the corpus harness above exists).
- `-mutatemutant=<id>` against a known ID produces exactly one
  `MutantResult` in the output, matching the same mutant a full run
  produces for that ID.

---

## 5. Dependency-closure workspace copy (higher priority than gap 6)

### Problem

`runner.go`'s `copyModule` copies the **entire Go module** (every file
under the directory holding `go.mod`) into a fresh temp workspace for
every single mutant, not just the target package's actual build/test
dependency closure. This was a deliberate phase-3 decision — a blind
recursive copy sidesteps having to correctly identify every file a
build genuinely needs (sibling package imports within the module, local
`replace` targets, `vendor/`, `go:embed` assets) — but it means workspace
cost scales with *module* size, not *target-package* size.

This stopped being a hypothetical concern the first time it was tested at
scale: turango's own `example`/`example-legacy` corpus entries mutate
`./example/...`/`./example/legacy/...` in place against turango's own
module (there's no separate fixture `go.mod` for them — the module
boundary genuinely is the whole turango repo). Adding 17 corpus fixture
subdirectories under that same repo root meant every one of `example`'s
219 mutants started copying all 17 irrelevant fixture directories too,
directly slowing down a run that had nothing to do with those fixtures.
Isolated single-package corpus fixtures (their own tiny `go.mod`) never
hit this, because for them "the whole module" already equals "the target
package" — but any real project mutating one package out of a larger
module has the identical exposure, just less visibly.

### Why this is a higher priority than gap 6 (git worktrees)

Both gaps reduce workspace-copy cost, but at different points:
dependency-closure copying reduces *what* gets copied (relevant to every
user, on every invocation, git or not); git worktrees change *how* a
given scope gets copied (only helps git users, and per gap 6's own
scoping, must stay strictly opt-in so turango doesn't pick up a git
dependency `-fuzz` deliberately doesn't have — its corpus lives in plain
`testdata/fuzz/...` files, no repository required). Dependency-closure
copying is the correctness-adjacent fix everyone benefits from by
default; worktrees are a further, narrower optimization on top for users
who happen to be in a git repo. Fix the universal problem before the
git-specific one.

### Design sketch (not built out to the same precision as gaps 1-4 yet)

Use `golang.org/x/tools/go/packages` — already a dependency, already used
in `engine.go`'s `load()` — to resolve the target package's actual import
graph within the module (`packages.Load` with `NeedImports`/`NeedDeps`,
scoped to the module, excluding stdlib/external-module dependencies which
don't need copying at all since they resolve from the module cache same
as any other build). Copy only: the target package's own directory, every
same-module package it imports (transitively), `go.mod`/`go.sum`, and —
same as today — `vendor/` if present and any local `replace` targets
(existing `localReplaces`/`rewriteReplaces` logic in `runner.go` still
applies, just scoped to the closure instead of the whole module).
`go:embed` assets need special handling: they're referenced by
`//go:embed` directive strings, not Go imports, so the dependency-closure
walk needs to also parse those directives (in the same files already
being read for the import graph) and include their referenced paths —
easy to miss if this is built by only reasoning about the import graph.

### Open questions — resolved

Both open questions turned out to be the same question, and it has a hard
answer, not a design preference:

**Dependency-closure copying is only correct under `ScopePackage` and
`ScopeImpact`. It is provably wrong under `ScopeFull`, and must never apply
there.**

The "dependency graph" the sketch above describes is the *forward* import
closure — every package the target package imports. That is exactly the
right set to *build and test the target package on its own*, which is all
`ScopePackage`/`ScopeImpact` ever ask `go test` to do. But `ScopeFull`'s
entire reason to exist (see its own doc comment: *"the only scope that
cannot miss a kill: a package's behaviour is frequently only asserted on by
its callers' tests, and those live in other packages"*) depends on the
*reverse* closure — every package that (transitively) imports the target —
which the forward closure says nothing about at all. A workspace built from
only the forward closure, tested under `ScopeFull`'s `go test ./...`, would
silently find and run only the target package's own tests (the only tests
physically present in that workspace), indistinguishable from
`ScopePackage` in outcome but still labeled `full` — a silent, dangerous
correctness regression on the *default* scope, not a performance tradeoff.

Computing the reverse closure instead (every package that could ever call
into the target) is not a viable alternative: for most real modules that
closure approaches the whole module anyway (anything more than a couple of
layers deep in a typical import graph), so it buys little over today's
whole-module copy while adding all of the same complexity the sketch above
already carries (embed directives, replace rewriting, vendor) *twice*, once
per direction.

**Resolution: this becomes a second strategy, gated by scope, not a
replacement for `copyModule`.** Under `ScopePackage`/`ScopeImpact`, use the
forward-closure copy the design sketch describes. Under `ScopeFull`,
`copyModule` keeps doing exactly what it does today — full module copy —
unconditionally, no closure computation attempted at all. This is a strictly
additive change to `runner.go`: a new function alongside `copyModule`, and
one branch in `run.run` (or `plan()`, if the closure is worth precomputing
once per package the way `buildImpact`/`planTCEBaseline` already are) keyed
on `m.scope`. No existing `ScopeFull` code path changes at all, which also
means this gap carries zero risk to the default scope by construction — a
bug in the new closure logic can only ever affect `ScopePackage`/
`ScopeImpact` runs, never the default.

### Independent review before activation (findings 1 and 2 fixed; now wired in)

**Verification status, updated 2026-08-10 (later the same day, post-reboot):
the fixes below were re-verified from scratch this session** — `go build
./...`, `go vet ./...`, `gofmt -l .` (clean except frozen `corpus/stdlib-*`
fixtures, expected), `go tool -modfile=tools/golangci-lint/go.mod
golangci-lint run` both with and without `--build-tags=integration` (0
issues each), `go test ./...`, and `go test -tags=integration
./internal/mutate/...` (which includes `TestRunWithImpactScope`, a real
`ScopeImpact` run against a real module) — all clean, not just trusted from
the prior session's self-report. **Activation itself landed the same
session**: `engine.loadClosures`/`engine.planClosure` resolve each
package's closure once (gated to non-`ScopeFull` runs, mirroring
`needsTypes`' zero-cost-by-default shape), threaded onto `fileJob`/`mutant`
as `closureDirs`, and `runner.workspaceFor` now tries `copyClosure` first
when non-nil, before its existing worktree/copyModule fallback chain — see
this document's summary list, item 5, and `runner.workspaceFor`'s own doc
comment for the full three-way precedence.

Per this section's own opening note, activating `runner.run`/`planPackage`
to actually call `copyClosure` was deliberately withheld from an unattended
session for review. That review happened (an independent pass over the
unwired code, not by the same session that wrote it) and found two real
correctness bugs — both in the "silently copy too little → spurious build
failure → a real mutant misclassified `NotViable`" category this section's
own risk framing worried about, and both invisible to the existing unit
tests because every test fixture hand-builds a `*packages.Package` whose
`Imports` already contains everything needed. A follow-up session fixed both,
plus one of the two minor findings, and added the regression coverage the
review called out as missing:

1. **Fixed.** `closureDirs`/`resolveClosure` only walked `pkg.Imports`,
   which is production-code-only unless the loader used `Tests: true` — and
   even then, a blackbox test file (`package foo_test`) is a *separate*
   `*packages.Package` whose imports were never merged into the one that
   code walked. `resolveClosure` is now variadic (`pkgs
   ...*packages.Package`) and merges every variant it's given — production,
   `"pkg [pkg.test]"`, and `"pkg_test [pkg.test]"` — into one synthetic root
   via the new `mergeVariants` before handing it to `closureDirs` (which
   itself is unchanged: it still only ever walks the single root it's
   given, and its doc comment now says so explicitly, pointing callers at
   `resolveClosure` instead). `mergeVariants` also pools `GoFiles`/
   `CompiledGoFiles` across variants, which incidentally fixes the "minor"
   `hasEmbedDirective` finding below too: a `//go:embed` directive in a
   black-box test file is now seen. **Residual risk, not eliminated**: this
   moves the correctness requirement from "impossible to get right" to "the
   future wiring code must actually load with `Tests: true` and pass every
   variant it gets back for the target directory into `resolveClosure`,
   not just the production one" — a caller contract now documented on
   `resolveClosure`'s doc comment, but not something the type system
   enforces. The new regression test
   (`TestResolveClosure/a_black-box_test_package's_own_import_is_included`)
   covers the external `"pkg_test"` shape specifically, since that's this
   project's own default and the shape the original review traced the bug
   through; it does not add a dedicated case for an *internal* test file's
   import (the `"pkg [pkg.test]"` variant, a whitebox `_test.go` in
   `package pkg` itself) — the merge mechanism treats every variant
   identically with no special-casing per shape, so there is no reason to
   expect it behaves differently, but that specific shape is asserted by
   inference from `TestMergeVariants`' generic union coverage, not by a
   dedicated `resolveClosure` case.
2. **Fixed.** `resolveClosure` reused `copyModule`'s `localReplaces()` as
   its only replace-directive bail-out check, but that function deliberately
   filters out replace targets whose destination resolves *inside*
   `moduleDir` (correct for `copyModule`, which copies the whole module so
   those paths always exist regardless). `localReplaces` now shares parsing
   with a new `parseReplaces`/`isInModule` pair, and a new
   `inModuleReplaceTargets` returns the half `localReplaces` excludes;
   `resolveClosure` checks each of those targets against the closure it just
   computed and falls back to `copyModule` if any target isn't already part
   of it. Covered by two new subtests: one where the in-module replace
   target is outside the closure (falls back) and one where it's already
   inside (resolves normally, proving the fix isn't just maximally
   conservative).

One minor finding fixed as part of the same change: `copyDirFiles` now
preserves symlinks the way `copyTree` already does (`os.Readlink`/
`os.Symlink` instead of transparently following the link and copying its
target's content as a plain file), covered by an extended `TestCopyDirFiles`.
The other minor finding (`hasEmbedDirective` scanning test files) was fixed
as a side effect of finding 1's fix, described above.

**Activation landed clean**: `engine.loadClosures` does load with `Tests:
true` and hands every variant it gets back for a directory into
`resolveClosure` via `engine.planClosure` — the caller obligation finding
1's fix could only document, not enforce, is satisfied by the actual
wiring code. Residual risk carried forward, not new: finding 1's "internal
test-file variant" gap in dedicated test coverage (see above — only the
external `pkg_test` shape has its own `TestResolveClosure` case) is
unchanged by activation and still worth closing if this area is touched
again.

---

## 6. Git-worktree-based execution (optional, opt-in only)

**Status: done.** `-mutateworkspace=worktree` (`Options.Workspace`,
`mutate.WorkspaceWorktree`), strictly opt-in as this section always
required — the zero value (`Options{}`, and every existing caller before
this change) is `WorkspaceCopy`, today's filesystem-copy behavior,
completely unchanged. `internal/mutate/runner.go`'s `copyWorktree` builds
each mutant's workspace with `git worktree add --detach` instead of a
recursive copy when requested, falling back to `copyModule` automatically
— not as an error — whenever the target isn't inside a *clean* git working
tree (`gitWorktreeClean`: the whole repository, not just the mutated
module, since `git worktree add` checks out HEAD, the last commit, never
the on-disk working copy, so any uncommitted edit anywhere in the repo
would otherwise be silently dropped from every mutant's workspace) or
isn't inside a git repository at all (`gitRepoRoot`, ok=false for any
plain directory — every `corpus/*/module/` fixture, deliberately not a git
repo, keeps working exactly as before).

One real bug found and fixed during implementation, not anticipated by
this section's original design sketch: computing a worktree's
module-relative path with `filepath.Rel(repoRoot, moduleDir)` is unsafe on
macOS, where `t.TempDir()`-style paths under `/var/...` are themselves a
symlink to `/private/var/...` that `git rev-parse --show-toplevel` always
resolves through, but `moduleDir` as handed to this code is not guaranteed
to be pre-resolved the same way — the lexical mismatch produced a wrong
relative path that (via the arithmetic of `filepath.Join`'s `..`
collapsing) could land back on the *original* module's files instead of
the new worktree's copy, silently defeating the whole point without
erroring. Fixed by asking git itself for the prefix (`git rev-parse
--show-prefix`, run with the same `-C moduleDir` as the toplevel lookup,
so both resolve consistently) instead of computing it with `filepath.Rel`.
Caught by `TestCopyWorktree`/`TestWorkspaceForUsesWorktreeWhenRequested`
actually asserting on the *content* of the checked-out `go.mod` and on
`cleanup()` really removing the worktree directory, not just on `ok==true`
— a weaker assertion would have passed against the bug.

Cleanup uses `git worktree remove --force`, not a plain filesystem delete:
a worktree is registered in the parent repository's `.git/worktrees/`
administrative area, and this runs once per mutant, so leaving that entry
dangling (as a bare `os.RemoveAll` on the checkout directory would) is a
real accumulating leak across a run with any real mutant count, not a
one-off. `TestRunWorkspaceWorktreeMatchesCopy` (integration-tagged) is the
end-to-end proof this section's own scope note implies is needed: the same
fixture, git-committed, mutated once under each workspace mode, must
classify every mutant identically — a worktree is a different mechanism
for the same execution copy, not a different verdict rule.

Per the scope note below, this only ever addresses workspace-*setup* cost;
it does nothing for the actual per-mutant `go build`/`go test` time, which
still dominates a real run.

### Problem

`runner.go`'s `copyModule` does a full recursive filesystem copy of the
target module per mutant (plus hand-rolled `replace`-directive rewriting —
`localReplaces`/`rewriteReplaces`/`placeRoots`/`commonDir`) to build each
mutant's isolated workspace. Filippo Valsorda's `muzoo`
(github.com/FiloSottile/mostly-harmless/tree/main/muzoo — a curated,
patch-based mutation tool, philosophically different from turango's fully
automated AST-operator generation, but relevant here for its *execution*
strategy) uses `git worktree add` per mutation instead: worktrees share the
object store rather than copying files, and — since a worktree is the same
repo checked out twice — sibling `replace`-target modules already resolve
via their real relative paths with no rewriting needed at all.

### Why this is optional, not a default

`go test -fuzz` has **zero git dependency** — its seed corpus lives in
plain `testdata/fuzz/FuzzXxx/` files and the engine's own generated corpus
goes in `$GOCACHE/fuzz`, both ordinary filesystem, no repository required.
turango matches that today: it works against any directory with a
`go.mod`, git or not (every corpus fixture under `corpus/*/module/` is a
plain directory, not a git repo, by design). A worktree-based execution
path must stay strictly opt-in — e.g. `-mutateworkspace=worktree`,
falling back to the existing copy strategy whenever the target isn't
inside a clean-enough git working tree — never a replacement for the
copy-based default, or turango would silently pick up a git dependency
`-fuzz` deliberately doesn't have.

### Scope note

Only ever addresses the workspace-setup cost (the copy + replace-rewrite
step), not the actual bottleneck observed while building the corpus
(section above) — per-mutant `go build`/`go test` compile-and-run time,
which a worktree does nothing for. Not designed in detail here; parked as
a named idea for whoever picks up the corpus/CI cost problem next, not
committed to a specific flag shape or build order yet.

---

## 7. Channel-based collector

**Status: done.** `collector` now sends `MutantResult`/`SuppressionResult`/
`EquivalentResult` on three unbuffered channels to a single consumer
goroutine started by `newCollector`, rather than mutex-guarding a shared
slice; `close()` closes all three and blocks for the consumer's final,
sorted `Result`. Verified under `-race` (unit tests and the real
multi-worker end-to-end tests), and no other package referenced the old
`collector{result: ...}`/`.sorted()` shape, so this was a self-contained
`internal/mutate/engine.go` change plus updates to the whitebox tests that
constructed a `collector` directly.

### Problem

`internal/mutate/engine.go`'s `collector` serialises every file worker's
results into one `*Result` via a `sync.Mutex` guarding two slice appends
(`collector.mutant`, `collector.suppression`). This is correct and
race-safe — `-race` passes clean, and the critical section is a single
O(1) append, so there is no performance case for changing it. The reason
to change it is testability, not correctness: `testing/synctest`'s bubble
only tracks a specific, documented list of "durably blocking" operations —
`go doc testing/synctest` names exactly five: a blocking channel
send/receive on a bubble-created channel, a `select` where every case is
such a channel, `sync.Cond.Wait`, `sync.WaitGroup.Wait` (when `Add` was
called inside the bubble), and `time.Sleep`. `sync.Mutex.Lock` is not on
that list. A goroutine blocked acquiring `collector.mu` is therefore
invisible to a synctest bubble: it cannot let the bubble's fake clock
advance, and if it were ever the sole reason a bubble isn't "all
durably blocked," `synctest.Wait()` would hang rather than correctly
model the wait.

### Design decision

Replace the mutex-guarded `collector` with a channel-fed single-consumer
design: each file worker sends `MutantResult`/`SuppressionResult` values on
a channel instead of calling `collector.mutant`/`collector.suppression`
directly; one consumer goroutine (started by `Run`, alongside the
`errgroup` worker pool) drains the channel and appends to the `Result`,
closing over the same sort-at-the-end logic `collector.sorted` has today.
This makes the whole pipeline synctest-visible: a test could drive the
worker pool with `synctest.Test`, control scheduling deterministically, and
assert on ordering/timing behavior (e.g. `-mutateparallel`'s bounded
concurrency) without real wall-clock waits or flaky timing assertions.

### Why this is low priority

Nothing in the collector is timing-dependent today — there is no sleep, no
fake clock, no scheduling behavior worth a synctest test yet. This is a
door to leave open for whenever that changes (e.g. if `-mutateparallel`'s
bounded-concurrency behavior ever needs a deterministic test), not a bug
being fixed. Race-safety, the property that matters today, is already
verified by `-race`, which channels would not improve on.

### Files/functions touched

- `internal/mutate/engine.go`: `collector` (replaced by a channel + single
  consumer goroutine), `Run` (starts the consumer, closes the channel after
  `execute`'s `errgroup.Wait()` returns), `execute`/`mutateFile`/`visitNode`
  (send on the channel instead of calling `sink.mutant`/`sink.suppression`).

---

## 8. Benchmark mutation-testing overhead

**Sequencing constraint, set explicitly by the user 2026-08-10: build the
harness whenever, but do not actually *execute* a real timing run until
it's the last thing done before proposal submission — plugged in, not on
battery.** Two reasons stated: timing numbers captured on battery power are
untrustworthy on their own (CPU frequency/power-state throttling skews
wall-clock measurements in a way that has nothing to do with the code being
measured), and more generally, benchmarking should happen once, right
before the numbers go into `PROPOSAL.md`/`BENCHMARKS.md` for real — not
repeatedly, mid-development, every time the harness changes. This was
learned the concrete way: a subagent building this harness was caught
mid-run actually executing `go test -bench` on battery and had to be
killed and redirected. `BenchmarkMutate` itself should be built, verified
to *compile* and pass lint (`go build`/`go vet`/`golangci-lint`), and
reasoned about for correctness — but not run for real numbers — until
someone explicitly says it's time.

### Problem

`PROPOSAL.md` argues turango is worth upstreaming, but its "Costs and
risks" section quantifies almost nothing about the actual cost side of
that argument. The only timing data points anywhere in this repo are
incidental and non-comparable: PROGRESS.md's "known limitation 1" notes
turango's own `ScopeFull` dogfooding run took "17+ minutes" (and explains
why — nested `go test ./...` re-runs, not a representative number for a
normal target), and scattered mentions of "minutes" for `stdlib-crypto-aes`
sweeps that were never even successfully captured (see PROGRESS.md's open
corpus-provenance thread). Nobody has run a controlled comparison and
written down "for a package this size, a full mutation run costs
approximately this many multiples of one `go test` run." Without that, the
stdlib pitch is asserting a qualitative benefit (catches real bugs — the
phase-7 case studies) while leaving the quantitative cost an open question
a reviewer will ask about immediately.

### Design decisions

**8a. Metric shape.** Report, per target package:

- production KLOC and test KLOC (`gocloc`-or-equivalent line count of
  non-`_test.go` vs. `_test.go` files — a `go build`/`go vet`-clean count,
  not a hand estimate),
- mutant count (`Result`'s own `Counts()`),
- one baseline `go test` wall-clock time (`goTestSuite`'s existing 3-run
  average is the right number to reuse here — see 8b), and
- total mutation-run wall-clock time **at each of several
  `-mutateparallel` settings** (see below) — each itself averaged over
  multiple runs, not a single sample, for the same reason `goTestSuite`
  already averages 3 baseline runs rather than trusting one (a cold build
  cache, background system load, or GC pause can dominate a single
  sample). **At least 3 runs per (target, scope, TCE, parallelism) cell.**
  This is what `go test -bench`'s own `-count=N` flag already gives for
  free — 8b's design already calls for `-count=6`+ per `benchstat`'s own
  recommendation, so there is no need for a second, hand-rolled averaging
  loop inside `BenchmarkMutate` itself: run the whole matrix with
  `-count=3` (minimum) or `-count=6` (preferred, matching 8b), and average
  (or let `benchstat` average) the repeated `ns/op`/custom-metric lines
  `go test -bench`'s own output already produces per subbenchmark name.

**Parallelism is swept, not fixed at one setting** — this is a deliberate
correction to an earlier draft of this section, which treated a single
serial-vs-parallel comparison as a nice-to-have, not the point. It is the
point: `-mutateparallel` is the one lever most directly under a CI author's
control (unlike scope, which trades away correctness confidence, or TCE,
which trades away a small chance of a false equivalence), so its actual
payoff curve — does doubling parallelism roughly halve wall-clock, or flatten
out well before `NumCPU` because of I/O or `go test`'s own build-cache
contention across concurrent workers — is exactly the number worth
publishing. Sweep at minimum `{1, 4, 8}` (serial, a common small-CI-runner
core count, a common larger one); scale the upper end to the actual
benchmarking machine's `NumCPU` if it's smaller than 8, rather than
requesting more workers than cores.

**Final report shape**: one table per (target `x` scope `x` TCE)
combination, columns = baseline plus one column per swept parallelism
level, e.g.:

| Package | Baseline test time | Mutate @ parallel=1 | Mutate @ parallel=4 | Mutate @ parallel=8 |
|---|---|---|---|---|
| `internal/mutate` | 1.2s | 4m30s | 1m20s | 52s |

with the **mutation multiplier** (total / baseline) worth deriving as a
secondary column or a follow-on note per row, not a replacement for the raw
times — a reader evaluating "is this affordable in my CI" wants the actual
wall-clock at the parallelism level they'd realistically run, not just a
dimensionless ratio.

Report this table once per **scope mode** (`full`/`package`/`impact`) and
once with **TCE on vs. off**, since those are exactly the other two levers
a user (or the stdlib pitch) would reach for to control cost, and neither
has published numbers today either.

**8b. Use `go test -bench`, Go's own built-in benchmark suite — not a
shell script.** This is idiomatic for this codebase specifically: turango
*is* a `go test` extension, so measuring it with the standard
`testing.B`/`go test -bench=...`/`benchstat` pipeline is the same "eat our
own dog food" instinct that already produced the corpus regression harness
(`TestCorpus`) rather than a bespoke checker. Concretely, a `BenchmarkMutate`
function with one `b.Run(name, func(b *testing.B) {...})` subtest per
`target x scope x TCE` combination, each calling `mutate.Run` in-process
(not shelling out to a separately-built `turango` binary — this repo
already links `internal/mutate` directly, so an in-process call is both
faster to iterate on and avoids a stale-binary trap a shell script would
risk).

Because one iteration of `mutate.Run` against a real package is itself a
full mutation sweep — seconds to minutes, not the microseconds `testing.B`
is built to auto-calibrate `b.N` around — this must run with
**`-benchtime=1x`** (a real, documented `go test` flag for exactly this
"one call is already expensive" case, e.g. how the Go toolchain's own
compiler benchmarks are run) rather than letting `testing.B` loop to fill a
time budget. Statistical stability then comes from the standard
`go test -bench=BenchmarkMutate -benchtime=1x -count=N` external-repeat
convention (`benchstat`'s own docs recommend `-count=6` or more) feeding
into `golang.org/x/perf/cmd/benchstat` for a proper before/after comparison
(e.g. TCE on vs. off, or this gap's own future re-runs after an engine
change) — a tool this project doesn't currently depend on, but whose whole
job is exactly this comparison and is the standard companion to `go test
-bench`, not a bespoke stats script.

Per-target metadata (KLOC, test KLOC, mutant count, the mutation
multiplier itself) is reported via `b.ReportMetric` as custom units
alongside the built-in `ns/op` timing, so a single `go test -bench` run's
output already carries everything `BENCHMARKS.md` needs — nothing to
cross-reference from a separate JSON file by hand. `goTestSuite`'s existing
3-run baseline average (the same number `-mutatetimeout`'s default already
depends on) is still the right source for the "one `go test` run" baseline
half of the multiplier — reused here, not recomputed by a second
mechanism, whether that means reading it back off `Result` (adding a field
if `report.go` doesn't already expose it) or, more simply, giving the
benchmark its own `b.Run("baseline/"+target, ...)` sibling subtest that
times a plain `go test` invocation of the same target the same way.

**8c. Target selection — real code, a real range of sizes, and this is a
human call, not an automated one.** The existing `corpus/` fixtures are the
wrong instrument for this specific question: the 13 `op-*/` fixtures are
deliberately tiny single-function files (systematic operator coverage, not
size variety), and the `stdlib-*` fixtures are frozen *partial* files (a
handful of functions each, not a whole real package with its whole real
test suite) — see PROGRESS.md's corpus-provenance thread for exactly how
narrow `stdlib-x509-pkix`/`stdlib-strconv-parseuint` are. Meaningful KLOC-vs-time
numbers need a handful of **real, complete Go packages** spanning roughly
small (~500 LOC), medium (~5K LOC), and large (~20K+ LOC) production code
with their real test suites — `example/` and `example-legacy/` are useful
as a sanity check (turango already dogfoods them) but are far too small to
anchor a range on their own. **Picking the actual target packages is
flagged as an open, human decision, not resolved here** — candidates should
be permissively licensed, fast/non-flaky under `go test` (a flaky suite
would corrupt the "3-run average" baseline the whole ratio depends on), and
ideally already familiar from this project's own validation work (e.g. a
complete stdlib package, not a hand-frozen excerpt of one, would also let
this benchmark reuse groundwork from the phase-7 case studies).

These targets should **not** live under `corpus/` — that directory's
`Discover()`/`TestCorpus` pairing has one job, asserting exact mutant
counts against a golden file, and a target chosen for benchmark realism
(a real, complete package, not a tiny deterministic fixture) is a poor fit
for that exact-count assertion. Recommend a sibling top-level
`benchmark/targets/` directory instead, kept deliberately separate in
purpose from `corpus/` even though the two may end up sharing a
provenance mechanism — see below.

How the target packages actually get into the repo is the same open
question PROGRESS.md's corpus-provenance thread already raised and left
unresolved (git submodules pinned to an exact upstream commit, for
verifiable provenance, vs. a frozen local copy): don't silently duplicate
that decision here. If submodules end up adopted for `corpus/`'s
stdlib-sourced fixtures, `benchmark/targets/` choosing full, complete
packages (rather than `corpus/`'s hand-trimmed excerpts) makes it if
anything an *easier* submodule case, not a harder one — pin a commit, use
the whole directory, no manual trimming to keep in sync.

**8d. Where results are captured.** `go test -bench`'s own output — plus
`benchstat`'s comparison tables when relevant (8b) — is already the
standard, reproducible artifact; the discipline this repo should still add
is committing a captured run rather than only describing the command,
following the same precedent `example/README.md` already sets ("real
re-run, not hand-edited", called out explicitly in PROGRESS.md when the
operator count changed and that doc's numbers were regenerated). Recommend
a new `BENCHMARKS.md` at repo root (parallel to `PROPOSAL.md`,
`ROADMAP.md`) that is substantially a pasted `go test -bench=BenchmarkMutate
-benchtime=1x -count=N` transcript, plus a short methodology section
(machine spec, Go version, `-mutateparallel` value, date, the exact command
run — the same "how would someone re-run this and get comparable numbers"
transparency `PROPOSAL.md`'s 2026 stdlib re-validation section already
models), and a pointer from `PROPOSAL.md`'s "Costs and risks" section into
the real numbers instead of leaving that section qualitative.

**8e. Honest scope limit.** This gap produces a snapshot on one machine at
one point in time, not a durable regression-tracked benchmark suite (that
would be a much larger undertaking — a `benchstat`-style tracked history —
and isn't what's being asked for here). If the numbers turn out to vary a
lot by package shape (e.g. a package with many small functions vs. one with
a few large ones, at the same KLOC) rather than fitting a clean per-KLOC
rate, the honest output is a table showing that variance, not a forced
single formula — this mirrors gap 1's "don't oversell a heuristic" instinct
and gap 2's "state what's genuinely unresolved" instinct elsewhere in this
document.

### Files/functions touched

- New `benchmark/targets/` at repo root (see 8c) — real, complete target
  packages, provenance mechanism TBD alongside `corpus/`'s open thread.
- New `internal/mutate/mutate_bench_test.go` (whitebox naming per this
  project's own test-convention rules only if it needs unexported access;
  otherwise a blackbox `internal/mutate_test` bench file, same convention
  every other test in this package already follows) — `BenchmarkMutate`,
  `b.Run` subtests per target/scope/TCE combination, `b.ReportMetric` calls
  for KLOC/test-KLOC/mutant-count/multiplier. Gated behind the same
  `//go:build integration` tag the existing real-toolchain tests use (this
  shells out to/links against the real `go test` toolchain against real
  package sources, exactly the reason that tag exists today) — or a new,
  even narrower tag if these targets are too heavy to run as part of the
  existing `make test-integration` scope; flagged as an open call, not
  decided here.
- New `BENCHMARKS.md` at repo root (see 8d).
- `internal/mutate/report.go` — only if the baseline `go test` time
  `goTestSuite` computes (8b) isn't already surfaced on `Result`; check
  before adding a field.
- `PROPOSAL.md`'s "Costs and risks" section — updated to cite real numbers
  instead of the current qualitative statement.
- `go.mod` — `golang.org/x/perf` is the only new dependency this gap needs
  (for `benchstat`), and only as a `go tool`-style tooling dependency
  (mirroring `tools/golangci-lint/go.mod`'s separate-module pattern) if the
  "stdlib + `golang.org/x/*` only" project policy (PROGRESS.md's dependency
  cleanup note) is read as still wanting it isolated from the main module's
  own `go.mod` — `golang.org/x/perf` is itself an `x/` package either way,
  so it's in-policy regardless of which `go.mod` it lands in.

### Build order

1. Confirm whether `goTestSuite`'s baseline average is already exposed
   anywhere in `Result`/`MutantResult`; add a field only if it's genuinely
   missing (8b).
2. Pick the target package set and provenance mechanism (8c) — human
   decision, not automated; resolve alongside (or explicitly after,
   without blocking on) `corpus/`'s own open submodule thread rather than
   inventing a second, inconsistent answer.
3. Write `BenchmarkMutate`; sanity-run one target at `-benchtime=1x
   -count=1` by hand and confirm `b.ReportMetric`'s custom metrics actually
   appear in `go test -bench` output the way expected — verify against the
   real toolchain output, not assumed from `testing` package docs alone,
   matching this project's habit of confirming CLI/flag behavior
   empirically before relying on it (see PROGRESS.md's `-mutate`
   redesign, which caught a real `go help testflag` mismatch the same way).
4. Run the full matrix: `go test -tags=integration -bench=BenchmarkMutate
   -benchtime=1x -count=6 ./internal/mutate/...` — the matrix already
   includes the `{1, 4, 8}` (or machine-scaled) `-mutateparallel` sweep as
   part of `BenchmarkMutate`'s own subbenchmark table per 8a, so this one
   command produces every row of the final table — piped through
   `benchstat` for the TCE-on-vs-off and scope-mode comparisons.
5. Write `BENCHMARKS.md` (captured transcript + methodology), update
   `PROPOSAL.md`.

### Verification

- `BENCHMARKS.md`'s numbers must be reproducible by re-running the exact
  committed `go test -bench` command against the committed (or clearly
  cited, if external) target packages — not hand-typed into the doc, the
  same standard `example/README.md` already holds itself to.
- Sanity check: `impact` scope's total time must never exceed `package`
  scope's, and `package` must never exceed `full`'s, for the same target —
  a violation means something in the run was misconfigured (mirrors the
  "full-scope must never report fewer mutants than package-scope" sanity
  check already called out in PROGRESS.md's aes/base64 open thread).
- `go vet`/`golangci-lint` must stay clean with the new bench file
  in the tree even when the `integration` tag isn't passed — the same bar
  every other integration-tagged file in this repo already clears.

---

## 9. Comment cleanup pass

**Status: done, all five sub-tasks.** See PROGRESS.md's 2026-08-12 entries
for the full fix list, including two real findings beyond pure comment
cleanup: README.md/PROPOSAL.md still described identifier-swap v2 as
unbuilt after it had landed, and `example/README.md`'s captured
transcript pre-dated both mutant IDs and the two identifier operators —
both corrected with real, re-run numbers, not just prose edits. 9b
(verbosity trim) got its explicit go-ahead, scoped to trimming only
comments that don't cite a rejected alternative or a concurrency/
correctness hazard — 5 comments qualified, out of every 4+-line comment
in the codebase read for this pass; the rest genuinely held up the
rationale-heavy convention this section originally defended.

### Problem

This codebase's comments have never had a dedicated pass — they accumulated
build-order-by-build-order, across nine build phases plus every gap in this
document, each session adding its own without auditing what earlier
sessions already wrote. Five genuinely different problems have been folded
into "clean up comments" so far, and they need separate treatment because
they trade off against each other, not because any one of them is hard:

**9a. Stale/inaccurate comments — a correctness pass, not a style
change.** A comment that asserts something about the code that is no
longer true is worse than no comment: it actively misleads the next
reader. This is not hypothetical — this very session found and fixed a
live example while wiring gap 5: `runner.go`'s `closureDirs` doc comment
said, in bold, "NOT WIRED INTO THE ENGINE YET," which became false the
moment gap 5's activation (this same session, above) landed. Two known
sources of this pattern going forward:
  - **Line-number references.** Several ROADMAP.md sections (gaps 1, 3, 5)
    cite exact line numbers ("engine.go lines 175–207," "runner.go lines
    96–178") that were accurate *at the time each section was written* —
    gap 1's own preamble says so explicitly ("current as of this writing
    ... re-check line numbers before implementing if the tree has moved
    on"). Every gap landed since has shifted line numbers in both files.
    These references should either be re-verified and corrected, or
    deliberately converted to function-name-only references (`[runner.run]`
    style godoc links, which this document already uses elsewhere and which
    don't rot the same way).
  - **"Not wired in" / "not started" / "TODO" style status markers left in
    code comments** (as opposed to this document, where that framing is
    the whole point) — any surviving in `internal/mutate/*.go` after gap 5's
    wiring should be grepped for and re-checked, not just the one this
    session happened to notice.

**9b. Verbosity — a real style question, not a mechanical trim, and
flagged as needing a human decision.** This project's comments are
*deliberately* long and rationale-heavy by established, repeatedly
reaffirmed convention — nearly every doc comment in `internal/mutate/` and
every operator package leads with *why*, often citing the specific
alternative that was rejected and why (e.g. `runner.go`'s `workspaceFor`,
`resolveClosure`, every `planXxx` function in `engine.go`). This is not
incidental sprawl; PROGRESS.md and this document's own writing style is the
same way, on purpose, and CLAUDE.md-adjacent project convention has never
pushed back on it — the opposite: findings sessions have repeatedly added
*more* of this kind of comment as the right way to document a non-obvious
design decision. "Clean up" read as "make comments shorter" would be a
genuine reversal of that convention, not a cleanup, and shouldn't happen
silently as a side effect of a pass framed as maintenance. If verbosity
trimming is wanted, scope it explicitly and separately (e.g. "shorten
comments over N lines that don't cite a rejected alternative or a
concurrency/correctness hazard") rather than folding it into 9a's
correctness pass, where the two goals would fight each other line by line.

**9c. Dead code and stray markers.** Commented-out code blocks and
`TODO`/`FIXME`/`XXX` markers are a different, genuinely mechanical class —
find them (`grep -rn 'TODO\|FIXME\|XXX'`, plus a manual scan for
commented-out statements, since those don't grep for a fixed token) and
resolve each one (do the thing, file it as a ROADMAP item if it's real
future work, or delete it if it's stale) rather than leaving it to rot
silently. Low risk, unlike 9a/9b: nothing here is load-bearing prose,
so a wrong call here just deletes or keeps a marker, not a claim about the
code's behavior.

**9d. Format/consistency.** Doc-comment format varies in minor ways across
the codebase — this document's own `[Xxx]` godoc cross-reference style is
used inconsistently between older gaps (1–4) and newer ones (5–7), and
some files favor a single long paragraph per comment where others break
into the "topic sentence, then rationale" shape most of `engine.go` and
`runner.go` use today. Worth a pass, but genuinely cosmetic — lowest
priority of the five, and the one most amenable to an automated or
semi-automated sweep (`gofmt`/`golangci-lint` don't check comment prose
style, so this stays a manual or LLM-assisted read-through, not a tool
this project can add to CI the way `gofmt -l` already is).

**9e. Missing or informal citations for named techniques — a real gap,
not paranoia.** `PROPOSAL.md` sets the right bar in one place already: its
"[Mull]" reference (`internal/mutate`'s stated prior-art comparison, cited
in `PROPOSAL.md`) links directly to `https://arxiv.org/pdf/1908.01540`, a
real, checkable paper. `ROADMAP.md` gap 2 does not hold itself to the same
bar: TCE is introduced as `"Kintis/Papadakis/Malevris et al., 'Trivial
Compiler Equivalence'"` — a title in quotes and three surnames, with no
link, venue, or year, unlike the `[go.dev/blog/rebuild]`/`[filippo-repro]`
citations two paragraphs below it in the very same section, which *are*
properly linked. For a document whose explicit goal is an eventual stdlib
proposal (`PROPOSAL.md`'s whole purpose), an unverifiable academic
name-drop sitting next to properly-cited blog posts is a credibility gap a
reviewer would notice immediately. Audit every named technique or paper
reference across `PROPOSAL.md`/`ROADMAP.md`/code comments (TCE is the one
confirmed instance; mutant subsumption is mentioned in gap 2's own text
without citation either) and bring each one up to the `[Mull]`-link
standard — a real DOI/arxiv/ACM/IEEE link, not just a re-statement of the
technique's common name.

### Why this is nine sub-decisions, not one task

9a and 9c are safe to do mechanically and don't require asking anyone
anything first. 9d is cosmetic and low-stakes either way. 9b is the one
that actually needs a human answer before any comment gets shortened — the
project's own established practice argues against it by default, so
proceeding without an explicit "yes, actually trim" would be silently
overriding a convention this project has reaffirmed repeatedly, not
"cleaning up." 9e is scoped narrowly (citations only, not prose) and is
safe to do without asking, provided each replacement citation is a real,
checked link — inventing a plausible-looking arxiv ID would be strictly
worse than the informal name-drop it replaced.

### Build order

1. 9a (staleness audit) and 9c (dead code/markers) first — mechanical,
   safe, no style judgment call needed. Grep-driven; a fresh read-through
   of every doc comment against the current code it describes is the only
   way to catch 9a's line-number and status-marker rot, since neither greps
   for a fixed pattern.
2. 9e (citations) next — also safe to do without further sign-off, scoped
   to references only.
3. 9d (format consistency) — cosmetic, do last, since it's the easiest to
   get subsumed by whatever 9b decides (a verbosity trim would itself
   change format in the same files).
4. 9b (verbosity) — **do not start without an explicit go-ahead**, separate
   from the go-ahead for this gap as a whole; this document's own framing
   above is the reason why.

### Verification

- `grep -rn 'TODO\|FIXME\|XXX'` across the repo (excluding frozen
  `corpus/stdlib-*` fixture source, which is historical stdlib code and not
  this project's to annotate) returns nothing unresolved.
- Every `[Xxx]`-style godoc cross-reference actually resolves to a real,
  currently-named symbol (`go doc` or `golangci-lint`'s own doc-link check,
  if one is enabled, would catch a reference broken by a since-renamed
  function).
- Every citation added under 9e is a real, dereferenceable link, spot-checked
  by actually opening it — the same standard this section holds the
  *existing* `[Mull]` citation to.

---

## 10. `literal/number` doesn't mutate float literals despite being documented to

**Done.** Found during the gap-1 v2 corpus investigation (2026-08-11, via
an independent fable-model review asked to sanity-check a suspiciously
high mutant count) — an incidental finding, not what that review was
actually looking for. Built the same night via a parallel worktree
subagent. `Applies`/`Mutate` now match `token.FLOAT` too, staying in
`literal/number` rather than a new sibling (int and float share the same
node shape and `Apply`/`Revert` mechanics — only the shift math differs).
Two of this section's own open questions were resolved with real,
empirically-confirmed answers that overturned an assumption in the
original design sketch below: a flat `±1` (or even a flat absolute
epsilon) either produces a trivially-caught mutant or, at large
magnitudes, a silent no-op once rendered — the fix uses a relative 0.1%
nudge instead, and `constant.Value.ExactString()` (the design sketch's
assumed rendering path) turns out to render a non-integer float as an
invalid-Go-syntax exact fraction, not a usable literal — `String()`'s
~6-significant-digit approximate rendering is used instead. See
`internal/mutator/literal/number.go`'s `shiftFloat` doc comment for the
full, evidence-based writeup.

### Problem

`internal/mutator/literal/number.go`'s `Applies` (line 35) matches only
`*ast.BasicLit` nodes with `Kind == token.INT`; `Mutate` (line 48) has the
same guard. Float literals (`token.FLOAT`) are never offered a mutation —
confirmed by reading the code directly, not inferred. But `PROGRESS.md`
("Extra: two new mutation operators + a literal package") and `README.md`
both describe this operator as shifting "an int **or float** literal by
±1." The operator's own doc comment on `NumberMutator` (in `number.go`
itself) is honest — it says "shifts an integer literal," no float claim —
so the mismatch is in the *surrounding* docs overselling what the code
actually does, not the code lying about itself.

Practical effect: a real bug in a float boundary constant (e.g. a
threshold like `const maxRatio = 0.95`) gets zero coverage from this
operator today, silently — nothing errors, nothing warns, the mutant
simply never gets generated. Lower severity than an overcounting bug would
have been (fewer mutants than claimed, not a wrong/misleading count), but
still a real gap between documentation and behavior.

### Design sketch (not built out to gap 1/2/5's precision — small, bounded fix)

Extend `Applies`/`Mutate` to also match `token.FLOAT`, and extend
`shiftBy` to handle a float `constant.Value` (`val.Kind() == constant.Float`)
alongside the existing `constant.Int` path. Open questions worth resolving
before implementing, not resolved here:

- **What does "shift by 1" mean for a float?** `constant.BinaryOp(val,
  token.ADD, constant.MakeInt64(1))` works generically across numeric
  `constant.Value` kinds per `go/constant`'s own API (it's not
  int-specific), so the *mechanism* likely just works unchanged — but
  "shift `0.95` to `1.95`" is a much less interesting mutant than "shift
  `0.95` to `0.949999...` or `0.950001...`" (a boundary-adjacent nudge,
  closer to what actually catches a float comparison bug in practice).
  Worth deciding whether float literals want a *different* shift magnitude
  (e.g. a small epsilon, or a percentage-of-value nudge) rather than
  reusing the integer operator's literal "±1," which could produce a
  float mutant so far from the original value that it's trivially caught
  by almost any test — low signal, same "avoid uninteresting mutants"
  instinct `operator/boundary` and `literal/number` already follow for
  integers.
- **Rendering.** `shifted.ExactString()` already handles float rendering
  via `go/constant`'s own formatting — verify it produces valid, minimal Go
  float syntax (not e.g. an over-precise decimal expansion) before trusting
  it unchanged for the float path.
- **Same operator or a new one?** Given the shift-magnitude question above
  might call for genuinely different logic (not just "run the same
  arithmetic on a different `constant.Kind`"), consider whether this
  belongs as `literal/number`'s extension or a new sibling operator (e.g.
  `literal/float`) — mirroring how `literal/number`/`literal/boolean` are
  already split by literal kind rather than one operator switching on
  `token.Kind` internally.

### Files/functions touched

- `internal/mutator/literal/number.go` — `Applies`, `Mutate`, `shiftBy` (or
  a new sibling file, per the open question above).
- `PROGRESS.md`/`README.md` — no change needed if the fix ships (docs
  become accurate); if the fix is *not* built, correct the "int or float"
  claim down to "int only" instead, so the docs stay honest either way.

### Verification

- Table-driven test (same pattern `number_test.go` already uses for the
  integer case) covering a float literal in each Go float syntax form
  (decimal, exponent notation) offering the expected two mutations.
- A corpus fixture or existing example package with a real float constant
  used in a boundary comparison, confirming the new mutant is actually
  generated end-to-end through the engine, not just at the unit level.

---

## 11. `-mutateestimate`: predict mutant count and run time before committing to a real run

**Done.** Built the same night via a parallel worktree subagent as gap 10.
`mutate.Estimate(ctx, opts) (*EstimateResult, error)` — a separate entry
point, not an `Options` flag (11a), reusing `load`/`plan`/`planPackage`/
`mutateFile`/`visitNode` exactly, with one new branch in `visitNode`'s
per-mutation loop (gated on a non-nil tally) that records a package hit
instead of calling `runner.run`. Per-package baseline timing (11b) runs
after the count-only walk, only for packages the walk actually found a
mutant in — deliberately, not precomputed alongside `ScopeImpact`'s
coverage map the way the ROADMAP's own "files touched" section originally
implied, since timing a package before knowing it has any mutants would
waste `go test` invocations; the design section's own prose (not that
list) already called for this. Extrapolation reports both a serial and a
`-mutateparallel`-divided number, both explicitly hedged (11c). TCE is
ignored for v1 as planned (11d). Flag name: `-mutateestimate=true` (11e),
combined with `-mutateoutput`/`-mutatemin` as a hard parse-time error
rather than a silent no-op. The zero-subprocess claim for the counting
phase is proven by a real test (`TestWalkForEstimateSpawnsNoSubprocess`),
not just asserted — an `execCalls` atomic counter (mirroring
`loadTypedCalls`'s existing precedent) confirms zero `go test`/`go build`
subprocesses during the walk. `README.md`'s flag table still needs a
`-mutateestimate` entry — flagged as a follow-up, not done as part of this
gap.

### Problem

There is currently no way to know, before starting a real `-mutate` run,
how many mutants it will generate or how long it will take. Tonight's own
benchmark work found this out the expensive way:
`corpus/stdlib-strconv-parseuint`'s `isprint.go` alone produces 2552
`literal/number` mutants (see gap 10's sibling finding, and PROGRESS.md's
gap-1 v2 section) — a run that looked like an ordinary small-package sweep
turned into a multi-hour one with zero warning beforehand. `-fuzz` doesn't
have a direct analogue to crib from here (it runs for a caller-specified
*duration*, not until a fixed, unknown-in-advance corpus is exhausted), so
this is new ground, not a port of an existing `go test` flag's behavior.

### Design decisions

**11a. Reuse the existing walk to count, don't build a second one.**
`engine.mutateFile`/`visitNode` already visits every node and computes
every mutation's ID unconditionally, even under `-mutatemutant=<id>`
replay, where only one matching mutation is ever handed to `run.run` (see
gap 4's `mutantID`/`Options.MutantID` mechanism — the exact precedent for
"walk everything, execute selectively"). Counting mode is the same shape
taken further: walk everything, execute *nothing*. Concretely, a new
`Options.EstimateOnly bool` (or a dedicated `mutate.Estimate(ctx, opts)`
entry point, sharing `plan()`/`mutateFile`/`visitNode` — open question,
not resolved here, on whether this is a mode of `Run` or a separate
function; a separate function is probably cleaner given the return shape
is genuinely different, not a `*Result` with `Status` fields that were
never actually determined) that makes `visitNode` skip the `run.run(ctx,
*spec)` call entirely and instead increment a per-package counter. This is
cheap — an AST walk with no `go test` subprocess anywhere — so estimate
mode should complete in roughly the time a normal run's *setup* takes
(parsing + `//nomutant` scanning), not meaningfully longer.

**11b. Baseline timing must be per-package, not one whole-module number,
whenever scope is narrower than `ScopeFull`.** This is the load-bearing
design point, not a detail: under `ScopePackage`/`ScopeImpact`, a real
mutant's `go test` invocation is scoped to just its own package
(`mutant.testArgs`, runner.go), so a single whole-module baseline
(`goTestSuite(goBin, opts.Dir, opts.patterns())`, already computed for
`-mutatetimeout`'s derivation) is the *wrong* per-mutant cost proxy — it
would systematically overestimate every package's cost by however much
larger the whole module's test suite is than that one package's own tests.
The estimate needs one baseline sample per package that actually has
mutants (encountered during 11a's walk), each timed the same way
`goTestSuite` already times the whole-module baseline, just scoped to that
package's own pattern instead of `opts.patterns()`. Under `ScopeFull`, the
existing whole-module baseline *is* the right number (every mutant really
does run `go test ./...` under that scope) — no new per-package timing
needed there, reuse what `resolveTimeout` already computes.

Real cost/accuracy tension worth surfacing, not hiding: a single baseline
sample per package (vs. `baselineRuns`' existing 3-run average) is faster
but noisier — cold-cache variance alone was measured tonight at 5x
(0.75s warm vs. 3.95s cold `GOCACHE`, see this session's conversation) for
the exact same test invocation. An estimate built on one noisy sample per
package could be off by a similar factor. Whether to spend 3x the setup
time for a steadier per-package average, or accept a rougher single-sample
estimate in exchange for speed (the entire point of this feature is being
*fast* to check before committing to the real run), is an open call —
lean toward a single sample for v1, with the estimate's own output text
saying plainly that it's a rough, not authoritative, number.

**11c. Extrapolation is honest about being optimistic, given tonight's own
evidence that parallel speedup is sub-linear under contention.** Total
estimated time = Σ over packages of (that package's mutant count ×
that package's baseline time), reported at least two ways: a naive
serial number (dividing by 1), and a number divided by the run's actual
`-mutateparallel` worker count (or its default) — labeled clearly as an
optimistic lower bound, not a promise, since real wall-clock speedup
depends on CPU contention this session directly observed tonight (a
measured ~8 mutants/minute against a raw per-mutant cost that should have
supported far more, once system load and shared-`GOCACHE` contention were
accounted for). Do not present a single confident number without that
caveat.

**11d. TCE interaction — explicitly out of scope for v1.** A mutant TCE
would filter as compiler-equivalent never reaches `go test` at all (see
gap 2), so a perfectly accurate estimate under `-mutatetce=true` would
need to run the (cheaper, but still real) TCE compile-and-compare step per
mutant during the walk — meaningfully more expensive than 11a's pure AST
walk, working against this feature's whole point of being a fast preview.
v1: the estimate ignores TCE and reports the raw mutant count regardless
of `-mutatetce`; note in the output that the real run may filter some
mutants TCE would catch, making the real run potentially faster than
estimated when TCE is on. A TCE-aware estimate is a reasonable v2, not
built here.

**11e. Flag naming — open question.** `-mutateestimate=true` (mirroring
`-mutatetce`'s boolean-flag shape) is the working name in this section;
alternatives like `-mutatedryrun` were considered and are also reasonable
— "dry run" signals "don't actually run it" clearly but doesn't by itself
promise a time estimate the way "estimate" does. Worth a naming call
before implementation, the same way gap 4 flagged `-mutant` vs.
`-mutatemutant` as an open naming question before that flag shipped.

### Files/functions touched

- `internal/mutate/engine.go` — new `Options.EstimateOnly` (or a separate
  `Estimate` entry point per 11a's open question), `visitNode`'s skip-run
  branch, per-package baseline timing threaded from `plan()`/
  `planPackage` the same way `planScope`'s `buildImpact`/`planTCEBaseline`
  already precompute other per-package artifacts once, before any mutant
  of that package runs.
- `internal/mutate/report.go` — a new result shape for the estimate (not
  `*Result`, which carries `Status`/`Output` fields an estimate never
  populates) and a console printer, parallel to `WriteSummary`.
- `cmd/turango/main.go` — new flag (name per 11e), short-circuiting to the
  estimate path and exiting before any real mutation run, `-mutateoutput`/
  `-mutatemin` presumably both no-ops in estimate mode (nothing was
  classified, so there's no report to write and no score to gate on) —
  worth an explicit check that main.go actually skips those rather than
  silently misbehaving.

### Build order

1. 11a (walk-only counting, `Options.EstimateOnly`) alone first — verify
   against a corpus fixture with a known golden mutant count that estimate
   mode's count exactly matches a real run's total, with zero `go test`
   subprocesses spawned (assert via the same kind of call-counter
   technique `loadTypedCalls` already uses to prove `loadTyped` is
   skipped when unneeded).
2. 11b (per-package baseline timing) — reuses `goTestSuite`'s existing
   shape, scoped per package.
3. 11c (extrapolation + console output).
4. CLI flag wiring (name per 11e).

### Verification

- Unit/fast test: estimate mode's mutant count matches a real `Run()`'s
  `len(Result.Mutants) + len(Result.Equivalents)` (equivalents still count
  as "would have been attempted" for an estimate that ignores TCE per
  11d) exactly, for at least one multi-package fixture — not just a
  single-file one, since 11b's per-package-not-whole-module design is the
  point being tested.
- Sanity check against tonight's own overnight benchmark data once it
  finishes: does the estimate's predicted time for
  `stdlib-strconv-parseuint` land anywhere near the real measured time
  `BENCHMARKS.md` will eventually record? This won't be an exact match
  (11c's own honesty about contention/sub-linear speedup), but a
  wildly-off estimate (10x+) would mean the design's cost model itself is
  wrong, not just imprecise.

---

## 12. Persistent mutant verdict cache (resume after interruption)

**Status: done**, built and verified exactly per the design below, in the
8-step build order it specifies. `internal/mutate/cache.go` (new file):
`cacheKey`/`cacheRecord`, `cacheFingerprint` (12a, whole-module or
dependency-closure file set per scope), `resolveToolchain`,
`loadCacheIndex`/`cacheStore` (12c/12d, JSON-Lines, torn-write recovery by
truncation). `Options.CacheDir` (empty disables, matching every other
opt-in feature's zero-value convention); `-mutatecache=<dir>`
(`cmd/turango/main.go`), rejected together with `-mutateestimate`,
accepted together with `-mutatemutant`. The central correctness claim —
that `mutantID` alone is unsafe, and the compound key actually closes the
gap — is proven directly by
`TestRunClosesSameMutantIDDifferentContentCollision`
(`internal/mutate/runner_integration_internal_test.go`): two real,
differently-behaving modules forced to share one `mutantID`, where the
second is verified to run a real `go test` rather than being served the
first's cached verdict. The resume-is-free claim is proven by
`TestRunCacheResumeIsFree` (`internal/mutate/engine_integration_internal_test.go`):
two `Run()` calls against the same unmodified fixture produce a
byte-for-byte identical `Result` with zero additional `execCalls` on the
second. Scope/TCE/`-mutatemutant` interactions each have their own
dedicated integration test in the same file.

### Problem

turango has **zero resume capability today**. Confirmed by grepping the
whole `internal/mutate`/`cmd/turango` source for "resume"/"checkpoint"/any
cache-file write: nothing exists. The only related mechanism is
`main.go`'s SIGINT/SIGTERM handling (`mutateRun`'s `interrupted` branch,
"turango: interrupted; reporting the mutants completed so far") — a
one-time snapshot of whatever `Result` the collector had accumulated in
memory at the moment of a *graceful* shutdown. It does nothing for a hard
kill (`SIGKILL`, an OOM reaper, a killed background job — exactly what
happened to a real overnight sweep against `corpus/stdlib-crypto-aes/module`
and a `corpus/stdlib-strconv-parseuint` bench run), and even on a graceful
shutdown it is not a checkpoint: relaunching after either kind of
interruption re-runs every mutant from scratch, including the ~97% of a
708-mutant sweep that had already completed once that same night.

The fix is a persistent, on-disk cache of mutant classification results,
keyed so that (1) re-running the same sweep against unchanged source skips
every mutant whose result is already cached, resuming near-instantly, and
(2) any code change invalidates *exactly* the entries it should — never
more silently-wrong, never fewer silently-stale. Getting (2) wrong is worse
than not building this at all: a cache that serves a stale verdict corrupts
exactly the number this whole tool exists to produce, silently.

### Design decisions

**12a. Cache key and invalidation — the central decision.** Three
candidate designs, weighed against the stated bar (never serve a
stale-but-still-matching verdict), not against convenience:

- **(Rejected as unsafe) `mutantID` alone.** `mutantID` (engine.go) hashes
  `(file path relative to module root, node's line, column, operator's
  registry name, mutation's index within Mutate()'s slice)` — a *position*
  in a specific AST, not the node's own bytes. Its own doc comment already
  admits this: "stable across re-runs of unchanged source... but not across
  edits to the file above the mutated line, since what's hashed is a
  position... not self-contained bytes." A shifted ID after an upstream
  edit is a safe *false miss* — wasteful, never wrong. The dangerous case
  the task framing asks to think through does exist: an edit that keeps a
  node's line, column, operator name and Mutate()-index all identical,
  while its actual content changes. This is not a contrived corner case —
  it's the ordinary shape of "replace this literal/identifier with a
  same-width one." Concretely: `literal/number` mutates `x + 5` to `x + 6`
  at some `(line, col)`; a developer later edits the source to `x + 7` at
  the *same* `(line, col)` (single-digit literal, same column). `mutantID`
  recomputed for the new mutation (`7`→`8`) is byte-identical to the old
  one (`5`→`6`) — same relPath, same line/col, same operator name, same
  index. A cache keyed on `mutantID` alone would serve the *old* mutant's
  verdict (a test suite's reaction to `x` becoming `x+6`) as the verdict
  for a completely different mutation (`x+7`→`x+8`) it never actually ran.
  That is precisely "a caching bug that reuses a wrong verdict" — not
  hypothetical, reachable by an ordinary one-character edit. `mutantID`
  itself is **not modified** by this design — gap 4's ID computation and
  `-mutatemutant=<id>` replay are untouched; the fix is purely additive, at
  the cache layer.

- **(Rejected as unnecessarily coarse) whole-module content fingerprint,
  always.** Hash every file under the module root once per run; any
  fingerprint mismatch invalidates the entire cache. This is *safe* — an
  edit anywhere legitimately changes the fingerprint, so no stale entry
  is ever served — but it throws away tonight's own second-order case: a
  large module where one file, unrelated to the package being mutated,
  changed between runs still invalidates every other package's cache too.
  Under `ScopeFull` this coarseness is not actually excessive — see below —
  but adopting it *unconditionally*, for every scope, gives up real,
  correctly-available precision for `ScopePackage`/`ScopeImpact` runs.

- **(Rejected as unsafe) mutated-file-only content fingerprint.** Hash only
  the one file being mutated; a change to any *other* file never
  invalidates its cache entries. This is the "per-file" option the task
  framing raises, and it fails the stated bar directly: a mutant's verdict
  is not a pure function of the mutated file's bytes. It is "did `go test`
  (scoped per `mutant.testArgs`) still pass" — and that test run's outcome
  depends on every file actually compiled and executed alongside it: sibling
  packages the mutated package imports, the test files themselves, and
  (under `ScopeFull`) potentially any package in the module, since
  `ScopeFull`'s entire reason to exist (its own doc comment) is that "a
  package's behaviour is frequently only asserted on by its callers'
  tests." A change to an imported helper, or to a test file, with the
  mutated file itself untouched, can flip a real mutant from `Survived` to
  `Killed` (or the reverse) — exactly the false-hit risk the task
  description warns is worse than no cache. Not adopted, even as a
  documented v1 shortcut, because the safe alternative below is barely
  more expensive.

- **(Chosen) `mutantID` + a scope-appropriate content fingerprint, gated
  further by scope/TCE/toolchain — reusing gap 5's dependency closure
  rather than inventing a second notion of "the relevant files."** The
  fingerprint's file set is defined to be **exactly the file set
  `runner.workspaceFor` would copy to execute this mutant** — i.e. the same
  files that actually determine `go test`'s outcome:

  - Under `ScopeFull`, or whenever gap 5's `resolveClosure`/`planClosure`
    declined for this package (`fileJob.closure == nil` — a `vendor/`
    directory, a `//go:embed` directive, an unsafe replace target, or
    `ScopeFull` itself, where a forward closure is provably wrong per gap
    5's own "Open questions — resolved" section): the fingerprint covers
    **every file under the module root** `copyModule` itself would copy
    (excluding `.git`, mirroring `copyTree`'s own skip rule exactly).
    This is not overcautious padding — it is the *provably correct*
    answer for that case, for the identical reason gap 5 already
    established: under `ScopeFull`, any file in the module could contain a
    test that starts or stops covering the mutated line, so "exactly the
    entries [an edit] should [invalidate]" genuinely is "all of them."
  - Otherwise (`fileJob.closure` non-nil): the fingerprint covers **exactly
    `go.mod`, `go.sum`, and every file directly inside each directory in
    the closure** — the identical file set `copyClosure` copies. An edit
    to a file outside that closure (a sibling package the mutated package
    never imports, an unrelated fixture directory) cannot affect this
    mutant's `go test` outcome, so it correctly does not invalidate this
    mutant's cache entry — this is what actually delivers tonight's wanted
    behavior (near-instant resume on zero code changes, and *narrow*
    invalidation on a real but unrelated edit) for any run not using
    `ScopeFull`.

  Computed via a new `cacheFingerprint(dir string, dirs map[string]bool)
  (string, error)` (mirrors `buildImpact`'s `(ctx, goBin, moduleDir, pkgDir
  string, goFiles []string)` parameter-naming style): walks the file set
  above in a deterministic (sorted-path) order, feeding each file's
  relative path and full content into one `sha256.New()` writer with
  `\x00` separators between fields — the same delimiter idiom `mutantID`
  already uses — and returns the full 64-character hex digest, **not
  truncated** the way `mutantID` is: `mutantID`'s truncation is fine
  because it is user-facing (pasted into a comment, typed into
  `-mutatemutant=`) and per-run; a truncation-induced collision on the
  fingerprint that gates every cache read would be a much worse, silent
  class of bug, so it keeps the full digest.

  The fingerprint is combined into a compound key with three more fields,
  each closing a specific, real gap this project has already run into
  elsewhere in its own history:

  - **`Scope` (the run's `Options.Scope.String()`)** — a real interaction
    the task explicitly asks about: the *same* mutant can be `Killed` under
    `ScopeFull` and `Survived` under `ScopePackage` (documented directly on
    the `Scope` type: "a mutant only a neighbouring package's tests
    exercise is killed under `ScopeFull` and survives under
    `ScopePackage`"). Since the fingerprint's own file set already differs
    by scope in the common case, this is technically redundant most of the
    time — but not provably always (a single-package module with no
    closure narrowing could produce the same fingerprint under both
    scopes), so it is included explicitly rather than relied upon
    incidentally.
  - **`TCE` (the run's `Options.TCE`)** — a mutant recorded as
    `Equivalent` under `TCE=true` must never be replayed as if that were a
    real verdict when the current run has `TCE=false`: that mutant was
    never run against the suite at all in the cached run, and under
    `TCE=false` it needs a real `Killed`/`Survived`/`NotViable`
    classification, which the cache does not have. Threaded onto `mutant`
    as an explicit `tceEnabled bool` field set from `Options.TCE` — **not**
    derived from `tceBaseline != nil`, because that field already means
    something narrower ("TCE requested *and* this package's baseline
    compile succeeded" — see `planTCEBaseline`'s fail-soft doc comment). A
    run with `TCE=true` whose baseline compile fails this time must still
    key its (real, non-equivalent) verdicts as `TCE=true` — reusing
    `tceBaseline != nil` as the key's TCE flag would silently mix them into
    the `TCE=false` bucket, which is wrong for the identical reason above.
  - **A toolchain fingerprint** (`Toolchain string`, e.g. `go version`'s
    output plus `GOOS`/`GOARCH`, resolved once via a new
    `resolveToolchain(ctx, goBin) (string, error)`, defaulting `GOOS`/
    `GOARCH` from `os.Getenv` with a `runtime.GOOS`/`runtime.GOARCH`
    fallback when unset) — a compiled/tested verdict can change across a Go
    version or a cross-compilation target even when every source byte is
    identical. Not exhaustive (build tags, `CGO_ENABLED`, and other `go
    env` values are not folded in for v1 — flagged honestly, not solved,
    the same way TCE's own reproducibility spike flagged what it did and
    did not verify): a cache directory shared across differently-configured
    machines is not proven safe by this design and should be treated the
    way `$GOCACHE` already implicitly is — machine/environment-scoped, not
    portable.

  ```go
  // cacheKey identifies exactly which prior run a cache record answers
  // for. Every field must match the current run's own value for a lookup
  // to be trusted — see ROADMAP.md gap 12a for why each one is here.
  type cacheKey struct {
      Toolchain   string // resolveToolchain: go version + GOOS/GOARCH
      Scope       string // Scope.String()
      TCE         bool
      Fingerprint string // cacheFingerprint's full hex SHA-256
      MutantID    string // mutantID, unmodified
  }
  ```

  `Options.Workspace` (copy vs. worktree) is deliberately **excluded**: it
  selects how a workspace is *built*, never what it contains or how it's
  tested, so two workspace strategies producing different verdicts for the
  same key would itself be a bug in workspace construction, not a
  legitimate source of cache variance. `Options.Parallel` needs no key
  entry either — it changes only how fast the cache is populated, never
  what gets written into it (the write path is already serialized, see
  12d). `Options.TestTimeout` is also excluded, deliberately, with an
  honest trade-off stated rather than solved: a `Killed`-by-timeout verdict
  cached under a short timeout could in principle be replayed even though a
  longer timeout might let that same mutant finish and reclassify as
  `Survived`. Including the exact timeout in the key would defeat the
  cache almost entirely in practice (the default is derived per run from a
  noisy baseline measurement, per `resolveTimeout`/`baselineTimeout`, and
  rarely reproduces bit-for-bit across runs) for a risk that only ever
  biases toward `Killed` — the same direction a real timeout already biases
  toward today (`run.run`'s own "a mutant that never terminates is a
  mutant the suite noticed" reasoning) — not toward silently missing a real
  kill.

**12b. Storage format and location.** JSON Lines (`.jsonl`), one
`cacheRecord` per line — chosen specifically for the "incremental append
safety" the task calls out: an append is a single `write()` call with no
read-modify-write of the rest of the file, unlike a single JSON array or
object that would require rewriting the whole file (or at least seeking
back over a trailing `]`) on every mutant. Plain `encoding/json` + a
`bufio.Scanner`/`bufio.Writer`, stdlib only, matching the "stdlib +
`golang.org/x/*` only" project policy (`PROGRESS.md`'s dependency-cleanup
note) — no sqlite/bbolt/badger considered, per the task's own framing.

Location: a new `Options.CacheDir string` field (empty means disabled,
matching the zero-value-is-safe convention every other opt-in feature in
this project already follows — TCE, dependency-closure copying,
git-worktree execution), with a fixed file name inside it,
`cacheFile = "mutate-cache.jsonl"` (mirroring `main.go`'s existing
`reportFile = "mutate-report.json"` constant-naming convention), joined via
a small `cachePath(dir string) string`.

This is a **deliberate divergence from `-mutateoutput`'s own precedent**,
worth stating explicitly rather than silently copying the wrong shape:
`-mutateoutput` is entirely post-hoc — `mutate.Options` has no output field
at all; `main.go`'s `mutateRun` calls `mutate.Run` to completion, then
separately calls its own `writeReport(cfg.output, result)` against the
*returned* `*Result`. That shape cannot work for a cache: caching must
affect `Run`'s own internal execution (skip `runner.run`'s expensive path
on a hit) and must write *incrementally*, mutant by mutant, not once at the
end — a Result only exists after `Run` returns, and by the time a
kill signal lands, it's too late to have used it. So `CacheDir` lives on
`Options` itself (same as `TCE`/`Workspace`), not layered on top by
`main.go` the way `-mutateoutput` is.

The cache directory must live outside every per-mutant temp workspace by
construction, and does: it is opened once, by the top-level `Run` /
`runner`, at a path the *caller* named — never inside `os.MkdirTemp("",
"turango-mutant-")` (deleted after every mutant, `run.run`'s own `defer
os.RemoveAll(tmp)`) or inside a `copyModule`/`copyClosure`/`copyWorktree`
workspace root (also per-mutant, also deleted). No change needed to make
this true; it falls out of where the cache is opened.

**12c. Write timing/durability.** Every completed verdict is appended as
soon as it's produced — not batched to end-of-run — via a single-consumer,
channel-fed writer (12d) opened once, before `execute()` starts, and closed
once, right after it returns (successfully or via cancellation) — the same
placement `sink := newCollector(); ...; return sink.close(), runErr`
already uses in `Run`. "Crash-safe" here means specifically: surviving a
killed *process* (tonight's actual failure mode — confirmed via `ps`/
`uptime`/`pmset`, not a power-loss event), not surviving a lost machine.
Each record is written with one `os.File.Write` call to a file opened
`O_APPEND`; POSIX guarantees that call either lands in full or (only on a
mid-write crash) leaves a partial trailing line — a killed process cannot
interleave two records' bytes, only truncate the last one. No per-record
`fsync` is needed against that threat model (data handed to `write()`
survives a killed process via the OS page cache regardless of fsync); one
`fsync`-on-close is done as a cheap, one-syscall-per-run belt-and-suspenders
measure, not a per-mutant cost.

Recovery is a **load-time truncate**, not a lazy skip: `loadCacheIndex`
reads line by line; the first line that fails to `json.Unmarshal` (which,
by the write guarantee above, can only ever be the *last* line present) is
treated as a torn write from a prior kill — the file is physically
truncated (`os.Truncate`) to the byte offset just before that line, and
loading continues as if it were never there. Truncating on load, rather
than only skipping the bad line while reading, keeps the on-disk invariant
simple for every future load and every future append: the file is always
either empty, or a sequence of complete JSON lines, never
"complete lines + garbage + more complete lines." A missing cache file is
not an error — `loadCacheIndex` returns an empty index — matching every
other "first run, nothing to reuse yet" case elsewhere in this project
(e.g. `TCE`'s fail-soft per-package baseline).

**12d. Concurrency.** Two independent halves, each reusing an
already-established pattern in this codebase rather than inventing a third:

- **Reads are lock-free by construction.** The entire cache file is loaded
  once, sequentially, into an immutable `map[cacheKey]cacheRecord`
  (`cacheIndex`) *before* `execute()` starts spawning concurrent file
  workers — the identical "read-only, safe to share across
  `Options.Parallel` workers with zero synchronization" shape
  `fileJob.mutators`'s run-wide shared slice already relies on today. No
  mutation ever happens to a loaded `cacheIndex` during a run.
- **Writes are serialized through a single consumer goroutine fed by one
  channel** — `cacheStore`, structurally identical to `collector`/
  `estimateTally` (engine.go): a `record(rec cacheRecord)` method that
  blocking-sends on a channel (the same effective backpressure
  `collector.mutant`'s doc comment already describes), one goroutine
  draining it and doing the actual file `Write`, and a `close()` that
  closes the channel and blocks for the consumer to finish (flush +
  fsync). This is a **separate** consumer from `collector`'s own three
  channels, not folded into it: `collector`'s job is building the
  in-memory `*Result`, a different failure mode from cache durability — a
  disk-full or permission error writing the cache must never lose or
  corrupt the run's real, in-memory `Result`, so keeping them as two
  independently-failing components means a `cacheStore` write error can be
  logged and dropped without collector ever knowing anything went wrong.

**12e. Where in the pipeline the check happens.** Inside `runner.run`
(runner.go), immediately **after** the existing `ScopeImpact`
no-covering-test shortcut (which stays exactly as it is — it is already
zero-cost, never spawns a subprocess, so caching it would add complexity
for no benefit) and **before** `testArgs`/`os.MkdirTemp`/`workspaceFor` —
i.e., strictly earlier than TCE's own insertion point (gap 2 places TCE
"after the workspace copy... before `r.goTest`"; the cache check must be
even earlier, since a hit should skip the workspace copy too, not just the
`go test` call).

```go
if r.cache != nil && !m.replay {
    if rec, ok := r.cache.get(m.cacheKey(r.toolchain)); ok {
        if rec.Equivalent {
            return result, false, true, nil
        }
        result.Status, result.Output = rec.Status, rec.Output
        return result, true, false, nil
    }
}
```

`m.cacheKey(toolchain string) cacheKey` is a small method on `mutant`
assembling the compound key from `m.cacheFingerprint`, `m.scope.String()`,
`m.tceEnabled`, `m.id`, and the passed-in toolchain string. `m.replay`
(new field, `= job.mutantID != ""`, set in `mutateFile`'s `spec :=
mutant{...}` construction) gates `-mutatemutant=<id>` replay past the
*read* path only — see 12f.

`before`/`After` are **not** persisted in the cache record and are not
needed on a hit either as a stored value: they are cheap
(`renderNode`/`renderAfter`, printer calls against the in-memory AST, no
subprocess) and already computed earlier in `run.run`, before this check,
exactly the same way a real run computes them — so a cache hit's
`MutantResult` is built from a mix of freshly-rendered local fields
(`ID`, `File`, `Line`, `Operator`, `Description`, `Before`, `After` — all
already known or already computed by this point in `run.run`, cache-hit or
not) and exactly two fields trusted from disk (`Status`, `Output`). This is
a deliberate minimization: the fewer fields a stale or corrupted cache
record could lie about, the smaller the blast radius of any bug in this
mechanism, and it also shrinks the on-disk record.

Two new write call-sites, both existing "a real verdict was just produced"
return points in `run.run`, each gaining one line:

- The `isTCEEquivalent` branch (`return result, false, true, nil`) — write
  `cacheRecord{Key: ..., Equivalent: true}` first.
- The final `classify`/timeout paths (`return result, true, false, nil`,
  including the timeout-`Killed` branch) — write
  `cacheRecord{Key: ..., Status: result.Status, Output: result.Output}`
  first.

Both are unconditional on `m.replay` — a replay run's freshly-confirmed
verdict is still written through, updating (in effect, appending a fresher
record for the same key — see 12g on why an older, now-redundant record
for the same key is not actively removed) whatever a future normal run
would read.

The syntactic no-op check (`bytes.Equal(src, m.baseline)`) stays entirely
outside caching, deliberately: it already runs before any subprocess, on
every walk, so there is nothing expensive to skip, and — since a cache hit
requires an identical `Fingerprint`, which by construction means the exact
same file bytes that produced this determination originally — the walk's
own upstream syntactic-no-op filtering already reproduces deterministically
on a fingerprint match, with nothing for the cache to add.

**12f. Interaction with existing features.**

- **TCE**: covered in 12a (the `TCE` key field) and 12e (the `Equivalent`
  record shape) — an equivalent verdict is cached and honored the same as
  a real one, short-circuiting even the TCE compile step on a hit (not
  just `go test`), and is never served to a run with a different `TCE`
  setting.
- **`-mutatemutant=<id>` replay**: bypasses the cache *read* unconditionally
  (`m.replay` gate in 12e) but still *writes through* — replay exists to
  actually reproduce/observe a specific mutant against a real `go test`
  run (a debugging tool), and serving a cached verdict instead would defeat
  that purpose entirely; but there's no reason a freshly-confirmed replay
  verdict shouldn't benefit a later full run's cache hit rate, so it's
  still recorded.
- **`-mutatescope`**: yes, part of the key (12a) — required precisely
  because, per `Scope`'s own doc comment, the identical mutant can classify
  differently across scopes.
- **`Options.Parallel`**: no effect on correctness — the read side is an
  immutable map built before any worker starts, and the write side is
  already serialized through one consumer goroutine (12d), so this was
  already a solved problem by the time caching was added, inherited
  directly from `collector`'s existing design.
- **`Options.Workspace`**: excluded from the key (12a) — does not affect
  what is tested, only how the workspace is built.
- **`-mutateoutput`**: unaffected and unaware of caching — a report written
  from a mostly-cache-hit run is field-for-field indistinguishable from one
  produced by a fully fresh run, since every `MutantResult` field is either
  freshly computed locally or (for `Status`/`Output` only) trusted from a
  record whose key already proves it describes this exact mutant under
  this exact scope/TCE/toolchain/source-fingerprint.
- **`-mutateestimate`**: rejected together with `-mutatecache` at CLI parse
  time (12h) — an estimate classifies nothing, so there is nothing to
  cache and nothing a cache could short-circuit.

**12g. Cache growth/pruning.** **Unbounded growth is accepted for v1**,
stated plainly rather than silently ignored. Every edit that changes a
package's fingerprint (12a) makes every previously-written record for that
fingerprint permanently dead — never looked up again, since lookups are
always keyed by the *current* fingerprint — but nothing here actively
removes it, so a project edited and re-mutated repeatedly over its lifetime
accumulates an ever-growing `mutate-cache.jsonl`. Records are individually
small (a status enum plus an already-≤16KB-truncated `Output` string,
usually far smaller for `Killed` verdicts), so this is a slow, bounded-rate
leak, not a runaway one, and the fix for v1 is the same one `$GOCACHE`
itself relies on: **deleting the `-mutatecache=<dir>` directory is always
safe and is the v1 pruning mechanism** — no code needed, no correctness
risk (an empty/missing cache is just a first run). A real compaction
pass (rewrite the file keeping only the newest record per key still worth
keeping, or `-mutatecacheprune`-style command) is reasonable future work,
explicitly not part of this build order.

**12h. Flag design.** `-mutatecache=<dir>`, mirroring `-mutateoutput`'s
exact shape (a directory value is itself the opt-in — no separate boolean
toggle the way `-mutatetce=true` needs one, since there is no meaningful
"cache enabled with no location" state to distinguish). Empty/absent means
disabled, the safe zero-value default every other opt-in feature in this
project uses. `flagCache = "mutatecache"` joins the `mutateFlags` const
block and slice; `mutateConfig.set` gains `case flagCache: return
c.setCache(value)`; `setCache` mirrors `setOutput` exactly (reject an empty
value with the same message shape). Unlike `-mutateoutput`, `setCache`
writes directly to `c.options.CacheDir` — no separate `mutateConfig` field
— per 12b's rationale (`Options` must own this, not `main.go`
post-hoc). The existing `-mutateestimate` rejection block in
`parseMutateFlags` (today: `cfg.estimate && (cfg.output != "" ||
cfg.hasMin)`) extends its condition to `|| cfg.options.CacheDir != ""`,
same error shape, same rationale ("nothing is classified in estimate mode,
so there is no verdict to cache"). `-mutatecache` combined with
`-mutatemutant` is explicitly **allowed**, not rejected — see 12f.

### Files/functions touched

- **`internal/mutate/cache.go`** (new file, mirroring `impact.go`'s role as
  a self-contained per-concern file):
  - `cacheKey`, `cacheRecord` types (12a, 12e).
  - `cacheFile`, `cachePath(dir string) string` (12b).
  - `cacheFingerprint(dir string, dirs map[string]bool) (string, error)`
    (12a) — whole-module walk when `dirs` is nil, `go.mod`/`go.sum` +
    `dirs`' files otherwise, mirroring `copyModule`/`copyClosure`'s exact
    file sets.
  - `resolveToolchain(ctx context.Context, goBin string) (string, error)`
    (12a).
  - `cacheIndex` type, `loadCacheIndex(path string) (*cacheIndex, error)`,
    `(*cacheIndex) get(key cacheKey) (cacheRecord, bool)` (12c, 12d).
  - `cacheStore` type, `newCacheStore(path string) (*cacheStore, error)`,
    `(*cacheStore) record(rec cacheRecord)`, `(*cacheStore) close() error`
    (12c, 12d).
- **`internal/mutate/engine.go`**:
  - `Options.CacheDir string` (new field, doc comment matching `TCE`'s
    "off by default... every other `Options` field's safe-default
    philosophy" framing).
  - `Run` (lines ~275–341 as of this writing): resolves `toolchain` once
    (only if `CacheDir != ""`), loads `cacheIndex` once, opens
    `cacheStore` once — both fail-soft (a load/open error disables caching
    for this run with a warning, never fails the run outright, mirroring
    `loadClosures`' own "a load error here just means no package gets a
    narrower workspace this run" precedent) — constructs `runner{...,
    cache:, store:, toolchain:}`, and closes `store` after `execute`
    returns (success or cancellation).
  - `plan()`/`planPackage()`: gain a `fingerprintMemo map[string]string`
    (moduleDir → fingerprint), populated at most once per distinct
    `moduleDir` across the whole (sequential, pre-`execute()`) planning
    pass — the `ScopeFull`/closure-declined case's whole-module hash must
    not be recomputed once per package, the same amortization concern
    `fullBaseline` already solves for `buildEstimateResult`. A new
    `planCacheFingerprint(ctx, opts, moduleDir string, dirs map[string]bool,
    memo map[string]string) (string, error)` returns `""` immediately when
    `opts.CacheDir == ""`, the identical zero-cost-unless-selected shape
    `planTCEBaseline` already establishes for `Options.TCE`.
  - `fileJob` gains `cacheFingerprint string`, `tceEnabled bool` (=
    `opts.TCE`, threaded alongside the existing `tceBaseline []byte`, kept
    distinct per 12a's rationale).
  - `mutateFile`'s `spec := mutant{...}` construction threads
    `cacheFingerprint: job.cacheFingerprint`, `tceEnabled:
    job.tceEnabled`, `replay: job.mutantID != ""`.
- **`internal/mutate/runner.go`**:
  - `runner` struct gains `cache *cacheIndex`, `store *cacheStore`,
    `toolchain string`.
  - `mutant` struct gains `cacheFingerprint string`, `tceEnabled bool`,
    `replay bool`; new method `(m mutant) cacheKey(toolchain string)
    cacheKey`.
  - `run.run`: new cache-check branch (12e) inserted after the
    `ScopeImpact` shortcut, before `testArgs`; two new `r.store.record(...)`
    call sites (12e).
- **`cmd/turango/main.go`**:
  - `flagCache = "mutatecache"` const, added to the `mutateFlags` slice.
  - `mutateConfig.set`: new `case flagCache`.
  - `setCache(value string) error` (mirrors `setOutput`).
  - `parseMutateFlags`'s existing `-mutateestimate` rejection condition
    extended per 12h.

### Build order

1. `cache.go`: `cacheKey`/`cacheRecord` + `cacheFingerprint` +
   `resolveToolchain`, unit-tested standalone (see Verification) with no
   engine wiring at all — matching gap 2's "the load-bearing test... must
   be written and passing *before* wiring [it] into the run loop, not
   after" precedent, since a broken fingerprint is exactly the class of
   bug this whole design exists to prevent.
2. `cache.go`: `loadCacheIndex`/`cacheStore`, unit-tested standalone
   (recovery-truncate behavior in particular) before any engine wiring.
3. `Options.CacheDir`; `plan()`/`planPackage()`'s fingerprint memo and
   `planCacheFingerprint`; `fileJob.cacheFingerprint`/`.tceEnabled`.
4. `mutant.cacheFingerprint`/`.tceEnabled`/`.replay`, `mutant.cacheKey`;
   `mutateFile`'s threading.
5. `runner.cache`/`.store`/`.toolchain`; `Run`'s construction/fail-soft
   load-and-open/close-after-execute wiring.
6. `run.run`'s cache-check branch and the two `store.record` call sites.
7. `main.go`: `-mutatecache` flag wiring and the `-mutateestimate`
   rejection extension.
8. Integration tests (see Verification) — in particular the
   resume-is-free test and the same-mutantID-different-content
   collision-safety test, since those two are the concrete proof of this
   section's central correctness claim, not just its prose.

### Verification

- `cacheFingerprint` unit tests: identical file set/content, hashed from
  two different temp-dir copies (mirrors gap 2's `compile()` reproducibility
  test shape) → equal fingerprints; one byte changed anywhere in the
  relevant file set → different fingerprint; a change to a file *outside*
  the relevant set (narrow-scope case: a file in a sibling package the
  closure doesn't include) → **unchanged** fingerprint — this is the
  concrete "the scope restriction is enforced, not just documented" test
  gap 1's own verification section models.
- `loadCacheIndex`/`cacheStore` recovery test: write N complete JSONL
  records via the real writer, then append a deliberately truncated,
  syntactically-invalid trailing line directly (simulating a kill mid-write,
  not going through `cacheStore` to produce it) — assert `loadCacheIndex`
  recovers exactly the N complete records, silently drops the trailing
  garbage, and that the file is truncated on disk to end exactly after the
  Nth record; assert a subsequent `newCacheStore` on that same path can
  append further records cleanly afterward.
- **The load-bearing end-to-end test**: run `Run()` twice against the same
  unmodified fixture module with the same `Options.CacheDir`. Assert the
  first run performs the expected number of real `go test`/`go build`
  subprocesses (via the existing `execCalls` atomic counter, the same
  technique `TestWalkForEstimateSpawnsNoSubprocess` already established)
  and the second run against **unchanged source** produces a byte-for-byte
  identical `Result` (same mutants, same `Status`/`Output`) while
  `execCalls` does **not increase at all** — the concrete proof of
  "resumes near-instantly," not an assertion in prose.
- **The central correctness test, proving the dangerous direction is
  actually closed**: two fixture variants sharing the exact same
  `(relPath, line, column, operator, index)` tuple — so `mutantID` is
  identical across them — but genuinely different content at that node
  (e.g. a same-width literal edit, `x + 5` → `x + 7`, at the same column).
  Populate the cache against variant A; run against variant B with the same
  `CacheDir`. Assert variant B's mutant is **not** served from the cached
  verdict (a fresh `go test` actually runs — `execCalls` increases — and
  the resulting verdict, if it differs from A's, is what's reported) — this
  is the test that directly disproves "mutantID alone is a safe cache key"
  rather than merely asserting it, and is the single most important test
  in this whole build order.
- Scope-interaction test: cache a verdict under `ScopeFull`; re-run the
  identical mutant, unchanged source, under `ScopePackage` — assert it is
  **not** served from cache (independently computed), proving `Scope` is
  load-bearing in the key, not decorative.
- TCE-interaction test: cache an `Equivalent` verdict under `TCE=true`;
  re-run with `TCE=false` on unchanged source — assert the mutant is
  **not** short-circuited as equivalent, and instead receives a real
  `Killed`/`Survived`/`NotViable` classification.
- `-mutatemutant` interaction test: prime the cache with a normal run, then
  run `-mutatemutant=<id>` for one of its mutants with the same
  `CacheDir` — assert a real `go test` execution still happens
  (`execCalls` increases), proving replay always bypasses the cache read;
  assert a subsequent normal run's cache now reflects that replay's result.
- `main_internal_test.go`: `-mutatecache=<dir>` combined with
  `-mutateestimate=true` is rejected at parse time, mirroring the existing
  `-mutateoutput`/`-mutatemin` + `-mutateestimate` rejection test;
  `-mutatecache=<dir>` combined with `-mutatemutant=<id>` is **accepted**
  (a negative-of-a-negative test, guarding against someone "fixing" this
  by copying the estimate rejection too broadly).
- `go vet`/`golangci-lint` clean with the new file in the tree, same bar
  every other file in this repo already clears.

---

## 13. `mutantID` collision on left-associative binary-expression chains

**Status: done.** See the top summary list's entry for the fix summary —
found as a side effect of investigating a real `corpus/stdlib-crypto-aes`
golden discrepancy (PROGRESS.md's 2026-08-12 entry). Fixed via a per-file
collision-rank counter (`internal/mutate/engine.go`'s `idDeduper`) rather
than the operand-index approach originally guessed at here — the rank
approach generalizes to any future node type that shares this
`Pos()`-collapsing behavior, not just `operator/binary` chains, and keeps
every non-colliding mutant's ID byte-for-byte identical to before the fix.

---

## 14. README badges (build/quality status, plus a mutation-score badge)

**Status: built.** See the top summary list's entry for what's live now
vs. what still needs the user's own account/secret setup at Codecov and
SonarCloud, and why the mutation-badge workflow's first real number
should be spot-checked once it actually runs in Actions.

### Problem

turango's README has no status badges at all today — not even a build
badge for `ci.yml`, which already exists and would cost nothing to
surface. The user asked for a badge row modeled on another of their
projects (`gradebot`)'s: `Go Reference` (pkg.go.dev), `Tests` (GitHub
Actions), `CodeQL`, `Codecov`, and two SonarCloud badges (quality gate,
coverage) — plus asked whether a badge showing the mutant/mutation-score
count is feasible. Checked before writing this: turango has none of the
underlying infrastructure four of those six badges assume — no
`codeql-analysis.yml` workflow, no Codecov account/upload step, no
SonarCloud project. Adding the badge markdown without the workflow behind
it would just render a broken/red badge, which is worse than no badge.

### Design decisions

**14a. Two badges are free right now, the rest are a real decision.**
`Go Reference` needs zero CI changes — it's pkg.go.dev's own badge for
any tagged public Go module. `Tests` is a one-line badge pointing at
`ci.yml`'s existing workflow. `CodeQL` needs a new
`.github/workflows/codeql-analysis.yml` (GitHub's security-scanning
workflow) — a real but small, self-contained addition. `Codecov` and
both `SonarCloud` badges each need a new external account, a CI upload
step, and (depending on the account's tier/repo visibility) a secret
token to manage — a genuine new operational dependency, not something to
add reflexively just because `gradebot` has it. Whether that overhead is
worth it for a not-yet-1.0 tool mid-way through a Go proposal is a call
for the user, not an engineering one.

**14b. A mutation-score badge is possible, but needs a live data
pipeline, not a static badge.** Rejected outright: hand-typing a number
into a static badge URL — it rots the moment a mutant is added or fixed,
and a badge nobody re-checks is worse than no badge (same reasoning as
14a's "don't add a broken badge"). Two real options, both needing turango
to mutation-test *itself* in CI and publish the result somewhere a badge
service can read:
- **shields.io endpoint badge**: CI runs turango against its own module
  (this project already dogfoods this way — see
  `TestRunAgainstRealModule`), computes a score from the JSON report, and
  publishes a small JSON file in shields.io's endpoint schema
  (`{"schemaVersion":1,"label":"mutation score","message":"92%","color":"green"}`)
  to somewhere stable and public — a dedicated `gh-pages`/badges branch
  this repo's own CI pushes to.
- **GitHub Gist + shields.io's gist endpoint badge**: lower setup cost (no
  Pages/branch config) but couples the badge to an external gist ID
  staying in sync, and needs its own write-token secret.
Either way, this needs its own scheduled workflow, not a per-PR one — see
14c.

**14c. Rejected: running self-mutation-testing on every PR to drive the
badge.** Same reasoning `corpus.yml` already established for this
project's own regression corpus (see that file's comment): a real
mutation sweep is too slow to gate every push. A scheduled +
`workflow_dispatch` cadence, mirroring `corpus.yml`'s own pattern, keeps
the badge fresh without becoming a bottleneck on ordinary PRs.

### Files touched

`README.md` (badge row under the title); `.github/workflows/codeql-analysis.yml`
(new, if CodeQL is pursued); a new `.github/workflows/mutation-badge.yml`
(self-mutation run + badge-JSON publish, scheduled like `corpus.yml`, if
the mutation-score badge is pursued); possibly a `gh-pages` branch.

### Build order

1. Add the two free badges (`Go Reference`, `Tests`) — zero new
   infrastructure, safe to do immediately.
2. Decide with the user whether CodeQL/Codecov/SonarCloud are worth their
   external-account overhead at this project's current stage — a scope
   decision, not a code one.
3. If pursuing the mutation-score badge: build the self-mutation-testing
   workflow on `corpus.yml`'s schedule/`workflow_dispatch` pattern, pick a
   badge-hosting mechanism (`gh-pages` endpoint vs. gist), wire the
   JSON-publish step.

### Verification

Every badge's underlying URL reflects real, live CI/service state — no
badge pointing at a workflow that doesn't exist or a service turango
isn't actually wired into. The mutation-score badge's number changes
across a run that adds or removes mutants, proving it's live data, not
frozen at add-time.

---

## Suggested sequencing across gaps 1-6

(Written when gaps 1-6 were this document's entire scope; gaps 7-14 were
added later and are not covered by the sequencing notes below.)

Independent enough to parallelize, but if built serially, **3 before 1
before 2** is the lower-friction order for gaps 1–3. Gap 4 (mutant IDs) is
lower-risk and largely independent of the other gaps' design questions —
it can land whenever, but doing it *after* gap 3 means `MutantResult` only
gets restructured once (both add fields to the same struct).

- Gap 3 touches all 11 operator files plus `runner.go`'s `run.run` and
  `engine.go`'s `mutateFile`, but every change is additive and mechanical —
  doing it first means gaps 1 and 2 (which also touch `runner.go`/
  `engine.go`, more substantially) rebase onto a smaller, already-landed
  diff instead of the reverse.
- Gap 1 is additive at the package level (new `internal/mutator/identifier`
  package, a `TypedMutator` interface, new `engine.go` functions) and
  touches `plan()`'s per-package job construction — the same area gap 2
  needs to add a per-package baseline-compile step to. Landing gap 1's
  `fileJob`-construction changes first means gap 2 extends an already-
  updated `plan()` rather than the two colliding on the same function
  in-flight.
- Gap 2 is the highest-risk of the three (the reproducibility spike is a
  real go/no-go gate, not a formality) and benefits from landing last, once
  the codebase's `plan()`/`fileJob`/per-package-precompute pattern has
  already been extended once (by gap 1) and is a known-good shape to extend
  again.

Gaps 5 and 6 both touch `runner.go`'s `copyModule` and are best sequenced
**after** 1–3 settle `plan()`/`fileJob`'s shape (gap 1 in particular already
adds a per-package precompute step there) rather than in parallel with them
— two unrelated changes to the same workspace-construction code at once is
exactly the kind of collision the gap-1-before-gap-2 note above is trying to
avoid elsewhere. Within 5 and 6 themselves: 5 before 6, per gap 5's own
"higher priority than gap 6" section — 5 is the universal fix, 6 is a
narrower, git-only optimization layered on top of whatever scope 5 already
narrows the copy down to.
