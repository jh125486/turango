# Benchmarks

This document will hold a captured transcript of `BenchmarkMutate`
(`internal/mutate/mutate_bench_test.go`) — Go's own `testing.B`/`go test
-bench` pipeline, run in-process against real target packages, exactly the
way ROADMAP.md gap 8 specifies — plus a short methodology section (machine
spec, Go version, `-mutateparallel` levels swept, date, exact command).

**Benchmarks intentionally not yet run.** The harness (`BenchmarkMutate`
and its supporting code in `internal/mutate/mutate_bench_test.go`) is
finished, builds clean, and is verified by inspection to be logically
sound, but no real mutation sweep has been executed for timing purposes as
part of writing this harness. Actually running it and capturing numbers
here is deliberately deferred to just before proposal submission — run on
a plugged-in machine, not on battery (CPU throttling/power-state skew on
battery power makes timing numbers untrustworthy), as the last step before
`PROPOSAL.md` goes out, not mid-development. When that run happens, this
document gets:

- A "Placeholder targets" section stating plainly that `BenchmarkMutate`'s
  current target table (`corpus/op-control-if/module` and
  `corpus/stdlib-strconv-parseuint/module`) is a proof-that-the-harness-works
  set, not the realistic small (~500 LOC) / medium (~5K LOC) / large
  (~20K+ LOC) production-package spread ROADMAP.md gap 8c calls for — gap
  8c's real target selection is a separate, still-open, human decision.
- A methodology section: machine spec, Go version, the `-mutateparallel`
  levels swept (`{1, 4, 8}`, scaled down to the run machine's
  `runtime.NumCPU()` if smaller — see `benchParallelLevels` in
  `mutate_bench_test.go`), date, and the exact `go test -bench` command(s)
  used.
- The captured `go test -bench` transcript itself, pasted verbatim (not
  hand-typed), the same "real re-run, not hand-edited" standard
  `example/README.md` already holds itself to.

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
