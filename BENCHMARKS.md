# Benchmarks

This document records one exploratory `BenchmarkMutate` run and its
methodology. Results are single observations (`-count=1`), not
comparison-grade estimates. Source revision and working-tree state were not
recorded, and the raw command output was not retained.

Both targets (`corpus/op-control-if/module` and
`corpus/stdlib-strconv-parseuint/module`) completed the scope × TCE ×
parallelism matrix. Data-quality limitations are documented below.

## Scope and limitations

Current targets are a single hand-written `if`-statement fixture and one
stdlib function. They validate benchmark mechanics but are not representative
of production codebases. Do not generalize performance results beyond these
targets.

## Methodology

- Machine: Apple M1 Pro, 10 cores (8P+2E), 16 GB RAM, macOS 26.6.1
  (build 25G76)
- Go: `go1.26.5 darwin/arm64`
- Date: 2026-08-10/11 (overnight run, `-count=1`, AC power throughout)
- `-mutateparallel` levels swept: `{1, 4, 8}` — all three are below this
  machine's `runtime.NumCPU()` (10), so `benchParallelLevels` did not need
  to scale any of them down
- Commands: the initial invocation was completed with targeted reruns. Each
  rerun used one fully specified sub-benchmark pattern because `go test
  -bench` matches slash-separated name segments independently.
  ```
  go test -tags=integration -bench=BenchmarkMutate -benchtime=1x -count=1 -run=NONE -v -timeout=0 ./internal/mutate/...
  ```
  followed by targeted continuations of the form:
  ```
  go test -tags=integration -bench='BenchmarkMutate/stdlib-strconv-parseuint/<scope>/tce=<bool>/parallel=<n>' -benchtime=1x -count=1 -run=NONE -v -timeout=0 ./internal/mutate/...
  ```
  A regex spanning multiple `/` levels does not match whole subtest paths;
  each continuation therefore selected one exact subtest.

## Data quality notes

Targeted reruns did not execute the baseline sub-benchmark, so no directly
comparable `x-baseline` value is available for those rows. Values derived from
a baseline captured in another invocation have been removed.

The row marked `†` overlapped with another CPU-intensive `go test` process.
Treat its timing as contaminated and exclude it from parallelism comparisons;
this run does not isolate or quantify the competing process's effect.

## Results

Line counts below are physical lines in `.go` files, including blanks and
comments.

### op-control-if (hand-written single-`if` fixture, 12 production / 12 test source lines)

Baseline (`go test`): 460.07ms

| scope | tce | parallel | time/op | mutants | x-baseline |
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

All configurations reported 8 mutants. In this single sample, impact scope
took about one-third the time of full/package scope; repeated runs are needed
to estimate the effect.

### stdlib-strconv-parseuint (stdlib fixture, 1625 production / 363 test source lines)

Baseline (`go test`): 479.28ms

| scope | tce | parallel | time/op | mutants | ms/mutant | x-baseline |
|---|---|---|---:|---:|---:|---:|
| full | false | 1 | 4229.66s | 3523 | 1200.6 | 8825.0x |
| full | false | 4 | 3058.51s | 3523 | 868.2 | 6381.5x |
| full | false | 8 | 2635.98s | 3523 | 748.2 | 5499.9x |
| full | true | 1 | 2994.94s | 3446 | 869.1 | 6248.8x |
| full | true | 4 | 2506.67s | 3446 | 727.4 | 5230.1x |
| full | true | 8 | 1852.08s | 3446 | 537.5 | — |
| package | false | 1 | 2489.62s | 3523 | 706.7 | — |
| package | false | 4 | 1500.05s | 3523 | 425.8 | — |
| package | false | 8 | 1485.50s | 3523 | 421.7 | — |
| package | true | 1 | 2852.23s | 3446 | 827.7 | — |
| package | true | 4 | 1899.27s | 3446 | 551.2 | — |
| package | true | 8 † | 2147.74s | 3446 | 623.3 | — |
| impact | false | 1 | 309.34s | 3523 | 87.8 | — |
| impact | false | 4 | 207.70s | 3523 | 59.0 | — |
| impact | false | 8 | 234.58s | 3523 | 66.6 | — |
| impact | true | 1 | 347.99s | 3519 | 98.9 | — |
| impact | true | 4 | 273.57s | 3519 | 77.7 | — |
| impact | true | 8 | 271.65s | 3519 | 77.2 | — |

`†` marks the contaminated row described in "Data quality notes."

With TCE disabled, all recorded configurations reported 3,523 mutants. With
TCE enabled, full/package scope reported 3,446 (77 filtered; 2.2%) and impact
scope reported 3,519 (4 filtered; 0.11%). This run did not investigate the
cause of the scope-dependent difference.

## Exploratory TCE cost model

Against `corpus/stdlib-strconv-parseuint` (full scope, parallel=1), TCE
classified 77 of 3,523 mutations (2.2%) as equivalent because their normalized
compiler disassembly matched the unmutated baseline.

For every uncached mutant that reaches TCE with a valid baseline,
`isTCEEquivalent` calls `compileDisassembly` before deciding whether to skip
the test run. Baseline compilation (`planTCEBaseline`) occurs once per package.

Single spot measurements used a warm `GOCACHE` on the same machine and
fixture. Exact commands were not retained, so these values support only an
exploratory cost model.

| Operation | Time |
|---|---|
| TCE's compile-and-compare check (`go build -trimpath -gcflags=-S -buildid=x`) | 0.414s |
| The `go test` run TCE is trying to skip | 0.605s |

For N mutants at this fixture's 2.2% equivalent rate:

- **Without TCE**: `N × 0.605s`
- **With TCE**: `N × 0.414s` (check every mutant) `+ N × 0.978 × 0.605s`
  (still test the 97.8% that aren't equivalent) `= N × 1.006s`

Using these point estimates, TCE predicts 1.006s per mutant versus 0.605s
without TCE. This estimate conflicts with the one-shot full-scope matrix,
where every TCE-enabled row was faster. Controlled repeated measurements are
required before drawing a performance conclusion. Together with false-positive
risk, this uncertainty supports keeping TCE opt-in pending representative
benchmarks.

TCE benefit depends on equivalent-mutant rate and relative compile/test costs.
Measure representative targets before enabling it.

## Reproducing the benchmark

```
go test -tags=integration -bench=BenchmarkMutate -benchtime=1x -count=1 -run=NONE -timeout=0 ./internal/mutate/...
```

Use `-count=1` for a smoke test. For comparisons, collect repeated samples
under controlled conditions and analyze them with
`golang.org/x/perf/cmd/benchstat`.
