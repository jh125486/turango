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

Ported the original go-turango prototype's example.go/example_test.go unchanged (foo/bar, single coarse `assert.Equal(t, 16, foo())`). Demonstrates the engine on real prior-art weak-test code: 38 mutants, 24 killed, 12 survived, 66.7% score. Survivors land exactly on the unreachable switch case, dead if-branch, and discarded bar() calls — a clean, concrete demo of what mutation testing catches that a single overall-result assertion misses.

## State as of last check (all phases 1-6 independently re-verified by coordinator, not just trusted from agent reports)

- `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`, `gofmt -l .` all clean.
- 410 tests passing across 9 packages.
- No git commits made anywhere in this build — everything is untracked/unstaged in the repo. User has not asked for a commit yet.

## Two known limitations found during phase 6's final verification (not bugs, but worth knowing)

1. **Default full-scope `-mutate` against turango's own module is pathological**: `turango test -mutate=./example/...` from this repo, with default `-mutatescope=full`, took 17+ minutes because each mutant's `go test ./...` re-runs turango's *entire* module including its own end-to-end mutation-engine tests (which themselves spawn nested `go test` children) — every mutant blows the ~10s derived timeout and gets misclassified `Killed` via the (correct, documented) "timeout counts as Killed" rule. Not a defect in the timeout logic itself, just a bad interaction when dogfooding turango on its own repo. `example/README.md` recommends `-mutatescope=package` for this reason. Worth considering later: detect/warn when the target module IS turango's own module, or just always recommend package/impact scope for local dogfooding in docs.
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

1. Check task list (TaskList tool) for current status.
2. If phase 6 agent (`a5970b3a0d6ed4040`) already finished, there will be a task-notification in conversation history — look for it before re-dispatching anything.
3. Once phase 6 is confirmed done: mark task 6 complete, task 7 (historical-bug validation) becomes unblocked — that's genuinely separate future work, not urgent.
4. Nothing has been committed to git. Ask the user before committing.
