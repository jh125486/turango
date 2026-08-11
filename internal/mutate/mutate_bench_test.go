//go:build integration

// BenchmarkMutate measures turango's mutation-testing overhead: how many
// multiples of one plain `go test` run a full mutation sweep costs, for a
// real target package, at each combination of scope, TCE and worker
// parallelism turango's own flags expose.
//
// This exists because PROPOSAL.md's "Costs and risks" section asserted the
// runtime cost qualitatively ("potentially orders of magnitude") without a
// single measured number behind it, and every timing data point elsewhere in
// this repo is incidental and non-comparable (PROGRESS.md's "17+ minutes"
// ScopeFull dogfooding note is explicitly a nested-`go test ./...` artifact,
// not a representative figure; the stdlib-crypto-aes sweep was never even
// successfully captured — see PROGRESS.md's corpus-provenance thread). See
// ROADMAP.md gap 8 for the full design this file implements.
//
// It uses Go's own testing.B/`go test -bench`/benchstat pipeline rather than
// a bespoke shell script — deliberately, since turango itself *is* a `go
// test` extension, so measuring it with the toolchain's own benchmark
// machinery is the same "eat our own dog food" instinct that already
// produced the corpus regression harness (TestCorpus) instead of a
// hand-rolled checker. Each mutate.Run call here is a full mutation sweep —
// seconds to minutes, not the microseconds testing.B is built to
// auto-calibrate b.N around — so this must always be run with
// -benchtime=1x, a real `go test` flag documented for exactly this
// "one call is already expensive" case (the same way the Go toolchain's own
// compiler benchmarks are run). Statistical stability then comes from the
// standard `go test -bench=BenchmarkMutate -benchtime=1x -count=N`
// external-repeat convention, not from looping inside this file.
//
// Gated behind the same "integration" build tag engine_integration_test.go
// already uses: every subtest shells out to (the baseline) or links against
// (mutate.Run itself, which shells out per mutant) the real Go toolchain
// against real package sources — exactly the reason that tag exists.
//
// Target selection (ROADMAP.md gap 8c) is explicitly left as an open, human
// decision there, not resolved by this file: benchTargets below is a
// deliberately small, easily-extended table populated *for now* with
// self-contained fixtures already checked into corpus/ purely to prove the
// harness itself works end to end. These are NOT the realistic
// small/medium/large KLOC-range spread gap 8c calls for — real target
// acquisition (permissively-licensed, complete, non-flaky packages spanning
// ~500/~5K/~20K+ LOC) is still unresolved. Do not read BENCHMARKS.md's
// numbers as anything but a placeholder proof-of-harness.
//
// Every target here is deliberately one of corpus/op-*/module or
// corpus/stdlib-*/module — each its own self-contained Go module with its
// own go.mod — rather than turango's own example/ package. example/ has no
// go.mod of its own, so mutating it in place resolves moduleDir to this
// entire repository: every mutant would then copy (or worktree-check-out)
// the whole turango module and run its whole `go test ./...` suite,
// multi-second per mutant even before mutation, and pathologically slow
// under [mutate.ScopeFull] specifically (see corpus/example/golden.json's
// own "full scope has a known pathological self-reference issue" note,
// and PROGRESS.md's paused corpus/aes/base64 thread for what that failure
// mode looks like in practice). A self-contained fixture module keeps every
// scope mode — including full — cheap regardless of target size.
package mutate_test

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/goproxy"
	"github.com/jh125486/turango/internal/mutate"
)

// benchTarget names one target package for BenchmarkMutate and where to find
// it. dir is repo-root-relative so the table stays readable and portable
// across checkouts; benchRepoRoot resolves it to an absolute path once per
// run.
//
// See this file's own doc comment for why every entry today is a
// self-contained corpus/ fixture module (its own go.mod) rather than
// turango's own example/ package, and PLACEHOLDER TARGETS below for why
// none of this is the real small/medium/large spread gap 8c still owes.
type benchTarget struct {
	// name is the subtest label. Kept short and free of "/" so it composes
	// cleanly with the scope/TCE/parallelism segments BenchmarkMutate
	// appends, and free of spaces so it survives -bench regexp filtering
	// (e.g. `-bench=BenchmarkMutate/op-control-if`) without quoting.
	name string

	// dir is the target module's directory, relative to the repository
	// root — e.g. "corpus/op-control-if/module".
	dir string
}

