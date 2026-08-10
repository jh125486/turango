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

## Extra: ROADMAP gap 6 done (`-mutateworkspace=worktree`); gap 5 reviewed, real bug found, still not wired in

Found while dogfooding the corpus harness above: `runner.go`'s `copyModule`
copies the *entire module* per mutant, not just the target package's
build/test closure — stopped being hypothetical the moment `example/`
started sharing a repo root with 17 new corpus fixture directories, which
directly slowed down `example`/`example-legacy`'s runs for no reason
related to them. Gap 5 (dependency-closure copy via `go/packages`' import
graph) is prioritized *above* gap 6 (git-worktree-based execution)
specifically because it benefits every user by default, the same way
`-fuzz` has zero git dependency — worktrees only help git users and must
stay strictly opt-in for the same reason.

**Gap 6 is now built**: `-mutateworkspace=worktree` (`Options.Workspace`),
strictly opt-in, zero value unchanged from today's `copyModule` behavior.
`copyWorktree` in `runner.go` uses `git worktree add --detach`, falling
back to `copyModule` automatically whenever the target isn't inside a
*clean* git working tree (checked across the whole repo, not just the
mutated module — `git worktree add` checks out `HEAD`, never the on-disk
working copy) or isn't a git repo at all (every `corpus/*/module/` fixture,
by design). One real macOS-specific bug found and fixed during
implementation: computing the worktree's module-relative path with
`filepath.Rel(repoRoot, moduleDir)` silently produced a *wrong* path — and
in the worst case, one that pointed back at the original module's files
instead of the new worktree's copy, with no error — because `/var/...`
under `t.TempDir()` is a symlink to `/private/var/...` that `git
rev-parse --show-toplevel` always resolves through but an arbitrary
`moduleDir` argument isn't guaranteed to already be resolved the same way.
Fixed by asking git for the prefix directly (`git rev-parse
--show-prefix`) instead of computing it lexically. Caught only because the
new tests assert on the *content* of the checked-out files and that
`cleanup()` actually removes the worktree, not just on a boolean `ok`.
Verified end to end: `TestRunWorkspaceWorktreeMatchesCopy` (integration)
proves a git-committed fixture classifies identically under
`WorkspaceCopy` and `WorkspaceWorktree`.

