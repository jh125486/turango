# Roadmap: closing turango's validation-identified gaps

This is a planning document, not a changelog. Gaps 1-3 were identified during
the historical-bug validation work described in `PROPOSAL.md` (the
strconv/base64/crypto case studies); gaps 4-6 surfaced later, during the
corpus-harness and dogfooding work. In priority order, since the first two
are the load-bearing evidence for the eventual stdlib pitch:

1. An identifier/constant-swap mutation operator — the mutation shape behind
   the strconv `ParseUint` bug (#21278), which none of the current 11
   operators can produce.
2. Trivial Compiler Equivalence (TCE) — filtering equivalent mutants by
   compiled-output comparison, flagged in `PROPOSAL.md`'s "Costs and risks"
   section as only partially solved today (turango filters *syntactic*
   no-ops; TCE filters *semantic* no-ops that print as different source).
3. Before/after source snippets on `MutantResult`, so a report is usable
   without hand-deriving the diff from `Description` and a line number.
4. Deterministic mutant IDs — a `-fuzz`-style content hash per mutant, so a
   specific mutant can be referenced in a comment, a regression test, or
   replayed directly via a new CLI flag.
5. Dependency-closure workspace copy — `copyModule` copies the whole module
   per mutant today, not just the target package's actual build/test
   closure; cost scales with module size, not target-package size, and this
   is not hypothetical (see the section for how it surfaced directly while
   dogfooding turango's own corpus harness).
6. Git-worktree-based execution, strictly opt-in — a cheaper *mechanism* for
   the same copy step gap 5 addresses the *scope* of, only for users already
   inside a git repo; lower priority than gap 5 because it only benefits git
   users, whereas gap 5 benefits everyone the same way `-fuzz` does (no git
   dependency at all).

Each section below states the problem, the design decisions and their
rationale, the exact files and functions touched, a build order, and how to
verify the result — matching the precision of the original build plan
(`~/.claude/plans/wondrous-crunching-avalanche.md`). Genuinely open questions
are called out as such, not resolved by assertion.

All file/line references below are current as of this writing (single
commit, `4332a9e`); re-check line numbers before implementing if the tree has
moved on.

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

This is a real architectural step up. `internal/mutate/engine.go`'s `load()`
(lines 385–422) is explicit about avoiding this today:

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

**v2 (not this plan's build order, flagged as follow-on)**: extend the
candidate set to include local variables whose declared type exactly
matches a package-level constant's type, restricted further to identifiers
used as an operand of a comparison (`<`, `<=`, `>`, `>=`, `==`, `!=` — the
same "boundary-relevant" spirit as `operator/boundary`), since that's where
a wrong-constant substitution actually changes behavior dangerously. This
filter is a heuristic, not a proof that it's sufficient or that it won't
still explode on a function with many boundary comparisons; it needs its own
validation pass against the strconv fixture before being trusted.

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

  `Run()` (engine.go, currently lines 175–207) calls `loadTyped` right after
  the existing `load(ctx, opts)` call, only if `needsTypes(mutators)`, and
  threads the result into `plan()`. `loadTyped` uses
  `packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax |
  packages.NeedImports | packages.NeedDeps` — the mode `load()`'s comment
  explicitly avoids today, now scoped to only the callers who ask for it.

  A package that fails to type-check under `loadTyped` is demoted for that
  operator only — the exact fail-soft precedent `plan()` already uses for
  `ScopeImpact` (engine.go lines 494–509: *"Demoting this one package to
  package scope costs time and nothing else, whereas failing the run would
  throw away every other package's work"*). Here, the demotion is "run this
  package's mutants without the identifier/const-swap operator," not a scope
  change.

**1c. Interface shape — do not touch `Mutator`.** `mutator.Mutator`
(mutator.go lines 61–77) is a two-method interface every one of the 11
operators implements statelessly. Adding scope/type context to it —
e.g. `Applies(node ast.Node, info *types.Info) bool` — would force all 11
existing, purely-syntactic operators to accept a parameter they ignore. That
directly contradicts the package doc's stated design goal (mutator.go lines
1–28): keep the interface to exactly what an operator needs, materialized as
a slice of `Mutation`, nothing more.

There is also a concurrency hazard to solve, not just an ergonomics one. A
single `Mutator` instance is shared and reused across the whole run
(`mutator.All()`, mutator.go lines 139–152), and files — including multiple
files of the *same* package — are mutated concurrently up to
`Options.Parallel` (engine.go's `fileJob`/`execute`, lines 214–225,
424–445). If a typed operator's scope/type info were set as a mutable field
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

In `plan()` (engine.go, lines 460–524), per package: if `needsTypes` was
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
  `Run()` (lines 175–207) and `plan()` (lines 460–524); `fileJob.mutators`
  becomes per-package-derived instead of always the run-wide shared slice;
  blank import `_ "github.com/jh125486/turango/internal/mutator/identifier"`
  added to the existing side-effect import block (lines 27–31).
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
  (`internal/mutator/operator/operator_test.go`'s `parseFunc`/`render`/
  `findNode` helpers) — except `parseFunc`'s own doc comment states
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

TCE (Kintis/Papadakis/Malevris et al., "Trivial Compiler Equivalence") is
deliberately unsophisticated: compile both versions, compare the resulting
object code, declare equivalent on a match. Per this project's own prior
research pass (referenced in the task framing), it should be **always-on**,
unlike mutant subsumption (a *selection* technique that trades completeness
for speed and should stay opt-in) — TCE is a *filtering* technique with no
completeness trade-off, assuming the compiled-equivalence check itself is
correct.

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

**2a. Compare compiled package archives, not linked binaries.** `go build -o
pkg.a <pattern>` on a non-main package writes the compiler's `.a` archive
directly — no linker involved. This is cheaper than `go test`'s own build
path (which also compiles test files and links a test binary) and matches
the granularity the TCE literature itself operates at: comparing the
compiled unit, not a whole linked executable.

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

### Problem

`MutantResult.Description` (report.go lines 110–112) is a terse
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
sub-node rather than a whole file — `operator_test.go`'s `render()` test
helper, which already does exactly `printer.Fprint(&buf, fset, node)` for a
single node rather than a file).

**3b. Which node to print is not always the walk's outer node.** `MutantResult
.Before`/`.After` need to be captured around `run.run`'s existing
`m.mutation.Apply()`/`defer m.mutation.Revert()` pair (runner.go lines
96–101). The walk (`mutateFile`, engine.go lines 536–645) does carry a
`node ast.Node` per iteration, but for `statement/remover` specifically,
that outer node is the *container* (`*ast.BlockStmt`/`*ast.CaseClause`), not
the individual statement being deleted — per that operator's own package
doc comment (statement.go lines 1–24): `Applies`/`Mutate` are called on the
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

`statement/remover.go`'s `Mutate` loop (lines 80–98) already has the exact
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

**3c. Console vs. JSON.** `writeSurvivors` (report.go lines 331–365) is
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
- `internal/mutate/engine.go` — `mutateFile` (lines 536–645) already tracks
  `spec.line`/`spec.operator` per node in its `ast.Inspect` callback; add
  `spec.node = node` alongside those as the fallback the runner uses when
  `Mutation.Node` is nil.
- `internal/mutate/runner.go` — `mutant` struct (lines 31–61) gains a `node
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
  fixtures used by `TestResultJSONRoundTrip` (line 78) and `TestWriteSummary`
  (line 212) with `Before`/`After` values; assert the JSON round-trips them
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

### Open questions, not resolved here

- Does this replace `copyModule` outright, or become a second strategy
  selected automatically when the target module is large enough to make
  the difference worth the added complexity (a whole-module copy has
  fewer moving parts and is easier to reason about correctness-wise)?
- How does dependency-closure scoping interact with `-mutatescope=full`,
  which by design reruns the *whole module's* test suite per mutant — if
  the workspace itself doesn't contain the whole module, does `full`
  scope need its own broader closure (every package, not just the target
  package's dependencies), effectively falling back toward a full-module
  copy for that scope specifically?

---

## 6. Git-worktree-based execution (optional, opt-in only)

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

## Suggested sequencing across all six

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