// benchTargets is PLACEHOLDER TARGETS ONLY (see the file doc comment and
// ROADMAP.md gap 8c): a small, easily-extended table, not the realistic
// small/~500-LOC/medium/~5K-LOC/large/~20K+-LOC production-package spread
// the design calls for. Picking those real targets is an open human
// decision this file does not resolve.
//
// Today's two entries are chosen only to exercise every axis
// (target x scope x TCE x parallelism) end to end at very different sizes,
// using fixtures already checked into corpus/ so nothing new is fetched
// from the network:
//
//   - op-control-if: corpus/op-control-if/module's golden.json pins exactly
//     8 mutants against a single 8-line function — about as small as a real
//     target gets, and fast enough to run the entire matrix in seconds.
//   - stdlib-strconv-parseuint: corpus/stdlib-strconv-parseuint/module's
//     golden.json pins 157 mutants against ~2 KLOC of frozen, real stdlib
//     `strconv` source (see PROPOSAL.md's evidence section) — still tiny
//     next to gap 8c's ~5K/~20K+ targets, but large enough (4 files) to show
//     the mutation multiplier growing with the mutant count and
//     -mutateparallel actually having files to spread across, unlike
//     op-control-if's single file. Its full target x scope x TCE x
//     parallelism matrix is considerably slower than op-control-if's (157
//     mutants vs. 8, each a real `go test` subprocess), so a captured run
//     against it may reasonably use -bench to narrow the matrix rather than
//     running every cell — see BENCHMARKS.md, once populated, for whatever
//     subset an actual run captures and why.
//
// Add a target by adding a row here: nothing else in this file changes.
var benchTargets = []benchTarget{
	{name: "op-control-if", dir: "corpus/op-control-if/module"},
	{name: "stdlib-strconv-parseuint", dir: "corpus/stdlib-strconv-parseuint/module"},
}

// benchScopes is every scope BenchmarkMutate sweeps, in the same
// cheapest-to-most-thorough order [mutate.Scope]'s own doc comment
// describes — full first, since it is the default a user gets without
// reaching for a flag, and the one gap 8's Problem section is specifically
// about quantifying.
var benchScopes = []mutate.Scope{mutate.ScopeFull, mutate.ScopePackage, mutate.ScopeImpact}

// benchParallelLevels returns the -mutateparallel values BenchmarkMutate
// sweeps: {1, 4, 8}, scaled down to the machine's actual runtime.NumCPU()
// wherever an entry would exceed it (so a smaller CI runner or laptop still
// gets a meaningful, duplicate-free sweep instead of three identical
// parallel=NumCPU rows). 1 is always included — the serial baseline every
// other level is compared against, matching ROADMAP.md gap 8a's "a
// serial-vs-parallel comparison is worth one extra data point" framing.
func benchParallelLevels() []int {
	n := runtime.NumCPU()

	var (
		out  []int
		seen = make(map[int]bool, 3)
	)

	for _, want := range []int{1, 4, 8} {
		v := min(want, n)
		if v < 1 {
			v = 1
		}

		if seen[v] {
			continue
		}

		seen[v] = true

		out = append(out, v)
	}

	return out
}

// BenchmarkMutate is the harness ROADMAP.md gap 8 asks for: one subtest per
// target x scope x TCE x parallelism combination, each running mutate.Run
// in-process against a real target package, plus one baseline subtest per
// target timing a single plain `go test` run the same way.
//
// -benchtime=1x is required (see the file doc comment): with it, b.N is 1
// for every subtest, so b.Elapsed() after the loop is exactly one mutation
// sweep's (or one baseline run's) wall-clock time — the same number
// reported as ns/op, and the number the mutation-multiplier metric divides.
func BenchmarkMutate(b *testing.B) {
	root := benchRepoRoot(b)

	goBin, err := goproxy.Resolve()
	if err != nil {
		b.Fatalf("goproxy.Resolve() error = %v", err)
	}

	parallelLevels := benchParallelLevels()

	for _, tgt := range benchTargets {
		dir := filepath.Join(root, tgt.dir)

		prodLOC, testLOC, err := sourceLines(dir)
		if err != nil {
			b.Fatalf("sourceLines(%s) error = %v", dir, err)
		}

		baseline := benchBaseline(b, tgt.name, goBin, dir)

		for _, scope := range benchScopes {
			for _, tce := range []bool{false, true} {
				for _, parallel := range parallelLevels {
					benchMutateOnce(b, tgt.name, dir, scope, tce, parallel, prodLOC, testLOC, baseline)
				}
			}
		}
	}
}

// benchBaseline times one plain `go test` run against dir — the "one
// baseline go test run" half of the mutation multiplier (ROADMAP.md gap
// 8a/8b) — and returns it for benchMutateOnce's later subtests to divide
// by.
//
// This is a sibling subtest that shells out directly, rather than reading
// the average [Result] already carries: it doesn't, today (see the file's
// companion ROADMAP.md gap 8 "Build order" step 1 — checked against
// report.go before writing this file, and the 3-run average
// [resolveTimeout]/goTestSuite computes internally is not exposed on
// [mutate.Result] or [mutate.MutantResult] anywhere). Adding a field for it
// would mean threading a new return value through resolveTimeout,
// baselineTimeout and Run for a number only this benchmark consumes;
// ROADMAP.md gap 8b explicitly allows either approach, and this is the
// smaller, lower-risk one for a first pass that leaves report.go's public
// API untouched. The command mirrors goTestSuite's own args (`-count=1
// -vet=off`) for an apples-to-apples comparison against what the engine
// itself times internally when deriving a run's default timeout.
func benchBaseline(b *testing.B, targetName, goBin, dir string) time.Duration {
	b.Helper()

	var baseline time.Duration

	b.Run(targetName+"/baseline", func(b *testing.B) {
		ctx := b.Context()

		for b.Loop() {
			//nolint:gosec // goBin is turango's own resolved toolchain, dir is a fixed corpus fixture path — neither is attacker-controlled input.
			cmd := exec.CommandContext(ctx, goBin, "test", "-count=1", "-vet=off", "./...")
			cmd.Dir = dir

			out, err := cmd.CombinedOutput()
			if err != nil {
				b.Fatalf("baseline go test in %s: %v\n%s", dir, err, out)
			}
		}

		baseline = b.Elapsed()
	})

	return baseline
}

