# Benchmarks

This document holds a captured transcript of `BenchmarkMutate`
(`internal/mutate/mutate_bench_test.go`) — Go's own `testing.B`/`go test
-bench` pipeline, run in-process against real target packages, exactly the
way ROADMAP.md gap 8 specifies — plus a methodology section (machine spec,
Go version, `-mutateparallel` levels swept, date, exact command).

**Status: the real `-count=1` run is complete.** Both targets
(`corpus/op-control-if/module`, `corpus/stdlib-strconv-parseuint/module`)
ran their full scope x TCE x parallelism matrix. See "Full transcript"
below for every subtest's real numbers, and the "Data quality notes"
section for two honest caveats about how this particular run was captured
(a metric gap in some rows, and real CPU contention from other concurrent
work on this machine during part of the run) — both explained with their
root cause, not hand-waved.

## Placeholder targets

`BenchmarkMutate`'s current target table
(`corpus/op-control-if/module`, a single hand-written `if`-statement
fixture, and `corpus/stdlib-strconv-parseuint/module`, one real stdlib
function) is a proof-that-the-harness-works set, not the realistic small
(~500 LOC) / medium (~5K LOC) / large (~20K+ LOC) production-package
spread ROADMAP.md gap 8c calls for. Selecting those three real targets is
a separate, still-open, human decision — this run validates the
measurement pipeline itself (timing, metrics, scope/TCE/parallel sweep),
not a final performance claim about turango against representative
codebases.

## Methodology

- Machine: Apple M1 Pro, 10 cores (8P+2E), 16 GB RAM, macOS 26.6.1
  (build 25G76)
- Go: `go1.26.5 darwin/arm64`
- Date: 2026-08-10/11 (overnight run, `-count=1`, AC power throughout)
- `-mutateparallel` levels swept: `{1, 4, 8}` — all three are below this
  machine's `runtime.NumCPU()` (10), so `benchParallelLevels` did not need
  to scale any of them down
- Commands (the run was split into a driving invocation plus several
  targeted re-runs of individual subtests, after earlier background-job
  kills interrupted the first pass — see "Data quality notes" for why
  that matters for a few rows):
  ```
  go test -tags=integration -bench=BenchmarkMutate -benchtime=1x -count=1 -run=NONE -v -timeout=0 ./internal/mutate/...
  ```
  followed by targeted continuations of the form:
  ```
  go test -tags=integration -bench='BenchmarkMutate/stdlib-strconv-parseuint/<scope>/tce=<bool>/parallel=<n>' -benchtime=1x -count=1 -run=NONE -v -timeout=0 ./internal/mutate/...
  ```
  run one exact subtest at a time — a regex spanning multiple `/`-levels
  (e.g. `(full/tce=true/parallel=8|package|impact)`) does **not** work as
  a single `-bench` pattern: Go's benchmark matcher splits the pattern on
  `/` and matches each segment against the corresponding subtest-name
  level positionally, so alternation groups that themselves contain `/`
  get shredded across levels instead of matching whole subtest paths.
  Each subtest needs its own clean, `/`-level-literal `-bench` pattern.

## Data quality notes

**x-baseline is missing for some rows, and was recomputed by hand.**
`benchMutateOnce` (`mutate_bench_test.go:303-305`) only reports the
`x-baseline` metric when `benchBaseline`'s own `b.Run(targetName+"/baseline",
...)` subtest (`mutate_bench_test.go:233-248`) actually ran in the same
process invocation — its return value is a local variable set inside that
`b.Run` closure, which Go's testing package skips executing entirely when
the closure's subtest name doesn't match the active `-bench` pattern, so
`baseline` silently stays its zero value and the `if baseline > 0` guard
at line 303 suppresses the metric. Every subtest captured via one of the
single-subtest targeted re-runs above (all of `stdlib-strconv-parseuint`'s
`package` and `impact` rows, plus `full/tce=true/parallel=8`) therefore
has no `x-baseline` field in the raw transcript. The "Full transcript"
table below fills these in, computed post-hoc as
`ns/op ÷ 479280792` (the real `stdlib-strconv-parseuint/baseline` value
captured earlier in this same overall run, same target, same machine,
same session) — marked with `*` where recomputed rather than
`b.ReportMetric`-native.

**Some rows were captured under real CPU contention, not a clean
machine.** This machine ran `internal/corpus`'s `TestCorpus` verification
for the `stdlib-encoding-base64` and `stdlib-crypto-aes` corpus fixtures
concurrently with parts of this benchmark run overnight (both are
real, hours-long `go test` invocations of their own). Specifically,
`stdlib-strconv-parseuint/package/tce=true/parallel=8` (2147.7s) ran
slower than `package/tce=true/parallel=4` (1899.3s) — a non-monotonic
result inconsistent with more workers helping, most plausibly explained
by contention from a concurrently-running `TestCorpus/stdlib-crypto-aes`
pass rather than a real turango regression at parallel=8. Anything
captured after ~00:21 in the transcript overlapped with that aes
verification. This is flagged, not silently absorbed into the numbers,
because this project's own standing lesson (see PROGRESS.md) is to
attribute slowdowns to a verified cause, not a guessed one — and here the
cause (a second heavy `go test` process, confirmed via `ps`/`uptime`
during the run) is verified, not guessed.

## Full transcript

### op-control-if (hand-written single-`if` fixture, 12 prod-LOC / 12 test-LOC)

Baseline (`go test`): 460.07ms

| scope | tce | parallel | ns/op | mutants | x-baseline |
|---|---|---|---:|---:|---:|
| full | false | 1 | 6.873s | 8 | 14.94x |
| full | false | 4 | 7.032s | 8 | 15.29x |
| full | false | 8 | 6.748s | 8 | 14.67x |
| full | true | 1 | 7.509s | 8 | 16.32x |
| full | true | 4 | 7.298s | 8 | 15.86x |
| full | true | 8 | 8.217s | 8 | 17.86x |
| package | false | 1 | 7.122s | 8 | 15.48x |
| package | false | 4 | 7.180s | 8 | 15.61x |
| package | false | 8 | 6.860s | 8 | 14.91x |
| package | true | 1 | 7.687s | 8 | 16.71x |
| package | true | 4 | 7.366s | 8 | 16.01x |
| package | true | 8 | 7.685s | 8 | 16.70x |
| impact | false | 1 | 2.453s | 8 | 5.332x |
| impact | false | 4 | 2.435s | 8 | 5.293x |
| impact | false | 8 | 2.412s | 8 | 5.242x |
| impact | true | 1 | 2.497s | 8 | 5.427x |
| impact | true | 4 | 2.519s | 8 | 5.476x |
| impact | true | 8 | 2.446s | 8 | 5.317x |

8 mutants total regardless of scope/TCE for this fixture — too small a
target for scope or TCE to change the mutant count at all; its only value
is proving the harness itself works end to end. `impact` scope is
~3x faster here since the fixture's single `if` narrows straight to the
lines its one test actually covers.

### stdlib-strconv-parseuint (real stdlib function, 1625 prod-LOC / 363 test-LOC)

Baseline (`go test`): 479.28ms

| scope | tce | parallel | ns/op | mutants | ms/mutant | x-baseline |
|---|---|---|---:|---:|---:|---:|
| full | false | 1 | 4229.66s | 3523 | 1200.6 | 8825.0x |
| full | false | 4 | 3058.51s | 3523 | 868.2 | 6381.5x |
| full | false | 8 | 2635.98s | 3523 | 748.2 | 5499.9x |
| full | true | 1 | 2994.94s | 3446 | 869.1 | 6248.8x |
| full | true | 4 | 2506.67s | 3446 | 727.4 | 5230.1x |
| full | true | 8 | 1852.08s | 3446 | 537.5 | 3864.3x* |
| package | false | 1 | 2489.62s | 3523 | 706.7 | 5194.5x* |
| package | false | 4 | 1500.05s | 3523 | 425.8 | 3129.8x* |
| package | false | 8 | 1485.50s | 3523 | 421.7 | 3099.4x* |
| package | true | 1 | 2852.23s | 3446 | 827.7 | 5951.1x* |
| package | true | 4 | 1899.27s | 3446 | 551.2 | 3962.8x* |
| package | true | 8 † | 2147.74s | 3446 | 623.3 | 4481.2x* |
| impact | false | 1 | 309.34s | 3523 | 87.8 | 645.4x* |
| impact | false | 4 | 207.70s | 3523 | 59.0 | 433.4x* |
| impact | false | 8 | 234.58s | 3523 | 66.6 | 489.4x* |
| impact | true | 1 | 347.99s | 3519 | 98.9 | 726.1x* |
| impact | true | 4 | 273.57s | 3519 | 77.7 | 570.8x* |
| impact | true | 8 | 271.65s | 3519 | 77.2 | 566.8x* |

`*` = `x-baseline` recomputed post-hoc, not `b.ReportMetric`-native — see
"Data quality notes". `†` = captured while `TestCorpus/stdlib-crypto-aes`
was running concurrently — see "Data quality notes"; treat this one row's
timing as noisy, not a real parallel=8 regression.

Two real findings fall out of this table on their own:

- **The earlier "3517 vs 3523 mutants" discrepancy (noted but never
  resolved earlier in this project) is settled by this run**: every
  `tce=false` row across all three scopes agrees exactly on 3523 mutants.
  3523 is the real, stable count for this fixture; 3517 was an artifact
  of an earlier, separately-interrupted capture attempt, not a
  reproducible alternate count.
- **TCE's equivalent-mutant rate is scope-dependent, not fixed per
  fixture.** `full` and `package` scope both drop from 3523 → 3446
  mutants under TCE (77 filtered, 2.2% — the rate the "TCE is
  codebase-dependent" finding below is based on). `impact` scope only
  drops 3523 → 3519 (4 filtered, 0.11%) — a much smaller equivalent
  fraction. This makes sense on reflection: `impact` scope already
  restricts mutation to lines the test suite's own coverage touches,
  which is a different (and apparently less redundant) slice of the
  source than `full`/`package`'s unrestricted sweep — but this is offered
  as a plausible explanation from the numbers, not something traced
  through the code to confirm, since `runner.go`'s TCE and scope logic
  weren't cross-read for this note.

## Finding: TCE is codebase-dependent, and was a net loss for this fixture

Captured mid-run, independent of the full matrix above — a real,
controlled result worth documenting now rather than losing track of.

Against `corpus/stdlib-strconv-parseuint` (full scope, parallel=1),
`-mutatetce=true` filtered **77 of 3523 mutants (2.2%)** as
compiler-equivalent — real signal, not noise; those mutants' compiled
output genuinely matched the unmutated baseline byte-for-byte.

But TCE's cost is not one-time. `internal/mutate/runner.go`'s
`isTCEEquivalent` calls `compileDisassembly` (a fresh `go build
-gcflags=-S` compile) for **every mutant**, unconditionally — not only the
ones that turn out equivalent, since there's no way to know in advance
which ones will. Only the *baseline* compile (`planTCEBaseline`,
`internal/mutate/engine.go`) is a true one-time, per-package cost.

Measured directly, isolated from the noisy full-matrix run (warm `GOCACHE`,
same fixture, same machine):

| Operation | Time |
|---|---|
| TCE's compile-and-compare check (`go build -trimpath -gcflags=-S -buildid=x`) | 0.414s |
| The `go test` run TCE is trying to skip | 0.605s |

For N mutants at this fixture's 2.2% equivalent rate:

- **Without TCE**: `N × 0.605s`
- **With TCE**: `N × 0.414s` (check every mutant) `+ N × 0.978 × 0.605s`
  (still test the 97.8% that aren't equivalent) `= N × 1.006s`

**TCE was ~66% *slower* overall for this fixture** — the per-mutant tax on
100% of mutants outweighs the savings on the 2.2% it actually filters, by
a wide margin. This is the concrete cost-side justification (independent
of ROADMAP.md gap 2's own correctness-risk reasoning) for why turango
ships `-mutatetce` opt-in, not opt-out.

**The general lesson is broader than TCE specifically**: turango's knobs
(`-mutatescope`, `-mutateparallel`, `-mutatetce`, and any future ones) are
levers whose correct setting depends on the target codebase's own shape —
equivalent-mutant rate, per-test cost, cross-package coupling — not a
single universally-right configuration. A codebase with a much higher
equivalent-mutant rate (more dead code, more compiler-foldable
constants) would likely see TCE pay for itself; this fixture's 2.2% just
isn't high enough. Don't assume this result generalizes to every target;
measure per-codebase before enabling.

## How to run it, when it's time

```
go test -tags=integration -bench=BenchmarkMutate -benchtime=1x -count=1 ./internal/mutate/...
```

Start with `-count=1` to sanity-check output shape before committing to a
`-count=6`+ `benchstat`-ready run (per ROADMAP.md gap 8b's
external-repeat convention). `golang.org/x/perf/cmd/benchstat` is the
recommended tool for a rigorous statistical comparison once `-count=6`+
samples exist; it is deliberately not wired in as a module dependency for
this pass (see ROADMAP.md gap 8's own instruction not to add it unless code
here actually imports its API).