**Gap 5 (dependency-closure copy) is still deliberately unwired**, but got
an independent review this session (spawned as a fable-model subagent,
reviewing only the already-built-but-inert code, not the session that
wrote it) that found a real, load-bearing bug before any activation: `closureDirs`/`resolveClosure`
only walk `pkg.Imports`, which is production-code-only — a blackbox
`_test.go` file (`package foo_test`) is a *separate* `*packages.Package`
whose imports never merge in. Since this project's own test-convention
cleanup (below) made blackbox-by-default the norm across the repo, wiring
gap 5 in as-is would very likely misclassify real mutants as `NotViable`
in exactly the packages this project itself just finished converting to
that convention — the existing unit tests don't catch it because every
test fixture hand-builds a `*packages.Package` with the needed imports
already present. A second, currently-dormant bug (an in-module `replace`
target outside the forward closure isn't detected as a fallback trigger)
was also found. Full writeup and the concrete fix needed before wiring is
in ROADMAP.md gap 5's new "Independent review before activation" section —
read that before touching `runner.run`/`planPackage` for gap 5, not just
this summary.

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

## Post-reboot verification (2026-08-10, this session)

Fable subagent `a8a3728d82642a0ff` (gap 5 bugfixes) confirmed **dead** —
`SendMessage` returned "No transcript found," reboot killed it. Its diff
was already on disk though, so verified manually instead of trusting its
self-report: `go build ./...`, `go vet ./...`, `gofmt -l .` (clean except
frozen `corpus/stdlib-*` fixtures, which is expected/intentional),
`golangci-lint run` both with and without `-tags=integration` (0 issues
each), `go test ./...`, `go test -tags=integration
./internal/mutate/...` — **all clean, exit 0**. Manually read the full
`runner.go` diff: it does fix both bugs the earlier fable review found —
`resolveClosure` now takes `...*packages.Package` and merges variants
(`mergeVariants`) so a blackbox `_test.go` package's imports are no longer
invisible to the closure walk, and `inModuleReplaceTargets` now catches an
in-module `replace` target that falls outside the computed closure as a
fallback trigger. Gap 6 (`-mutateworkspace=worktree`) also verified intact
in the same diff. **All 11 modified files are now believed good and ready
to commit** — not yet committed, per standing instruction (see item 7
below); ask before committing.

`turango.png`'s unexplained modification (58KB → 1.5MB, flagged in commit
`40d0883`'s note) is still unresolved and still excluded from any commit.

## Gap 5 activation, ROADMAP gaps 8/9 added, PROPOSAL.md scope note (2026-08-10, same session, later)

The 11 files above were reviewed, verified, and **committed** (`dfe1ce2`)
before this section's work started — `turango.png`'s own separate
modification turned out to already be committed independently in `d9ba7e1`
("added logo"), confirmed intentional by the user, nothing left pending on
it.

With gap 5's components already fixed and verified inert, this session
did the activation step ROADMAP.md gap 5 had deliberately deferred:
`engine.loadClosures` (new, mirrors `loadTyped`'s zero-cost-unless-needed
shape, gated on scope != `ScopeFull`) and `engine.planClosure` (new,
per-package, resolves via `resolveClosure`) feed `fileJob.closure` →
`mutant.closureDirs` → `runner.workspaceFor`, which now tries
`copyClosure` first when non-nil, ahead of the existing worktree/copyModule
fallback chain (see `workspaceFor`'s rewritten doc comment for the
three-way precedence and why a worktree can never substitute for a
closure copy — it always checks out the whole repo at HEAD). One new unit
test (`TestWorkspaceForPrefersClosureOverWorktree`) proves that precedence
directly; the existing `TestRunWithImpactScope` integration test (already
running under real `ScopeImpact`) now exercises the new path end-to-end
against a real module with no changes needed to the test itself. Full
re-verification after wiring: `go build`, `go vet`, `gofmt -l .`,
`golangci-lint` (both modes), `go test ./...`, `go test -tags=integration
./internal/mutate/...` — all clean. ROADMAP.md gap 5 updated to **Done**
throughout (summary list + detailed section).

Also this session: **ROADMAP.md gap 8** added (benchmark mutation-testing
overhead vs. KLOC/test-KLOC, using Go's own `testing.B`/`go test -bench`/
`benchstat` pipeline rather than a bespoke script — not started, design
only) and **gap 9** added (a five-part comment-cleanup plan: staleness
audit, dead-code/marker sweep, format consistency, and citation rigor are
all safe to do without further sign-off; a verbosity trim is explicitly
flagged as needing the user's own go-ahead first, since it would reverse
this project's established, repeatedly-reaffirmed long-rationale-comment
style — not started as a whole, but 9a/9c/9e's *audit* work was done
inline this session, see next paragraph).

**9a/9c/9e done inline, not deferred**: grepped for `TODO`/`FIXME`/`XXX`
across the repo (excluding frozen `corpus/stdlib-*`) — none found. Checked
`internal/mutate/*.go` and `internal/mutator/**/*.go` for stale
"not wired in"/"not built" style status claims — none remain (the one real
instance, `closureDirs`' now-false "NOT WIRED INTO THE ENGINE YET," was
fixed as part of the gap 5 activation work above, before gap 9 was even
proposed). Spot-checked every `[Xxx]`-bracket godoc cross-reference this
session's own new comments introduced — all resolve to real symbols,
consistent with the codebase's existing (non-standard but consistent)
`[engine.Xxx]`/`[runner.Xxx]` pseudo-qualified style. **Found and fixed a
real citation error while auditing 9e**: ROADMAP.md gap 2 attributed TCE to
"Kintis/Papadakis/Malevris et al." — verified via web search that this is
wrong; the actual ICSE'15 TCE paper's authors are Papadakis, Jia, Harman &
Le Traon (Kintis co-authored a different, related paper on second-order
mutation for equivalent-mutant isolation). Fixed with a real DOI link
(`https://doi.org/10.1109/ICSE.2015.103`), matching the citation rigor
`PROPOSAL.md`'s existing `[Mull]` reference already sets.

**PROPOSAL.md scope note added** (user's explicit request): a new "Costs
and risks" bullet plus an Abstract-level pointer, stating plainly that
turango is a reference implementation with only mild, conservative cost
controls (scope narrowing, baseline timeout, worker parallelism, opt-in
TCE/closure-copy) and does not attempt techniques the mutation-testing
literature treats as standard at scale (mutant subsumption/selective
mutation, higher-order mutation, ML-guided prioritization, diff-scoped
incremental testing) — the proposal's claim is that the *mechanism*
belongs in the toolchain, not that this is the fastest possible
implementation of it.

**Not yet committed** as of this section: `PROPOSAL.md`, `ROADMAP.md`,
`internal/mutate/engine.go`, `internal/mutate/runner.go`,
`internal/mutate/runner_internal_test.go` — ask before committing, per
standing instruction.

## If resuming after a break

**Paused 2026-08-10 for a laptop reboot, mid-task — read this section fully before doing anything.**

1. **Check `git status` first.** As of the pause: 11 files modified, uncommitted (gap 6's work — see #3). `turango.png`'s modification is still excluded and unresolved. Don't assume the working tree is clean just because this doc says so.
2. **Check on the fable subagent (gap 5 fixes) first — it may have finished, stalled, or need a nudge.** It was asked (via `SendMessage` to agent id `a8a3728d82642a0ff`, no name set) to fix two real bugs an earlier fable review found in `internal/mutate/runner.go`'s unwired gap-5 code: (1) `closureDirs`/`resolveClosure` only walk `pkg.Imports`, blind to blackbox `_test.go` imports (the load-bearing bug — this repo's own tests are blackbox-by-default); (2) an in-module `replace` target outside the computed closure isn't caught as a fallback trigger. It was told explicitly NOT to wire `copyClosure`/`resolveClosure` into `runner.run`/`planPackage` — that stays a separate decision. Its last two status updates both said "waiting on the corpus regression suite" (`TestCorpus`, the slow real end-to-end suite) — but a direct process check at the time found *no* `go test`/`turango-bin` process actually running and an empty output file being `tail -f`'d since hours earlier: it looks stalled, likely hit the same background-job-killing issue described in #5 below, not genuinely still working. It DID leave real, substantial diffs on disk already (`internal/mutate/runner.go` +368/-35, `internal/mutate/runner_internal_test.go` +436, `ROADMAP.md` +130 as of the pause) — read those diffs and verify them yourself (`go build`, `go vet`, `gofmt -l .`, the golangci-lint invocation below, `go test ./...`, `go test -tags=integration ./internal/mutate/...`) rather than trusting its self-report or re-running its same stalled approach. If it's still alive, `SendMessage` to the same agent id will resume it with context; if not, finish verifying/cleaning up its diff yourself.
3. **Gap 6 (`-mutateworkspace=worktree`) is done** — implemented, tested, lint-clean, documented in README.md and ROADMAP.md, verified against the full `go test ./...` suite. Uncommitted, staged-ready. One real macOS-specific bug was found and fixed during implementation: computing a worktree's module-relative path via `filepath.Rel(repoRoot, moduleDir)` is unsafe because `t.TempDir()`'s `/var/...` is a symlink to `/private/var/...` that `git rev-parse --show-toplevel` resolves through but an arbitrary `moduleDir` argument isn't guaranteed to be pre-resolved the same way — fixed by asking git for the prefix directly (`git rev-parse --show-prefix`) instead of computing it lexically. See `copyWorktree`'s doc comment in `runner.go`.
4. **The corpus aes/base64 golden.json capture is still open, and has a newly-diagnosed root cause**: every attempt to run a fixture's full mutation sweep in this environment's backgrounded Bash gets killed after roughly a minute — confirmed even running one job alone, with no other concurrency, so it isn't resource contention or a timeout this session set. Root cause undiagnosed (looks like an environment-level ceiling on backgrounded Bash job wall-clock time). **This needs to run in the user's own foreground terminal**, outside Claude Code's background job mechanism entirely — see the exact commands in the conversation, or just re-derive them from `README.md`'s flag docs (`-mutatescope=package -mutatetimeout=30s -mutateoutput=<dir>` against each fixture's `module/` dir). Sanity-check once captured: full-scope must never report fewer mutants than package-scope for the same fixture.
5. **New sub-thread this session, NOT finished — corpus provenance via git submodule** (user's idea, in response to the aes/base64 struggle): pin the exact upstream `golang/go` commit(s) each stdlib-sourced corpus fixture (`stdlib-strconv-parseuint`, `stdlib-x509-pkix`, `stdlib-crypto-aes`, `stdlib-encoding-base64`) was actually cut from, via real git submodules, so provenance is verifiable rather than just asserted. Findings so far (each one required real commit-level archaeology — release-tag bisection alone was not enough, and burned a lot of tool calls; a shallow `--filter=blob:none` sparse-checkout clone of `golang/go` was made at `/tmp/goclone` for this, scoped to `src/crypto/x509/pkix src/encoding/base64 src/crypto/aes src/strconv` — gone after the reboot, re-clone if resuming this thread):
   - `stdlib-strconv-parseuint`: **resolved, exact match.** Byte-for-byte identical to commit `1d81251599fd1b8f9da888e10c1054c96d1e1fb1` — the parent of `fc6b74ce39748efc360afea4164c92a710ad6e77`, the commit that fixed golang/go#21278 (the exact historical `ParseUint` overflow bug this fixture exists to reproduce). Verified via direct byte diff against that commit's `src/strconv/atoi.go`. Safe to submodule-pin with zero risk to the existing golden.json.
   - `stdlib-x509-pkix`: **resolved, base + documented edits.** Base commit `a0da9c00ae` ("crypto: add available godoc link", landed in `go1.22.0`) — confirmed by finding it's the *only* commit touching `pkix.go` between `go1.21.0` and `go1.22.0`, and the diff against it stays constant (56 lines) all the way through `go1.26.0`, meaning nothing upstream changed after that point. The remaining 56-line diff is not upstream drift — it's real, deliberate local edits on top: a `strings.Builder` refactor of `RDNSequence.String()`, a genuine behavioral tweak (skip hex-encoding for string-typed attribute values before falling through to normal escaping — a real semantic change, not just style), and an added doc comment on `AttributeTypeAndValue`. Safe to submodule-pin the base commit; the fixture's actual `.go` file stays as-is (the local edits are the fixture, not drift to eliminate).
   - `stdlib-encoding-base64`: **resolved, negative result — no real commit to pin.** Checked back to `go1.14` (2020); at every version checked, upstream is *longer* than the fixture (~130 lines longer even at the oldest point checked), and the whole decode loop (the SIMD-style fast paths, mid-stream newline handling, padding validation) is restructured, not trimmed. This is a hand-written/heavily-adapted fixture inspired by base64, not a freeze of any real commit — going back further than `go1.14` is very unlikely to converge and would just be guessing. Do not keep bisecting this one.
   - `stdlib-crypto-aes`: **not started.** The plan (per direct user instruction) was to spot-check `aes.go` plus 2-3 other files the same way pkix/base64 were checked, and stop early if the same "no clean pin, hand-adapted" pattern shows up (same era of code, and base64's result makes that the likely outcome) — full-tree bisection across all 61 files is not warranted unless the spot-check disagrees between files. Pick this up fresh; nothing was found or ruled out yet.
   - Given the mixed results, the two submodules originally planned ("current" and "historical") may not both be worth adding — `strconv-parseuint` alone justifies a "historical" submodule pinned to `1d81251599fd1b8f9da888e10c1054c96d1e1fb1`; whether a "current" submodule is worth adding for `x509-pkix`'s single base commit (and possibly `aes`, TBD) is a judgment call once `aes`'s spot-check lands — don't assume the two-submodule plan is still the right shape without re-checking with the user, since `base64` turned out to not need one at all.
6. ROADMAP.md's 7 gaps, current state: (1) identifier-swap **done** (v1 only — v2, local-var-to-const, closes the real strconv shape, still open); (2) TCE **done** (opt-in, `-mutatetce=true`); (3) before/after snippets **done**; (4) deterministic mutant IDs **done**; (5) dependency-closure workspace copy — **components built and tested; an independent (fable) review found two real bugs; a second fable pass was fixing them as of the pause, see #2 above — status unconfirmed until verified**; (6) git-worktree execution — **done** (`-mutateworkspace=worktree`, see #3); (7) channel-based collector **done**.
7. Ask the user before committing anything new — this doc records what's already committed, not a standing permission to keep committing. Also: never retry a blocked `git commit` unattended/in a loop when it's failing on a physical-presence signing requirement (1Password Touch ID/fingerprint) — attempt once, report, stop; that was explicit user feedback earlier this session after a long unattended retry loop.
8. If asked "does X exist / is X implemented," verify against the actual current source (`grep`/`Read`) before answering — this project has repeatedly caught assumptions stated from memory instead of verified code. Don't repeat that.