// benchMutateOnce runs one target x scope x TCE x parallelism combination
// through mutate.Run and reports its metrics.
//
// TestTimeout is deliberately left at its zero value, the same choice
// TestRunAgainstRealModule (engine_integration_test.go) makes and for the
// same reason: leaving it zero exercises mutate.Run's derived-baseline
// timeout path end to end, which is what a real user gets by default
// (cmd/turango never sets -mutatetimeout unless asked), rather than
// measuring an artificially fixed budget no default run would actually use.
func benchMutateOnce(b *testing.B, targetName, dir string, scope mutate.Scope, tce bool, parallel, prodLOC, testLOC int, baseline time.Duration) {
	b.Helper()

	name := fmt.Sprintf("%s/%s/tce=%s/parallel=%s", targetName, scope, strconv.FormatBool(tce), strconv.Itoa(parallel))

	b.Run(name, func(b *testing.B) {
		ctx := b.Context()

		opts := mutate.Options{
			Packages: []string{"./..."},
			Dir:      dir,
			Scope:    scope,
			TCE:      tce,
			Parallel: parallel,
		}

		var result *mutate.Result

		for b.Loop() {
			var err error

			result, err = mutate.Run(ctx, opts)
			if err != nil {
				b.Fatalf("Run() error = %v", err)
			}
		}

		elapsed := b.Elapsed()

		b.ReportMetric(float64(prodLOC), "prod-loc")
		b.ReportMetric(float64(testLOC), "test-loc")
		b.ReportMetric(float64(len(result.Mutants)), "mutants")

		// The mutation multiplier itself — the number ROADMAP.md gap 8's
		// Problem section says is missing from PROPOSAL.md entirely: how
		// many multiples of one `go test` run this combination cost.
		// Guarded against a zero baseline only for safety (benchBaseline
		// always runs first and Fatalfs on a failing baseline, so baseline
		// is never legitimately zero by the time this line runs); reporting
		// nothing rather than a divide-by-zero Inf keeps a broken baseline
		// from producing a metric that looks like real data.
		if baseline > 0 {
			b.ReportMetric(elapsed.Seconds()/baseline.Seconds(), "x-baseline")
		}
	})
}

// sourceLines counts real lines of Go source under dir, split into
// production (non-_test.go) and test (_test.go) files — the KLOC/test-KLOC
// halves of ROADMAP.md gap 8a's per-target metadata.
//
// This is a plain newline count, not a gocloc-style code/comment/blank
// breakdown: gap 8a's actual requirement is "a go build/go vet-clean count,
// not a hand estimate" — i.e. a number computed from the real files a run
// actually mutates, not typed into this file by hand — which a line count
// already satisfies without adding a gocloc-equivalent dependency this
// project's stdlib-plus-x/-only policy (see PROGRESS.md's dependency
// cleanup note) would need to justify.
func sourceLines(dir string) (prodLOC, testLOC int, err error) {
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		n, err := countFileLines(path)
		if err != nil {
			return err
		}

		if strings.HasSuffix(path, "_test.go") {
			testLOC += n
		} else {
			prodLOC += n
		}

		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}

	return prodLOC, testLOC, nil
}

// countFileLines counts path's newlines the same way `wc -l` does: one per
// scanned line, regardless of its content.
func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var n int

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		n++
	}

	return n, scanner.Err()
}

// benchRepoRoot resolves the repository root from the test binary's working
// directory (internal/mutate, under `go test`'s default behaviour) by
// walking upward to the directory holding go.mod — the same approach
// internal/corpus/corpus_test.go's repoRoot takes (and the same rationale:
// this keeps working regardless of how deep internal/mutate ends up nested
// or how the test binary is invoked, rather than hard-coding "../..").
// Duplicated here rather than exported from internal/corpus because that
// package's repoRoot takes a *testing.T, not the *testing.B this file needs,
// and promoting a two-line helper to shared exported API for one caller
// isn't worth the churn.
func benchRepoRoot(b *testing.B) string {
	b.Helper()

	wd, err := os.Getwd()
	if err != nil {
		b.Fatalf("Getwd() error = %v", err)
	}

	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			b.Fatalf("benchRepoRoot: no go.mod found above %s", wd)
		}

		dir = parent
	}
}
