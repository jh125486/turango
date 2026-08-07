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

## State as of last check (verified directly against the repo, not trusted from old notes)

- `go build ./...`, `go vet ./...`, `go test ./...` (non-`-short`, includes real integration tests), `gofmt -l .` all clean on real turango source (frozen `corpus/stdlib-*` fixture source is deliberately left un-gofmt'd — it's historical stdlib code, not ours to reformat).
- Operator count: **13**, across 6 packages (`control`, `expression`, `literal`, `operator`, `statement`, `identifier`).
- Git history:
  ```
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

## If resuming after a break

1. Everything through the identifier/constant-swap operator, the `-mutate` redesign, and the corpus harness is committed (see git log above). **Nothing is mid-flight or uncommitted** — working tree was clean as of the last check.
2. **The one real open thread**: `corpus/stdlib-crypto-aes/` and `corpus/stdlib-encoding-base64/` need their `golden.json` files captured (module source is already committed, just needs running against a build of the current binary and the real numbers written in per the schema every other `corpus/*/golden.json` already follows). Do this on a faster/less-loaded machine, and run each fixture's `turango test` invocation *alone*, foreground, without a `timeout N` wrapper that can kill it mid-run — that specific pattern produced bad data repeatedly last session. Sanity-check: full-scope must never report fewer mutants than package-scope for the same fixture.
3. ROADMAP.md has 6 gaps now, in priority order: (1) identifier-swap v2 (local-var-to-const, closes the real strconv shape), (2) TCE, (3) before/after snippets, (4) deterministic mutant IDs, (5) dependency-closure workspace copy, (6) git-worktree execution (opt-in only, lower priority than 5). None of 2-6 are built.
4. Ask the user before committing anything new — this doc records what's already committed, not a standing permission to keep committing.
5. If asked "does X exist / is X implemented," verify against the actual current source (`grep`/`Read`) before answering — this session had several rounds of the user correctly catching an assumption stated from memory instead of verified code. Don't repeat that.
