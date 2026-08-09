# turango build status

Plan: `/Users/jacob.hochstetler/.claude/plans/wondrous-crunching-avalanche.md` (source of truth for design)

## Task status (6 build phases + phase 7 follow-on)

1. [DONE] Scaffold module + shim + mutator skeleton — go.mod, internal/goproxy (passthrough), cmd/turango/main.go (dispatch+alias gate), internal/mutator/mutator.go (interface+registry)
2. [DONE] Port 9 AST mutators — internal/mutator/{control,expression,statement,operator}
3. [DONE] Mutation engine + runner — internal/mutate/{engine,runner,report}.go, baseline-derived per-mutant timeout (3-run avg x NumCPU), go test -json classification, replace/vendor/embed-aware temp workspace copy
4. [DONE] //nomutant suppression — internal/mutate/suppress.go, statement-anchored leading/trailing, cascades into compound-statement bodies via ast.Inspect returning false
5. [DONE] CLI flags + worker pool + scope modes — cmd/turango/main.go flag parsing (-mutate, -mutatescope, -mutateoperators, -mutateparallel, -mutatetimeout, -mutateoutput, -mutatemin), file-level worker pool (errgroup), ScopeFull/Package/Impact (impact = per-test coverage map via internal/mutate/impact.go), SIGINT/SIGTERM partial-report flush
6. [DONE] Reporting polish + example/ package + full plan Verification pass against final built binary — Status JSON marshal, SuppressionRatio(), relativized paths, surviving-mutants console listing, example/ package (pricing.go/stats.go + deliberately imperfect tests + 2 //nomutant cases + README with real output). Every bullet in the plan's Verification section confirmed against the actual built binary.
7. [DONE] Validate against known historical Go bugs — see "Phase 7 results" in the plan file. Two case studies (strconv ParseUint overflow: negative but honest — bug was a wrong-constant substitution, a mutation shape turango's 9 operators can't produce; encoding/base64 Decode corrupt-index: found a live surviving mutant at a code path structurally identical to the shipped bug but not touched by the historical fix — the actual "money shot"). Third candidate (bytes/strings IndexRune) abandoned as not worth the isolation effort. Concrete gap identified: no identifier/constant-swap operator exists yet. Not committed to the repo — this was a validation exercise, artifacts left in /tmp/bugcheck/.

## Extra: example/legacy package (added after phase 6, committed)

Ported the original go-turango prototype's example.go/example_test.go unchanged (foo/bar, single coarse assertion). Demonstrates the engine on real prior-art weak-test code: 38 mutants, 24 killed, 12 survived, 66.7% score. Survivors land exactly on the unreachable switch case, dead if-branch, and discarded bar() calls — a clean, concrete demo of what mutation testing catches that a single overall-result assertion misses. The one assertion originally used `testify/assert.Equal`; it's now a plain `t.Errorf` check (see "Dependency cleanup" below) — same demo, same behavior.

## Extra: two new mutation operators + a literal package (post phase-7, committed)

Added after the phase-7 historical-bug validation surfaced a concrete gap ("no constant-mutation operator" — see task 7 above and PROPOSAL.md's Evidence section):

- **`operator/boundary`** (`internal/mutator/operator/boundary.go`) — relational boundary shift: `<`↔`<=`, `>`↔`>=`. The classic off-by-one mutant. Distinct from `operator/binary`, which does negation-style swaps (`==`↔`!=`, `&&`↔`||`, etc.) and never touches boundary relations.
- **`internal/mutator/literal/`** (new package) — `literal/number` (shifts an int or float literal by ±1) and `literal/boolean` (`true`↔`false`).

This closes *part* of the constant-mutation gap — literal ints/floats/bools and relational boundaries are now covered. A full identifier/constant-swap operator (rewriting a named constant or identifier reference, e.g. the historical strconv#21278 wrong-bit-width-constant shape) still requires `go/types` to resolve identifier bindings and is not built — that remains open, documented as a known limitation in PROPOSAL.md.

Total operator count is now 12 (the original 9 plus these 3), across 5 packages: `control`, `expression`, `literal`, `operator`, `statement`.

## Extra: dependency cleanup + PROPOSAL.md (post phase-7, committed)

- **Removed `github.com/stretchr/testify`.** Project policy is stdlib + `golang.org/x/*` only, no other third-party deps. The one test that used it, `example/legacy/legacy_test.go`, was rewritten to a plain `if got != want { t.Errorf(...) }` check — same assertion, no dependency. `go.sum` no longer references testify (the only remaining non-`golang.org/x` line is `google/go-cmp`, a transitive test-only dependency of `golang.org/x/tools`, not a direct import).
- **`go.mod`** had two separate `require(...)` blocks (an artifact of `go get` being run at different times); merged into one alphabetized block (`golang.org/x/mod`, `golang.org/x/sync`, `golang.org/x/tools`).
- **Added `PROPOSAL.md`** at repo root: a golang/go-style proposal document arguing for `go test -mutate` as a stdlib feature, modeled directly on the real `-fuzz` design draft. Cites the project's 2018 predecessor paper's crypto findings (crypto/aes's volatile mutation score across early Go versions, crypto/x509/pkix at 0%), a fresh 2026 re-validation of those same two findings against current `golang/go` HEAD, the phase-7 case study where a real historical stdlib bug shape (encoding/base64 CorruptInputError) was reproduced and a live sibling survivor found, and frames the proposal as complementary to `golang/go#75315` (a real, currently-open, narrower Go proposal for assembly-only mutation testing filed by Filippo Valsorda). Not a code change; purely a document making the case for upstreaming.

## Extra: identifier/constant-swap operator (ROADMAP gap 1, committed)

13th operator, `identifier/constswap` (`internal/mutator/identifier/`): package-level
const-for-const swap, exact `types.Identical` type match, scoped to the same
`const(...)` block or file. First operator needing `go/types` — added
`mutator.TypedMutator` (additive interface, `WithScope(info, pkg) Mutator`
returns a new package-bound value, never mutates the shared registry
instance) and lazy type-checking in `engine.go` (`needsTypes`/`loadTyped`,
only paid when a typed operator is actually selected; a plain-operator run
resolves zero extra type info — verified via an atomic call counter in a
test, not just inferred). v1 is intentionally narrow: does **not** reproduce
the exact strconv `ParseUint` bug shape (that was local-var-to-const, not
const-to-const) — a v2 extension is documented in ROADMAP.md, not built.

## Extra: `-mutate` redesigned as a function-name regexp (real bug fix, committed)

The user caught a real design defect, not a docs issue: `-mutate`'s value
was being assigned directly to `Options.Packages` (a package pattern),
which does not match how `-run`/`-bench`/`-fuzz` actually work — confirmed
against `go help testflag` and an empirical `go test -bench ./...` vs.
`-bench=. ./...` test before touching any code (the space-separated form
silently ran zero benchmarks — real footgun, reinforced why turango's
existing `=`-only flag convention is correct and was not loosened).

Fixed for real: `-mutate=<regexp>` now matches function/method names (via
a new `Options.FuncPattern`, filtered in `mutateFile`'s walk by skipping
non-matching `*ast.FuncDecl` subtrees — same cascade-skip mechanism
`//nomutant` already uses). Package selection moved to ordinary trailing
positional arguments in `cmd/turango/main.go`, exactly like the three real
flags. `-mutate=.` matches every function, mirroring `-bench=.`. A real
nil-pointer panic was caught and fixed along the way (a test constructing
`fileJob{}` directly left `funcPattern` nil; now treated as "match
everything," matching the empty-pattern convention). Verified end-to-end
against the real binary, not just unit tests: `-mutate=.` → 41 mutants,
`-mutate=NoSuchFunctionXYZ` → 0, `-mutate=^shiftBy$` → a real 6-mutant
subset.

Every doc (`README.md`, `PROPOSAL.md`, `ROADMAP.md`, `example/README.md`,
`example/doc.go`) was corrected to match — `example/README.md`'s captured
output is a real re-run (160 mutants/117 killed/31 survived/12 not-viable,
79.1%), not a syntax-only text edit, since the operator count had also
grown from 9 to 13 since that doc was last accurate.

## Extra: mutation corpus + regression harness (ROADMAP-adjacent, committed, PAUSED/PARTIAL)

`internal/corpus/` + `corpus/`: golden-file discovery (`corpus.go`) and a
table-driven `TestCorpus` (`.github/workflows/ci.yml`'s "Corpus regression"
step) that runs every `corpus/*/golden*.json` entry through `mutate.Run`
and asserts exact counts — the actual "eat our own dog food" regression
gate the user asked for, so an engine change that silently breaks mutation
generation now fails a real test instead of nothing catching it.

**17 of 19 intended entries are done and trustworthy**: 13 hand-built,
minimal per-operator fixtures (`corpus/op-*/`, one per mutation operation,
each a tiny deliberately-uncovered function so every mutant survives
deterministically — systematic coverage, not incidental), plus
`stdlib-x509-pkix`, `stdlib-strconv-parseuint`, `example`, `example-legacy`.

**`stdlib-crypto-aes` and `stdlib-encoding-base64` are explicitly PAUSED,
not done** — this is the one open thread if resuming. Their frozen
`module/` source is committed (real material, ready to use) but has no
`golden.json` yet: every attempt to capture their real numbers this session
produced bad data (interrupted runs killed by a background agent's own
timeout wrapper before finishing; full-scope aes reported *fewer* mutants
than package-scope, which is impossible, proving the run was truncated) or
hit real CPU contention from too many parallel agents running mutation
sweeps simultaneously on one machine. `Discover()` skips any directory with
no golden file, so the harness runs clean today at 17/17 — finishing these
two is explicitly deferred to a faster machine, per direct instruction, not
forgotten.

## Extra: ROADMAP gaps 5 and 6 (dependency-closure copy, git worktrees)

Found while dogfooding the corpus harness above: `runner.go`'s `copyModule`
copies the *entire module* per mutant, not just the target package's
build/test closure — stopped being hypothetical the moment `example/`
started sharing a repo root with 17 new corpus fixture directories, which
directly slowed down `example`/`example-legacy`'s runs for no reason
related to them. Gap 5 (dependency-closure copy via `go/packages`' import
graph) is the fix, prioritized *above* gap 6 (git-worktree-based execution)
specifically because it benefits every user by default, the same way
`-fuzz` has zero git dependency — worktrees only help git users and must
stay strictly opt-in for the same reason. Neither is built; both are design
sketches in ROADMAP.md.

## Extra: golangci-lint tooling + all 43 findings fixed (committed, `fb8ee24`)

`tools/golangci-lint/` is a separate `go.mod` (dependabot-trackable,
mirrors the `jh125486/pdf2qti` pattern), invoked via `go tool -modfile=`;
`Makefile` gained a `lint` target, CI runs it. Every one of the first
run's 43 findings was either fixed for real (errorlint, goconst, a
gocritic `exitAfterDefer` bug in `main()`/`os.Exit` where `defer stop()`
was silently skipped, 2 `nilnil` returns turned into `(value, ok, err)`,
4 real `gocyclo` extractions) or justified-suppressed with a `//nolint`
reason (gosec subprocess-exec findings on code whose whole job is
shelling out to `go test`, a couple of test-fixture false positives).
`example/stats.go` gained named `Trend*` constants as part of the
goconst fix, which gave `identifier/constswap` new material — the
corpus/example golden counts and `example/README.md`'s captured output
were regenerated against the real binary, not hand-edited.

## Extra: mutant IDs, TCE, before/after snippets, channel collector,
## test-convention cleanup, dependency-closure components (committed, `40d0883`)

A single long session closed most of `ROADMAP.md`'s remaining gaps:

- **Gap 4, deterministic mutant IDs — done.** SHA-256 hash of (file path
  relative to module root, line, column, operator name, mutation index),
  truncated to 12 hex chars. `MutantResult.ID`, `-mutatemutant=<id>`
  replay flag (`Options.MutantID`) — the walk still visits every node,
  but only a matching mutation is ever handed to the runner.
- **Gap 2, TCE — done, but not as designed.** The spike (build the same
  package twice, from two different temp-dir copies, `-trimpath` + a
  fixed `-buildid`) found the original plan's raw-archive-byte
  comparison unreliable: Go's export data encodes source line positions,
  which shift whenever a mutation changes the line count, so a textbook
  equivalent mutant (dead-store elimination) compared as "different"
  purely from that noise. The shipped design compares normalized `go
  build -gcflags=-S` disassembly instead (line-position comments
  stripped) — validated against both a real dead-store-elimination case
  (correctly equal) and a real behavior change (correctly different).
  `-mutatetce=true` (`Options.TCE`), off by default — the risk direction
  here is asymmetric (a false positive silently discards a real mutant,
  unlike a narrower scope which can only under-report), so this stays
  opt-in until it has real-world runs behind it. `Result.Equivalents`,
  `EquivalentResult`, a `writeEquivalents` console line (only printed
  when non-zero).
- **Gap 3, before/after snippets — done, plan partly wrong.** The
  original plan said all 13 operators need a `Mutation.Node` addition;
  actually implementing it found only `control/{if,else,case}` and
  `statement/remover` do — every operator whose `Apply` edits a field in
  place on the node it was already called with needs no change, since
  printing that same node before/after already reflects the edit.
  `statement/remover` needed a second correction mid-implementation too:
  its `Apply` repoints a list slot rather than editing the removed
  statement's own fields, so printing the same node twice would show
  identical (stale) text — `MutantResult.After` resolves this by
  reporting empty when the before/after render is identical, not a
  duplicate of `Before`. `MutantResult.Before`/`.After`, populated in
  the JSON report; the console survivor table is unchanged (3c).
- **Gap 7, channel-based collector — done.** `collector` now sends on
  three channels to a single consumer goroutine instead of guarding a
  shared slice with a mutex. Not a correctness fix (the mutex was
  already race-safe) — a testability one: `testing/synctest`'s
  durably-blocked detection covers channel ops, not `sync.Mutex.Lock`,
  so this is what would let a future test drive `-mutateparallel`'s
  scheduling deterministically. Verified under `-race` with real
  concurrent file workers.
- **Gap 5, dependency-closure workspace copy — real design bug found,
  components built and unit-tested, deliberately NOT wired in.** The
  original design sketch's open question ("how does this interact with
  `-mutatescope=full`?") turned out to have a hard answer: it doesn't,
  and must not — a forward import closure can never capture the
  *reverse* closure `ScopeFull`'s cross-package kill detection depends
  on, so applying this under the default scope would silently
  misclassify mutants, not just cost performance. `closureDirs`,
  `resolveClosure`, `copyClosure`, `copyDirFiles`,
  `hasEmbedDirective` exist in `internal/mutate/runner.go`, scoped to
  only ever apply under `ScopePackage`/`ScopeImpact`, with automatic
  fallback to the existing full-module copy on any uncertainty (a
  `//go:embed` directive anywhere in the closure, a `vendor/` directory,
  any local `replace` directive). Unit-tested with hand-built
  `*packages.Package` graphs (no real toolchain needed for most of it).
  **Nothing calls these yet** — wiring `runner.run` to use `copyClosure`
  under the narrower scopes is left as its own, separately-reviewable
  step, on purpose: this is a correctness-sensitive activation, not a
  performance toggle, and deserved a human look before going live in an
  unattended session.
- **Gap 6, git worktrees — untouched.** Lower priority than gap 5, was
  sequenced after it; not started.
- **go-test-conventions cleanup.** Every `_test.go` file in the repo was
  audited (by 13 parallel subagents, one per package) against this
  project's 9 testing rules — table-driven, `t.Parallel()` throughout,
  blackbox-unless-justified (whitebox files renamed to
  `*_internal_test.go`), one test function per exported symbol,
  `t.Context()` over `context.Background()`, `testing/synctest` for
  timing. `go.mod` bumped `go 1.23` → `go 1.26` to allow `t.Context()`
  (1.24+) and `testing/synctest` (1.25+).
- **Integration test tagging.** The tests that shell out to the real Go
  toolchain (compiling/testing throwaway module copies) now live behind
  a `//go:build integration` tag instead of a `testing.Short()` skip —
  `go test ./...` never compiles them in at all. New `make
  test-integration` target, a dedicated CI step.

Verified throughout: `go build`/`go vet`/`gofmt` clean, `golangci-lint`
(with and without `-tags=integration`) reports 0 issues, full suite
(including `-race` on the concurrency-sensitive tests) passes in both
modes.

**`turango.png` was found modified (58KB → 1.5MB) with no corresponding
intentional edit this session** — excluded from `40d0883`, still needs
the user's review before it's committed either way.

## State as of last check (verified directly against the repo, not trusted from old notes)

- `go build ./...`, `go vet ./...`, `go test ./...` (non-`-short`, includes real integration tests), `gofmt -l .` all clean on real turango source (frozen `corpus/stdlib-*` fixture source is deliberately left un-gofmt'd — it's historical stdlib code, not ours to reformat).
- Operator count: **13**, across 6 packages (`control`, `expression`, `literal`, `operator`, `statement`, `identifier`) — unchanged this session (the mutant-ID/TCE/before-after work touched the engine and reporting, not the operator set).
- Git history:
  ```
  40d0883 Add mutant IDs, TCE, before/after snippets, channel collector, test-convention cleanup
  fb8ee24 Add golangci-lint tooling and fix everything it found
  576d91d Add mutation corpus + regression harness (paused: aes/base64 incomplete)
  5fbf380 Add turango.png mascot; correct -mutate docs; add ROADMAP gaps 5/6
  3d93ab6 Add identifier/constant-swap operator; redesign -mutate as a function-name regexp
  01e0cf2 Add README.md and ROADMAP.md; refresh PROGRESS/PROPOSAL for 11-op set
  c0ed9fd Add CI workflow, ignore .claude/, merge go.mod require blocks
  bc92dc5 Remove testify dependency; add mutation-testing proposal doc
  6524cf9 Add operator/boundary and literal/{number,boolean} mutators
  f804ab7 Add example/legacy: weak-test demo ported from go-turango
  3554d64 Add turango: mutation testing as a go command shim
  4332a9e Initial commit
  ```

## Two known limitations found during phase 6's final verification (not bugs, but worth knowing)

1. **Default full-scope `-mutate` against turango's own module is pathological**: `turango test -mutate=./example/...` from this repo (CLI syntax at the time this was written — `-mutate` has since changed to a `-run`/`-bench`/`-fuzz`-style function-name regexp, not a package pattern; the equivalent invocation today is `turango test -mutate=. ./example/...`), with default `-mutatescope=full`, took 17+ minutes because each mutant's `go test ./...` re-runs turango's *entire* module including its own end-to-end mutation-engine tests (which themselves spawn nested `go test` children) — every mutant blows the ~10s derived timeout and gets misclassified `Killed` via the (correct, documented) "timeout counts as Killed" rule. Not a defect in the timeout logic itself, just a bad interaction when dogfooding turango on its own repo. `example/README.md` recommends `-mutatescope=package` for this reason. Worth considering later: detect/warn when the target module IS turango's own module, or just always recommend package/impact scope for local dogfooding in docs.
2. **`-mutatescope=impact` reports zero-coverage lines (e.g. a bare `const` declaration) as `Survived`** rather than skipping them — inherent to coverage-based test selection (no test exercises a const decl directly), not a defect, but worth noting if the score looks unexpectedly low under impact scope.

## Key design decisions locked in (don't re-litigate, see plan file for full rationale)

- Shim is `turango`, not a patch to real `go` — intercepts `test ... -mutate=...`, forwards everything else verbatim via process replacement.
- Alias-as-`go` (renaming/symlinking turango ahead of real go in PATH) is experimental opt-in only (`TURANGO_EXPERIMENTAL_ALIAS=1` env var gate), not v1-advertised.
- //nomutant suppression cascades into compound-statement bodies (confirmed by user, matches Stryker-style scope suppression).
- Temp-workspace copy (not mutate-in-place) chosen for v1, fixed to handle replace directives/vendor/go:embed (confirmed by user).
- Kill-detection default scope is FULL suite per mutant (not package-scoped) — package and impact (coverage-map-based) are opt-in via -mutatescope.
- Per-mutant timeout is baseline-derived (3x real suite run, averaged, x NumCPU), not a fixed constant — added per user's specific request mid-build to avoid runaway mutants (e.g. i++ -> i-- infinite loops) wasting time up to go test's own 10-min default. Timeout hits classify as Killed.
- -mutateparallel parallelizes at the FILE level (not per-mutation) since one file's AST is shared/mutated in-place across its own mutants — safe boundary is per-file, not per-mutant.
- Mutant IDs hash (relative path, line, column, operator, mutation index) — not a counter — so IDs survive re-runs of unchanged source but are honestly not stable across upstream edits; this trade-off is documented, not hidden.
- TCE ships opt-in (default off), not opt-out, despite the ROADMAP's original lean toward opt-out — the risk is asymmetric (a false positive silently discards a real mutant; a narrow scope can only under-report kills), so the safer default won out. Revisit once there's real-world run history.
- Dependency-closure copying (gap 5) is gated to ScopePackage/ScopeImpact only, never ScopeFull — a forward import closure structurally cannot support ScopeFull's cross-package kill detection, which needs the reverse closure. This is load-bearing, not a style choice — don't let anyone wire it in for ScopeFull "as an optimization."

## If resuming after a break

1. **Check `git status` first.** `turango.png`'s modification is still excluded and unresolved — don't assume the working tree is clean just because this doc says so.
2. **The corpus aes/base64 thread is still open, untouched this session**: `corpus/stdlib-crypto-aes/` and `corpus/stdlib-encoding-base64/` still need their `golden.json` files captured. Do this on a faster/less-loaded machine, and run each fixture's `turango test` invocation *alone*, foreground, without a `timeout N` wrapper that can kill it mid-run. Sanity-check: full-scope must never report fewer mutants than package-scope for the same fixture.
3. ROADMAP.md's 7 gaps, current state: (1) identifier-swap **done** (v1 only — v2, local-var-to-const, closes the real strconv shape, still open); (2) TCE **done** (opt-in, `-mutatetce=true`); (3) before/after snippets **done**; (4) deterministic mutant IDs **done**; (5) dependency-closure workspace copy — **components built and tested, deliberately not wired into `runner.run`** (a correctness-sensitive activation decision left for direct review, not a TODO forgotten); (6) git-worktree execution — **untouched**; (7) channel-based collector **done**.
4. **Gap 5's wiring is the highest-value next step** if the user wants to keep going: `runner.run` needs to call `copyClosure` instead of `copyModule` when `m.scope != ScopeFull` and a per-package `resolveClosure` call (probably precomputed once in `planPackage`, the same shape `planTCEBaseline`/`buildImpact` already use) succeeded. The design and the reason it's scope-gated are fully written up in ROADMAP.md gap 5 — read that before touching `runner.run`, not just this summary.
5. Ask the user before committing anything new — this doc records what's already committed (or, right now, staged-pending-signing), not a standing permission to keep committing.
6. If asked "does X exist / is X implemented," verify against the actual current source (`grep`/`Read`) before answering — this project has repeatedly caught assumptions stated from memory instead of verified code. Don't repeat that.
