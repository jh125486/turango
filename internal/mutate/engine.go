package mutate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/packages"

	"github.com/jh125486/turango/internal/goproxy"
	"github.com/jh125486/turango/internal/mutator"

	// Importing the operator packages for their side effect is what populates
	// the mutator registry; mutator.All() is empty without them.
	_ "github.com/jh125486/turango/internal/mutator/control"
	_ "github.com/jh125486/turango/internal/mutator/expression"
	_ "github.com/jh125486/turango/internal/mutator/identifier"
	_ "github.com/jh125486/turango/internal/mutator/literal"
	_ "github.com/jh125486/turango/internal/mutator/operator"
	_ "github.com/jh125486/turango/internal/mutator/statement"
)

// unknownSpelling is the fallback String() spelling for a Scope, Workspace
// or Status value outside its defined range — reachable only via an
// explicit invalid conversion (e.g. Scope(99)), never through ParseScope/
// ParseWorkspace or the classifier, both of which only ever produce a
// defined value.
const unknownSpelling = "unknown"

// Scope selects which tests are run to decide whether a mutant was caught.
//
// The three modes trade run time against confidence, and they can disagree: a
// mutant only a neighbouring package's tests exercise is killed under
// [ScopeFull] and survives under [ScopePackage]. Narrower scopes never produce
// *more* kills than wider ones, so a narrow scope's score is a lower bound on
// the full one.
type Scope int

const (
	// ScopeFull runs the whole module's tests (`go test ./...`) against every
	// mutant. It is the default because it is the only scope that cannot miss a
	// kill: a package's behaviour is frequently only asserted on by its
	// callers' tests, and those live in other packages.
	ScopeFull Scope = iota

	// ScopePackage runs only the tests of the package holding the mutated file.
	// Much cheaper than [ScopeFull] on a large module, at the cost of reporting
	// cross-package kills as survivors.
	ScopePackage

	// ScopeImpact runs only the tests that actually execute the mutated line,
	// derived from a per-test coverage map built once per package before any
	// mutant runs (see impact.go). A line no test covers is not tested at all,
	// so its mutants are reported as survived without running anything.
	ScopeImpact
)

// String reports the scope's -mutatescope spelling.
func (s Scope) String() string {
	switch s {
	case ScopeFull:
		return "full"
	case ScopePackage:
		return "package"
	case ScopeImpact:
		return "impact"
	default:
		return unknownSpelling
	}
}

// ParseScope converts a -mutatescope flag value to a [Scope].
//
// It lives beside the type rather than in the command so that any caller
// constructing [Options] from user input — the CLI today, a config file later —
// agrees on the spellings.
func ParseScope(s string) (Scope, error) {
	switch s {
	case "full":
		return ScopeFull, nil
	case "package":
		return ScopePackage, nil
	case "impact":
		return ScopeImpact, nil
	default:
		return ScopeFull, fmt.Errorf("mutate: unknown scope %q (want full, package or impact)", s)
	}
}

// Workspace selects how a mutant's throwaway execution copy of the module is
// built. See runner.go's copyModule/copyWorktree for the two strategies.
type Workspace int

const (
	// WorkspaceCopy recursively copies the module into a fresh temp directory
	// per mutant. It has no dependency on git and works against any module,
	// git-tracked or not — the default, and the only strategy available
	// before ROADMAP.md gap 6.
	WorkspaceCopy Workspace = iota

	// WorkspaceWorktree uses `git worktree add` instead of a filesystem copy.
	// Strictly opt-in and never a hard requirement: it is only ever attempted
	// when the target module is inside a clean git working tree (see
	// runner.go's gitWorktreeClean), falling back to [WorkspaceCopy]
	// automatically otherwise — so requesting it is always safe, even
	// against a directory (a corpus fixture's own module/, say) that turns
	// out not to be a clean git checkout, or not a git repo at all.
	WorkspaceWorktree
)

// String reports the workspace's -mutateworkspace spelling.
func (w Workspace) String() string {
	switch w {
	case WorkspaceCopy:
		return "copy"
	case WorkspaceWorktree:
		return "worktree"
	default:
		return unknownSpelling
	}
}

// ParseWorkspace converts a -mutateworkspace flag value to a [Workspace].
func ParseWorkspace(s string) (Workspace, error) {
	switch s {
	case "copy":
		return WorkspaceCopy, nil
	case "worktree":
		return WorkspaceWorktree, nil
	default:
		return WorkspaceCopy, fmt.Errorf("mutate: unknown workspace %q (want copy or worktree)", s)
	}
}

// Options configures a mutation run.
//
// Every field has a usable zero value: the zero Options mutates the package in
// the working directory, with every registered operator, at [ScopeFull], one
// file at a time, with a derived per-mutant timeout.
type Options struct {
	// Packages holds the package patterns to mutate, in `go test` syntax
	// ("./...", "./internal/...", an import path). Empty means "." — the
	// package in Dir.
	//
	// This is package *selection*, deliberately kept separate from
	// FuncPattern below — the same separation -run/-bench/-fuzz all have
	// between "which package(s)" (their trailing positional args) and
	// "which named target within them" (the flag's own regexp value).
	Packages []string

	// Dir is the directory patterns are resolved relative to. Empty means the
	// process working directory.
	Dir string

	// FuncPattern is a regular expression matched against the name of every
	// top-level function and method declaration in the selected packages.
	// Only functions whose name matches — and everything nested in their
	// bodies — are mutated. Package-level declarations outside any function
	// (a var/const block, say) are not "in" a function for this pattern to
	// match against, so this filter never affects them.
	//
	// Empty means every function matches: Go's regexp package treats an
	// empty pattern as matching everywhere, so the zero value naturally
	// means "no narrowing," the same convention Packages' own zero value
	// uses for package selection.
	FuncPattern string

	// Operators names the mutation operators to apply, using their registry
	// names ("operator/binary", "control/if"). Empty means every registered
	// operator. An unknown name fails the run rather than being skipped: a
	// typo'd operator that silently reduced the mutant set would quietly
	// inflate the score.
	Operators []string

	// Scope selects the tests each mutant is judged by. The zero value is
	// [ScopeFull].
	Scope Scope

	// Parallel bounds how many *files* are mutated concurrently. Zero or less
	// means one.
	//
	// The unit is a file, not a mutant, and that is a correctness constraint
	// rather than a tuning choice: every mutant of a file shares that file's
	// AST, which the runner mutates in place and reverts, so two mutants of one
	// file can never run at the same time. Different files hold entirely
	// separate trees.
	Parallel int

	// TestTimeout bounds each individual mutant's `go test` run. A bound is not
	// optional: mutation is very good at producing infinite loops — turning an
	// `i++` into an `i--` hangs the suite forever — and without one such a
	// mutant stalls the whole run until `go test`'s own 10-minute default
	// fires.
	//
	// Zero means "derive it": the engine times the unmutated suite before
	// mutating anything and scales that (see [baselineTimeout]). Set it
	// explicitly to skip the baseline entirely, which is what phase 5's
	// -mutatetimeout flag does.
	TestTimeout time.Duration

	// MutantID replays exactly one mutant by its [MutantResult.ID] instead of
	// running every mutation the selected operators offer. Empty means "every
	// mutant," the ordinary run.
	//
	// The walk still visits every file and node — that part is a cheap AST
	// walk regardless — but a mutation whose computed ID does not match this
	// one is never handed to the runner, so the resulting [Result] holds at
	// most one [MutantResult].
	MutantID string

	// TCE enables Trivial Compiler Equivalence: a mutant whose compiled
	// output exactly matches a per-package baseline (see [planPackage]) is
	// filtered before it reaches the test suite, and reported under
	// [Result.Equivalents] instead of [Result.Mutants]. Off by default —
	// the zero value matches every other Options field's "safe default"
	// philosophy, and unlike a narrower scope (which can only under-report
	// kills, never mis-report one), a false positive in the compiled-output
	// comparison would silently discard a real mutant. See ROADMAP.md gap
	// 2 for the validated design and its spike.
	TCE bool

	// Workspace selects how each mutant's throwaway execution copy is built.
	// The zero value is [WorkspaceCopy] — today's existing filesystem-copy
	// behaviour, unchanged. See ROADMAP.md gap 6.
	Workspace Workspace
}

// baselineRuns is how many times the unmutated suite is timed before a run.
//
// Three is enough to damp the first run's cold build cache without turning the
// measurement into a meaningful fraction of the mutation run itself.
const baselineRuns = 3

// MinBaselineTimeout is the floor applied to a derived timeout.
//
// The baseline measures a suite whose build cache is already warm, so it barely
// pays for compilation — but every mutant recompiles its package from changed
// source, and that cost is not in the measurement. On a fast, small suite the
// derived product can land below the time a single mutant needs just to build,
// which would report perfectly good mutants as killed-by-timeout. The floor
// only ever raises a derived value; it never lowers one, and it never applies
// to an explicit [Options.TestTimeout].
const MinBaselineTimeout = 10 * time.Second

// Run executes a full mutation run and reports every mutant it produced.
//
// Files are mutated concurrently, up to [Options.Parallel] at a time, so the
// order in which results *arrive* is not deterministic. The report is: Run
// sorts [Result.Mutants] and [Result.Suppressions] by file, line, operator and
// description before returning, so two runs over unchanged sources still
// produce identical reports regardless of scheduling. Within one file the walk
// is strictly sequential — a file's mutants share one AST that is mutated in
// place and reverted between mutants, so the whole run reuses one parse per
// file.
//
// Run is cancellation-aware between mutants: when ctx is done, every in-flight
// file stops at its next mutant rather than finishing its walk, and Run returns
// the mutants completed so far together with ctx.Err(), so a caller
// interrupting a long run still gets a partial report rather than nothing.
func Run(ctx context.Context, opts Options) (*Result, error) {
	goBin, err := goproxy.Resolve()
	if err != nil {
		return nil, err
	}

	mutators, err := opts.mutators()
	if err != nil {
		return nil, err
	}

	funcPattern, err := regexp.Compile(opts.FuncPattern)
	if err != nil {
		return nil, fmt.Errorf("mutate: func pattern %q: %w", opts.FuncPattern, err)
	}

	pkgs, err := load(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Type information is only ever resolved when the selected operator set
	// actually needs it (see needsTypes): the vastly more common run, which
	// selects only purely-syntactic operators, pays nothing extra here.
	var typedPkgs map[string]*packages.Package

	if needsTypes(mutators) {
		typedPkgs, err = loadTyped(ctx, opts)
		if err != nil {
			return nil, err
		}
	}

	// Dependency-closure resolution (ROADMAP.md gap 5) is only ever
	// attempted when the run's own requested scope is not [ScopeFull]: a
	// forward import closure is provably the wrong thing to test under
	// ScopeFull (it cannot see the reverse closure ScopeFull's cross-package
	// kill detection depends on — see closureDirs' doc comment), so a
	// ScopeFull run pays nothing extra here, the same zero-cost-by-default
	// shape needsTypes already gives typed operators. A failure loading
	// test-aware packages is deliberately non-fatal: this is a pure
	// optimisation over the always-correct copyModule fallback, so a
	// load error here just means no package gets a narrower workspace this
	// run, not a failed run.
	var closurePkgs map[string][]*packages.Package

	if opts.Scope != ScopeFull {
		closurePkgs, _ = loadClosures(ctx, opts)
	}

	timeout, err := resolveTimeout(ctx, opts, goTestSuite(goBin, opts.Dir, opts.patterns()))
	if err != nil {
		return nil, err
	}

	run := &runner{goBin: goBin, testTimeout: timeout, workspace: opts.Workspace}

	jobs, err := plan(ctx, goBin, opts, pkgs, mutators, typedPkgs, closurePkgs, funcPattern, false)
	if err != nil {
		return &Result{}, err
	}

	sink := newCollector()
	runErr := execute(ctx, run, jobs, opts.parallel(), sink, nil)

	return sink.close(), runErr
}

// Estimate performs a walk-only preview of what -mutate would produce: how
// many mutants a real [Run] would generate, broken down per package, and a
// rough, honestly-hedged prediction of how long running them would take —
// without ever writing a mutation to disk or spawning a single `go test`
// subprocess to classify one. See ROADMAP.md gap 11 for the full design and
// the reasoning behind every caveat [EstimateResult] carries.
//
// It is a separate entry point from Run, not an [Options] flag, deliberately
// — see gap 11a: the two results answer structurally different questions.
// Run's *Result carries Status/Output fields a mutant that was never
// executed could never populate honestly (a zero Status would even print as
// "killed", the iota's zero value); [EstimateResult]'s fields — a count, a
// single timing sample — are exactly what an unexecuted walk actually knows
// and nothing it doesn't.
//
// Estimate reuses exactly the same package/operator/type resolution and AST
// walk Run does — load, needsTypes/loadTyped, plan/planPackage, mutateFile/
// visitNode — via [walkForEstimate]. The only behavioural difference is one
// branch inside visitNode's per-mutation loop (guarded by a non-nil tally)
// that tallies a package's count instead of calling [runner.run]. Dependency-
// closure resolution (ROADMAP.md gap 5) and per-package coverage maps
// (ScopeImpact) are both execution-time concerns with nothing to contribute
// to a count, so planPackage skips building them for an estimate-only job.
//
// Per-package baseline timing (ROADMAP.md gap 11b) intentionally runs
// *after* the count-only walk finishes, not precomputed alongside it the way
// ScopeImpact's coverage map or TCE's baseline compile are for a real run:
// only a package that the walk actually found at least one mutant in is
// worth timing at all, and that is only known once the walk is done.
func Estimate(ctx context.Context, opts Options) (*EstimateResult, error) {
	goBin, err := goproxy.Resolve()
	if err != nil {
		return nil, err
	}

	counts, err := walkForEstimate(ctx, goBin, opts)
	if err != nil {
		return nil, err
	}

	return buildEstimateResult(ctx, goBin, opts, counts), nil
}

// walkForEstimate performs [Estimate]'s counting phase alone: resolve
// mutators and packages exactly as [Run] does, then reuse plan/execute/
// mutateFile/visitNode with a non-nil tally so visitNode's estimate branch
// tallies every matching mutation instead of calling [runner.run].
//
// Split out from Estimate specifically so a test can assert this phase alone
// spawns zero `go test`/`go build` subprocesses (see execCalls in
// runner.go), independent of [buildEstimateResult]'s per-package baseline
// timing, which deliberately does spawn them — the same "prove the cheap
// part is actually cheap" technique [loadTypedCalls] already established for
// the identifier/constswap typed-operator gate.
func walkForEstimate(ctx context.Context, goBin string, opts Options) (estimateCounts, error) {
	mutators, err := opts.mutators()
	if err != nil {
		return estimateCounts{}, err
	}

	funcPattern, err := regexp.Compile(opts.FuncPattern)
	if err != nil {
		return estimateCounts{}, fmt.Errorf("mutate: func pattern %q: %w", opts.FuncPattern, err)
	}

	pkgs, err := load(ctx, opts)
	if err != nil {
		return estimateCounts{}, err
	}

	// Type information is resolved under the identical zero-cost-unless-needed
	// gate Run() uses — an estimate must select the same TypedMutator-bound
	// mutations a real run would, or its count would not match Run()'s.
	var typedPkgs map[string]*packages.Package

	if needsTypes(mutators) {
		typedPkgs, err = loadTyped(ctx, opts)
		if err != nil {
			return estimateCounts{}, err
		}
	}

	// closurePkgs is deliberately nil: dependency-closure resolution only
	// ever affects how a mutant's *execution* workspace is built
	// (runner.workspaceFor), never which mutations the walk finds, so
	// resolving it here would cost real time for zero effect on the count.
	jobs, err := plan(ctx, goBin, opts, pkgs, mutators, typedPkgs, nil, funcPattern, true)
	if err != nil {
		return estimateCounts{}, err
	}

	tally := newEstimateTally()
	walkErr := execute(ctx, nil, jobs, opts.parallel(), nil, tally)
	counts := tally.close()

	return counts, walkErr
}

// buildEstimateResult times each package the walk found a mutant in and
// extrapolates a total, per ROADMAP.md gap 11b/11c.
//
// Under [ScopeFull] every mutant really does run `go test ./...`
// (mutant.testArgs), so one whole-module sample applies identically to
// every package; it is measured once, not per package, and only if the walk
// found at least one mutant anywhere (nothing to time otherwise). Under a
// narrower scope, a whole-module baseline would systematically overestimate
// every package's real per-mutant cost — see gap 11b — so each package gets
// its own single-sample timing instead, scoped to just its own pattern, via
// [packageBaseline].
//
// Every timing here is a single sample, not [baselineRuns]' three-run
// average a real run uses to derive its timeout: cold-cache variance alone
// was measured at roughly 5x for an identical invocation (0.75s warm vs.
// 3.95s cold GOCACHE) during this gap's own validation, so treat every
// [PackageEstimate.Baseline] as a rough number, not an authoritative one —
// spending 3x the setup time for a steadier average would work directly
// against this feature's whole point of being fast to check before
// committing to the real run.
func buildEstimateResult(ctx context.Context, goBin string, opts Options, counts estimateCounts) *EstimateResult {
	result := &EstimateResult{
		Total:   counts.total,
		Workers: opts.parallel(),
		TCE:     opts.TCE,
	}

	var fullBaseline time.Duration

	if opts.Scope == ScopeFull && counts.total > 0 {
		fullBaseline, _ = goTestSuite(goBin, opts.Dir, opts.patterns())(ctx)
	}

	for _, pkgPath := range counts.order {
		h := counts.hits[pkgPath]

		baseline := fullBaseline
		if opts.Scope != ScopeFull {
			baseline = packageBaseline(ctx, goBin, h.moduleDir, h.pkgDir)
		}

		result.Packages = append(result.Packages, PackageEstimate{
			Package:  pkgPath,
			Mutants:  h.count,
			Baseline: baseline,
		})

		result.SerialEstimate += time.Duration(h.count) * baseline
	}

	if result.Workers > 0 {
		result.ParallelEstimate = result.SerialEstimate / time.Duration(result.Workers)
	}

	return result
}

// packageBaseline times one sample of pkgDir's own tests, scoped to just
// that package's pattern — the same [mutant.testArgs] shape a real mutant's
// `go test` invocation uses under [ScopePackage]/[ScopeImpact]. A pattern or
// timing failure reports a zero baseline rather than failing the whole
// estimate: a rough number for every other package is more useful than
// none at all over one package's toolchain hiccup, matching the fail-soft
// precedent [planScope] and [planTCEBaseline] already set for per-package
// precomputes that are optimisations, not correctness requirements.
func packageBaseline(ctx context.Context, goBin, moduleDir, pkgDir string) time.Duration {
	pattern, err := packagePattern(moduleDir, pkgDir)
	if err != nil {
		return 0
	}

	d, err := goTestSuite(goBin, moduleDir, []string{pattern})(ctx)
	if err != nil {
		return 0
	}

	return d
}

// estimateHit is one matching mutation [Estimate]'s walk found, carrying
// just enough for [buildEstimateResult] to later time a per-package baseline
// sample against the right pattern.
type estimateHit struct {
	pkgPath, moduleDir, pkgDir string
}

// pkgHits is one package's running tally plus the location
// [buildEstimateResult] needs to time its own baseline sample under a scope
// narrower than [ScopeFull].
type pkgHits struct {
	moduleDir, pkgDir string
	count             int
}

// estimateCounts is [estimateTally]'s final, aggregated result: how many
// matching mutations the walk found for each package.
type estimateCounts struct {
	total int

	// order preserves each package's first-seen position during the walk —
	// a pkgPath is an import path, not otherwise a meaningful sort key, so
	// insertion order is what [EstimateResult.Packages] is reported in.
	order []string
	hits  map[string]*pkgHits
}

// estimateTally aggregates every file worker's walk-only mutation count into
// one per-package [estimateCounts], via a single consumer goroutine draining
// one channel — the same channel-not-mutex shape [collector] uses, for the
// identical testing/synctest reason documented on collector's own doc
// comment. It is [Estimate]'s stand-in for collector: where collector
// records a verdict per mutant, estimateTally only ever records that a
// matching mutation exists, and never touches [runner.run] to find out —
// see visitNode's tally branch.
type estimateTally struct {
	hits chan estimateHit
	done chan estimateCounts
}

// newEstimateTally starts the consumer goroutine and returns a tally ready
// to receive hits. The caller must call close exactly once, after every
// producer goroutine that might call count has finished — the same contract
// [newCollector] documents for its own close.
func newEstimateTally() *estimateTally {
	t := &estimateTally{
		hits: make(chan estimateHit),
		done: make(chan estimateCounts),
	}

	go t.consume()

	return t
}

// count records one matching mutation for pkgPath. Blocks until the
// consumer goroutine receives it, the same effective backpressure
// [collector.mutant] provides.
func (t *estimateTally) count(pkgPath, moduleDir, pkgDir string) {
	t.hits <- estimateHit{pkgPath: pkgPath, moduleDir: moduleDir, pkgDir: pkgDir}
}

// consume drains hits until the channel is closed, accumulating into one
// estimateCounts, then publishes it on done.
func (t *estimateTally) consume() {
	result := estimateCounts{hits: map[string]*pkgHits{}}

	for hit := range t.hits {
		h, ok := result.hits[hit.pkgPath]
		if !ok {
			h = &pkgHits{moduleDir: hit.moduleDir, pkgDir: hit.pkgDir}
			result.hits[hit.pkgPath] = h
			result.order = append(result.order, hit.pkgPath)
		}

		h.count++
		result.total++
	}

	t.done <- result
}

// close signals that no more hits will be sent, and blocks until the
// consumer goroutine has finished aggregating, returning the final
// estimateCounts. Must be called exactly once, after every producer
// goroutine has already returned.
func (t *estimateTally) close() estimateCounts {
	close(t.hits)

	return <-t.done
}

// execute drives the file workers, bounded at parallel files in flight.
//
// The group's context is derived from ctx, so the first failing file cancels
// the rest instead of leaving a doomed run to grind through every remaining
// mutant — each of which costs a full `go test`.
//
// Exactly one of sink/tally is non-nil: a real run passes sink and a nil
// tally; [walkForEstimate] passes a nil sink, a nil run, and a non-nil
// tally — see [visitNode]'s branch on tally, the only place either is
// actually read.
func execute(ctx context.Context, run *runner, jobs []fileJob, parallel int, sink *collector, tally *estimateTally) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(parallel)

	for i := range jobs {
		job := &jobs[i]

		group.Go(func() error {
			return mutateFile(groupCtx, run, job, sink, tally)
		})
	}

	return group.Wait()
}

// collector serialises the file workers' appends into one Result via a
// single consumer goroutine draining three channels, one per result kind,
// rather than a mutex-guarded shared slice.
//
// Channels, not a mutex, specifically because of testing/synctest: its
// bubble's "durably blocked" detection — the thing that lets a test
// deterministically drive concurrent code without real wall-clock waits —
// covers channel send/receive, select, sync.Cond.Wait, sync.WaitGroup.Wait
// and time.Sleep (see `go doc testing/synctest`). sync.Mutex.Lock is not on
// that list: a goroutine blocked acquiring a mutex is invisible to a
// synctest bubble. This was a correctness-neutral change when made (the
// mutex was already race-safe, verified under -race) — see ROADMAP.md gap 7
// — made purely so a future test of -mutateparallel's scheduling behavior
// has something to drive deterministically.
//
// Results are accumulated unordered and sorted once at the end rather than
// being slotted into a pre-sized per-file position: how many mutants a file
// yields is only known once its walk is finished.
type collector struct {
	mutants      chan MutantResult
	suppressions chan SuppressionResult
	equivalents  chan EquivalentResult

	// done carries the final, sorted Result from consume to close, once
	// every channel above has been closed and drained.
	done chan *Result
}

// newCollector starts the consumer goroutine and returns a collector ready
// to receive results. The caller must call close exactly once, after every
// producer goroutine that might call mutant/suppression/equivalent has
// finished.
func newCollector() *collector {
	c := &collector{
		mutants:      make(chan MutantResult),
		suppressions: make(chan SuppressionResult),
		equivalents:  make(chan EquivalentResult),
		done:         make(chan *Result),
	}

	go c.consume()

	return c
}

// mutant records one mutant's verdict. Blocks until the consumer goroutine
// receives it — the same effective backpressure a mutex's critical section
// provided, just expressed as a channel send.
func (c *collector) mutant(res MutantResult) { c.mutants <- res }

// suppression records one //nomutant hit.
func (c *collector) suppression(res SuppressionResult) { c.suppressions <- res }

// equivalent records one mutation Trivial Compiler Equivalence filtered out.
func (c *collector) equivalent(res EquivalentResult) { c.equivalents <- res }

// consume drains all three channels until every one is closed, accumulating
// into one Result, then sorts it and publishes it on done. It is the sole
// owner of the Result being built — nothing else touches it — so no locking
// is needed here either.
func (c *collector) consume() {
	result := &Result{}

	mutants, suppressions, equivalents := c.mutants, c.suppressions, c.equivalents

	for mutants != nil || suppressions != nil || equivalents != nil {
		select {
		case m, ok := <-mutants:
			if !ok {
				mutants = nil // a nil channel blocks forever in select, removing this case

				continue
			}

			result.Mutants = append(result.Mutants, m)
		case s, ok := <-suppressions:
			if !ok {
				suppressions = nil

				continue
			}

			result.Suppressions = append(result.Suppressions, s)
		case e, ok := <-equivalents:
			if !ok {
				equivalents = nil

				continue
			}

			result.Equivalents = append(result.Equivalents, e)
		}
	}

	sortResult(result)
	c.done <- result
}

// close signals that no more results will be sent, and blocks until the
// consumer goroutine has finished sorting, returning the final Result. It
// must be called exactly once, after every producer goroutine has already
// returned — calling it earlier would race the consumer's close-detection
// against a producer still mid-send.
func (c *collector) close() *Result {
	close(c.mutants)
	close(c.suppressions)
	close(c.equivalents)

	return <-c.done
}

// sortResult restores the deterministic report order documented on [Result]
// itself: file, then line, then operator, then description (suppressions
// have no operator/description to break a tie on, so file/line is as far as
// their ordering goes).
func sortResult(result *Result) {
	sort.SliceStable(result.Mutants, func(i, j int) bool {
		a, b := result.Mutants[i], result.Mutants[j]

		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Operator != b.Operator:
			return a.Operator < b.Operator
		default:
			return a.Description < b.Description
		}
	})

	sort.SliceStable(result.Suppressions, func(i, j int) bool {
		a, b := result.Suppressions[i], result.Suppressions[j]

		if a.File != b.File {
			return a.File < b.File
		}

		return a.Line < b.Line
	})

	sort.SliceStable(result.Equivalents, func(i, j int) bool {
		a, b := result.Equivalents[i], result.Equivalents[j]

		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Operator != b.Operator:
			return a.Operator < b.Operator
		default:
			return a.Description < b.Description
		}
	})
}

// suiteTimer runs the unmutated test suite once and reports how long it took.
//
// It exists as a function type so the baseline arithmetic can be tested without
// shelling out to a toolchain three times.
type suiteTimer func(ctx context.Context) (time.Duration, error)

// resolveTimeout decides the per-mutant timeout for a run.
//
// An explicit [Options.TestTimeout] wins and short-circuits: measuring a
// baseline nobody is going to use would add three full suite runs to the front
// of the run for nothing.
func resolveTimeout(ctx context.Context, opts Options, timeSuite suiteTimer) (time.Duration, error) {
	if opts.TestTimeout > 0 {
		return opts.TestTimeout, nil
	}

	return baselineTimeout(ctx, baselineRuns, runtime.NumCPU(), timeSuite)
}

// baselineTimeout times the unmutated suite runs times and derives a per-mutant
// budget from the mean.
//
// The mean is multiplied by the CPU count. That looks generous for a sequential
// run, and it is — the multiplier is there because mutants are meant to run
// concurrently (phase 5's -mutateparallel), and a suite sharing a machine with
// NumCPU other suites takes correspondingly longer in wall-clock terms. Deriving
// the same number in both modes keeps a mutant from being killed by the timeout
// purely because the run was parallel.
//
// A failing baseline is a hard error. Every verdict a mutation run produces is
// relative to a suite that passes on unmutated code; if it does not, every
// mutant is "killed" by a failure that was already there and the whole report is
// meaningless.
func baselineTimeout(ctx context.Context, runs, cpus int, timeSuite suiteTimer) (time.Duration, error) {
	if runs <= 0 || cpus <= 0 {
		return MinBaselineTimeout, nil
	}

	var total time.Duration

	for i := range runs {
		elapsed, err := timeSuite(ctx)
		if err != nil {
			return 0, fmt.Errorf("mutate: baseline run %d of %d: %w", i+1, runs, err)
		}

		total += elapsed
	}

	timeout := (total / time.Duration(runs)) * time.Duration(cpus)
	if timeout < MinBaselineTimeout {
		return MinBaselineTimeout, nil
	}

	return timeout, nil
}

// patterns resolves the package patterns to load.
func (o Options) patterns() []string {
	if len(o.Packages) == 0 {
		return []string{"."}
	}

	return o.Packages
}

// mutators resolves the operator set for a run.
//
// An unnamed set is [mutator.All], which is already sorted by name; a named set
// keeps the order the caller gave, since that is just as reproducible and
// reordering a user's list in an error message would be confusing.
func (o Options) mutators() ([]mutator.Mutator, error) {
	if len(o.Operators) == 0 {
		return mutator.All(), nil
	}

	out := make([]mutator.Mutator, 0, len(o.Operators))

	for _, name := range o.Operators {
		m, err := mutator.New(name)
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	return out, nil
}

// parallel clamps the worker count to a usable value.
func (o Options) parallel() int {
	if o.Parallel < 1 {
		return 1
	}

	return o.Parallel
}

// load resolves the target patterns to packages.
//
// The load mode asks only for names, files and module metadata: the engine
// parses the files itself (with comments, which phase 4's //nomutant scan
// needs) rather than reusing packages' syntax trees, and no operator needs type
// information. That keeps the load cheap and avoids type-checking a module that
// may not even type-check cleanly.
func load(ctx context.Context, opts Options) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedModule,
		Dir:     opts.Dir,
		Tests:   false,
	}

	pkgs, err := packages.Load(cfg, opts.patterns()...)
	if err != nil {
		return nil, fmt.Errorf("mutate: loading packages: %w", err)
	}

	var errs []error

	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, fmt.Errorf("mutate: %s: %w", pkg.PkgPath, e))
		}
	})

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("mutate: no packages matched %v", opts.patterns())
	}

	return pkgs, nil
}

// loadTypedCalls counts calls to loadTyped across the process, for tests to
// assert against (see loadTyped's doc comment). It is not otherwise consulted
// by the engine.
var loadTypedCalls atomic.Int64

// needsTypes reports whether any mutator in the set requires static type
// information, i.e. implements [mutator.TypedMutator].
func needsTypes(mutators []mutator.Mutator) bool {
	for _, m := range mutators {
		if _, ok := m.(mutator.TypedMutator); ok {
			return true
		}
	}

	return false
}

// loadTyped re-resolves opts.patterns() with full type information, keyed by
// PkgPath, for [mutator.TypedMutator] operators to consult. It is only ever
// called when [needsTypes] reports true — the common run pays nothing for
// this.
//
// This is a second, separate packages.Load call rather than adding
// NeedTypes/NeedTypesInfo/NeedSyntax to load()'s one call, and that is
// deliberate: load()'s own doc comment states plainly that it keeps loading
// cheap because "no operator needs type information" today, and upgrading its
// mode would make every run — including ones that never select a typed
// operator — pay for type-checking, and would fail package resolution for the
// whole run the moment one package in the module does not fully type-check.
// Keeping this as a separate, opt-in call means a package that fails to
// type-check here can be demoted for this operator alone, in plan() (mirroring
// the ScopeImpact fail-soft precedent there), rather than failing the run.
func loadTyped(ctx context.Context, opts Options) (map[string]*packages.Package, error) {
	// loadTypedCalls exists so a test can prove the "a run that selects no
	// TypedMutator never resolves type information" claim directly, rather
	// than only inferring it from timing or absence of a side effect.
	loadTypedCalls.Add(1)

	cfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax |
			packages.NeedImports | packages.NeedDeps |
			// NeedCompiledGoFiles is what populates CompiledGoFiles, which
			// syntaxFor matches against a job's path to find the syntax tree
			// that corresponds to it — NeedSyntax alone populates Syntax but
			// not the file-path slice it lines up with.
			packages.NeedCompiledGoFiles |
			// NeedName is what populates PkgPath, the key loadTyped's result
			// map (and plan()'s lookup into it) is keyed by.
			packages.NeedName,
		Dir:   opts.Dir,
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, opts.patterns()...)
	if err != nil {
		return nil, fmt.Errorf("mutate: loading typed packages: %w", err)
	}

	// Per-package type errors are not treated as fatal here: pkg.IllTyped
	// records them, and plan() consults that to demote just the affected
	// package's typed operators, rather than this function failing the
	// call outright the way load() fails on a pkg.Errors hit.
	byPath := make(map[string]*packages.Package, len(pkgs))

	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		byPath[pkg.PkgPath] = pkg
	})

	return byPath, nil
}

// loadClosures re-resolves opts.patterns() with import-graph and test-variant
// information, for [resolveClosure] to consult (ROADMAP.md gap 5). It is
// only ever called when the run's requested scope is not [ScopeFull] — see
// [Run]'s own gating.
//
// Tests: true is load-bearing, not incidental: without it, a black-box
// "pkg_test" external test package's own imports are invisible to
// [resolveClosure], which is exactly the bug an earlier review of this
// mechanism found before it was ever wired in (see ROADMAP.md gap 5's
// "Independent review before activation" section) — this project's own
// test-convention cleanup made black-box tests the repo-wide default, so
// getting this wrong here would misclassify mutants in the packages this
// project itself just finished converting to that convention.
//
// The result is keyed by absolute package directory, not PkgPath: a
// Tests:true load returns up to three *packages.Package entries per
// directory (the production package, an internal "pkg [pkg.test]" variant,
// and an external "pkg_test [pkg.test]" variant), each with its own
// synthesized PkgPath, but all three sharing the one directory
// [planPackage] already knows how to compute — the natural join key between
// this result and the plain, Tests:false pkgs [Run] already loaded via
// [load].
func loadClosures(ctx context.Context, opts Options) (map[string][]*packages.Package, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedModule | packages.NeedImports | packages.NeedDeps,
		Dir:   opts.Dir,
		Tests: true,
	}

	pkgs, err := packages.Load(cfg, opts.patterns()...)
	if err != nil {
		return nil, fmt.Errorf("mutate: loading test-aware packages: %w", err)
	}

	byDir := make(map[string][]*packages.Package, len(pkgs))

	for _, pkg := range pkgs {
		files := pkg.GoFiles
		if len(files) == 0 {
			files = pkg.CompiledGoFiles
		}

		if len(files) == 0 {
			continue // a synthetic or otherwise fileless package: nothing resolveClosure could anchor a directory on
		}

		dir := filepath.Dir(files[0])
		byDir[dir] = append(byDir[dir], pkg)
	}

	return byDir, nil
}

// fileJob is one file's share of a run: everything a worker needs to mutate it
// without consulting any state shared with the other workers.
type fileJob struct {
	// moduleDir is the absolute root of the module holding path.
	moduleDir string

	// pkgPath is the import path of the package holding path, e.g.
	// "example.com/fixture/mathx". It is only consulted by [Estimate]'s
	// tally (see visitNode) to key a package's mutant count and, later, its
	// baseline-timing sample — a real run never reads it, since every
	// [MutantResult] already identifies its package via File.
	pkgPath string

	// path is the absolute path of the file to mutate.
	path string

	// scope selects the tests each of this file's mutants is judged by. It is
	// per job rather than read from Options so that a package whose coverage
	// map could not be built can be demoted from [ScopeImpact] to
	// [ScopePackage] on its own, without downgrading the whole run.
	scope Scope

	// cover is the package's line-to-tests coverage map, non-nil only when
	// scope is [ScopeImpact].
	cover *impactMap

	// mutators is this job's operator set. It is the run-wide shared slice
	// for most jobs (identical instances, zero extra cost); it is a
	// per-package slice, built once in plan(), only for packages where a
	// selected operator implements [mutator.TypedMutator] and was
	// successfully bound via WithScope.
	mutators []mutator.Mutator

	// funcPattern is the compiled [Options.FuncPattern], identical for every
	// job in a run — unlike mutators, there is no per-package variation to
	// account for here, so this is just the one run-wide compiled regexp.
	funcPattern *regexp.Regexp

	// mutantID is [Options.MutantID], identical for every job in a run. Empty
	// means every mutant runs; see [visitNode].
	mutantID string

	// tceBaseline is the package's normalized -S disassembly, compiled once
	// from the unmutated sources when [Options.TCE] is set — the same
	// once-per-package precompute shape ScopeImpact's coverage map already
	// uses. Nil means TCE is inactive for this job, either because the
	// option is unset or because the baseline compile failed and the
	// package was fail-soft demoted to running without it.
	tceBaseline []byte

	// closure is the package's pre-resolved dependency closure (ROADMAP.md
	// gap 5), non-nil only when scope above is not [ScopeFull] and
	// [resolveClosure] resolved it safely for this package. Threaded
	// through to every [mutant] the package's mutateFile walk produces —
	// see [runner.workspaceFor].
	closure map[string]bool

	// typedFset and typedSyntax are the FileSet and already-parsed,
	// already-type-checked syntax tree for path, set only when mutators
	// above was bound against type information. mutateFile uses these
	// instead of parsing path fresh: a bound mutator's info.Uses/info.Defs
	// lookups are keyed by *ast.Ident pointer identity, which only the tree
	// that was actually type-checked satisfies — a fresh, independent parse
	// of the same source text would produce equal-looking but distinct
	// *ast.Ident values that resolve to nothing in those maps.
	typedFset   *token.FileSet
	typedSyntax *ast.File
}

// plan enumerates the files to mutate and, for [ScopeImpact], builds each
// package's coverage map before any mutant runs.
//
// The coverage maps are built here, up front and sequentially, rather than
// lazily inside the workers: a map costs one `go test` per test function in the
// package, and it is only worth that once, amortised over every mutant in the
// package. Building it inside a worker would need locking around a
// half-finished map for the sibling files of the same package to wait on, for
// no gain.
//
// Packages outside a module are skipped rather than failing the run: the
// temp-workspace strategy copies a module, so there is nothing to copy for a
// GOPATH-style or synthetic package.
//
// typedPkgs is non-nil only when [needsTypes] reported true for this run; it
// is used, per package, to bind any [mutator.TypedMutator] in mutators via
// [bindMutators].
//
// funcPattern is the compiled [Options.FuncPattern], set identically on every
// job — see [fileJob.funcPattern].
//
// closurePkgs is non-nil only when [Run] determined the run's scope is not
// [ScopeFull]; it is used, per package, to resolve [fileJob.closure] via
// [resolveClosure] (ROADMAP.md gap 5).
//
// estimateOnly is true only for [walkForEstimate]'s call: it makes
// planPackage skip every execution-time-only per-package precompute
// (ScopeImpact's coverage map, TCE's baseline compile, gap 5's dependency
// closure) since none of them affect which mutations the walk finds — only
// how a real mutant would later be executed, which [Estimate] never does.
func plan(ctx context.Context, goBin string, opts Options, pkgs []*packages.Package, mutators []mutator.Mutator, typedPkgs map[string]*packages.Package, closurePkgs map[string][]*packages.Package, funcPattern *regexp.Regexp, estimateOnly bool) ([]fileJob, error) {
	var jobs []fileJob

	for _, pkg := range pkgs {
		pkgJobs, err := planPackage(ctx, goBin, opts, pkg, mutators, typedPkgs, closurePkgs, funcPattern, estimateOnly)
		if err != nil {
			return nil, err
		}

		jobs = append(jobs, pkgJobs...)
	}

	return jobs, nil
}

// planClosure resolves this package's dependency closure (ROADMAP.md gap
// 5), or nil if any precondition doesn't hold — scope is [ScopeFull] (where
// a forward closure is provably wrong, not just unresolved — see
// closureDirs' doc comment), closurePkgs wasn't loaded this run, no variant
// was found for dir (a package [load] resolved but [loadClosures] somehow
// didn't, e.g. a load error affecting only the Tests:true call), or
// [resolveClosure] itself declined (a vendor/ directory, an embed
// directive, an unsafe replace target). Every one of these is the identical
// "fall back to copyModule/copyWorktree" outcome from [runner.workspaceFor]'s
// point of view — nil is the only signal it needs.
func planClosure(scope Scope, closurePkgs map[string][]*packages.Package, dir string) map[string]bool {
	if scope == ScopeFull || closurePkgs == nil {
		return nil
	}

	variants, ok := closurePkgs[dir]
	if !ok {
		return nil
	}

	dirs, ok := resolveClosure(variants...)
	if !ok {
		return nil
	}

	return dirs
}

// planScope resolves the scope and, under [ScopeImpact], the coverage map
// this package's jobs use — a per-package precompute done once before any of
// the package's mutants run.
//
// A package whose coverage map fails to build is demoted to [ScopePackage]
// rather than failing the run: impact scope is an optimisation over package
// scope, and every way of building the map can fail on a package that is
// nonetheless perfectly mutable (an unparseable `go test -list` output, a
// coverage profile a future toolchain writes differently, a test that only
// passes when run alongside its siblings). Demoting this one package costs
// time and nothing else, whereas failing the run would throw away every
// other package's work. The only error this returns is ctx's own
// cancellation.
func planScope(ctx context.Context, goBin string, want Scope, pkg *packages.Package, files []string) (Scope, *impactMap, error) {
	if want != ScopeImpact {
		return want, nil, nil
	}

	if err := ctx.Err(); err != nil {
		return want, nil, err
	}

	cover, err := buildImpact(ctx, goBin, pkg.Module.Dir, filepath.Dir(files[0]), pkg.GoFiles)
	if err != nil {
		if ctx.Err() != nil {
			return want, nil, ctx.Err()
		}

		return ScopePackage, nil, nil
	}

	return want, cover, nil
}

// planTCEBaseline computes the package's once-per-package TCE baseline (see
// [Options.TCE]), or returns nil if TCE is off or the baseline compile
// itself fails.
//
// It builds directly against moduleDir rather than a workspace copy: nothing
// is mutated yet, so there is nothing to isolate (the same reasoning
// goTestSuite's baseline-timing run already documents). Fail soft, per
// package — the same shape as [planPackage]'s ScopeImpact demotion: a
// package whose baseline won't compile for TCE purposes (a toolchain quirk,
// an already-broken package) just runs without TCE, rather than failing the
// whole run. The only error this returns is ctx's own cancellation.
func planTCEBaseline(ctx context.Context, goBin string, opts Options, moduleDir, firstFile string) ([]byte, error) {
	if !opts.TCE {
		return nil, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pattern, err := packagePattern(moduleDir, filepath.Dir(firstFile))
	if err != nil {
		return nil, nil
	}

	baseline, _ := compileDisassembly(ctx, goBin, moduleDir, pattern)

	return baseline, nil
}

// planPackage builds pkg's file jobs, or nil if pkg has nothing mutable (no
// module info, or no non-test .go files). See [plan] for the parameters,
// including estimateOnly.
func planPackage(ctx context.Context, goBin string, opts Options, pkg *packages.Package, mutators []mutator.Mutator, typedPkgs map[string]*packages.Package, closurePkgs map[string][]*packages.Package, funcPattern *regexp.Regexp, estimateOnly bool) ([]fileJob, error) {
	if pkg.Module == nil || pkg.Module.Dir == "" {
		return nil, nil
	}

	var files []string

	for _, path := range pkg.GoFiles {
		// GoFiles already excludes _test.go (Tests is false), but mutating
		// a test file would be meaningless even if one slipped through: the
		// mutant and its own detector would be the same code.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		files = append(files, path)
	}

	if len(files) == 0 {
		return nil, nil
	}

	// Every precompute below is an execution-time-only concern — it changes
	// how a real mutant would later be tested, never which mutations the
	// walk finds — so [Estimate]'s walk skips all three: computing them
	// would cost real go test/go build subprocesses for a preview whose
	// whole point is spawning none (see [walkForEstimate]'s doc comment and
	// ROADMAP.md gap 11a).
	var (
		scope       Scope
		cover       *impactMap
		tceBaseline []byte
		closure     map[string]bool
	)

	if !estimateOnly {
		var err error

		scope, cover, err = planScope(ctx, goBin, opts.Scope, pkg, files)
		if err != nil {
			return nil, err
		}

		tceBaseline, err = planTCEBaseline(ctx, goBin, opts, pkg.Module.Dir, files[0])
		if err != nil {
			return nil, err
		}

		closure = planClosure(scope, closurePkgs, filepath.Dir(files[0]))
	}

	// Per-package, not run-wide, only when a selected operator implements
	// mutator.TypedMutator: most jobs keep pkgMutators == mutators (the
	// identical, run-wide shared slice), so only packages where a typed
	// operator is both selected and successfully bound pay anything extra
	// here.
	pkgMutators := mutators

	var typedPkg *packages.Package

	if typedPkgs != nil {
		tp := typedPkgs[pkg.PkgPath]

		// Fail soft, per package — the same shape as the ScopeImpact
		// demotion above: a package that failed to type-check under
		// loadTyped (or was not resolved by it at all) loses this
		// operator's involvement for itself alone, not the whole run.
		if tp != nil && !tp.IllTyped {
			typedPkg = tp
		}

		pkgMutators = bindMutators(mutators, typedPkg)
	}

	jobs := make([]fileJob, 0, len(files))

	for _, path := range files {
		job := fileJob{
			moduleDir:   pkg.Module.Dir,
			pkgPath:     pkg.PkgPath,
			path:        path,
			scope:       scope,
			cover:       cover,
			mutators:    pkgMutators,
			funcPattern: funcPattern,
			mutantID:    opts.MutantID,
			tceBaseline: tceBaseline,
			closure:     closure,
		}

		if typedPkg != nil {
			job.typedFset = typedPkg.Fset
			job.typedSyntax = syntaxFor(typedPkg, path)
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// bindMutators returns the per-package mutator set: every mutator that does not
// implement [mutator.TypedMutator] is kept as-is — the identical, run-wide
// shared instance, so a package with no typed operators involved allocates a
// new slice but reuses every element. Every mutator that does implement it is
// replaced with a package-bound value from WithScope when typedPkg is usable,
// and dropped entirely otherwise: a package with no usable type information
// runs its mutants without this operator, rather than via the registry's
// stateless, inert placeholder (which would silently match nothing while
// still occupying a slot) or failing the whole run over one package's type
// errors.
func bindMutators(mutators []mutator.Mutator, typedPkg *packages.Package) []mutator.Mutator {
	out := make([]mutator.Mutator, 0, len(mutators))

	for _, m := range mutators {
		typed, ok := m.(mutator.TypedMutator)
		if !ok {
			out = append(out, m)

			continue
		}

		if typedPkg == nil {
			continue
		}

		out = append(out, typed.WithScope(typedPkg.TypesInfo, typedPkg.Types))
	}

	return out
}

// syntaxFor returns the parsed, type-checked syntax tree for path within
// typedPkg, or nil if path is not among typedPkg's compiled files.
func syntaxFor(typedPkg *packages.Package, path string) *ast.File {
	for i, f := range typedPkg.CompiledGoFiles {
		if f == path && i < len(typedPkg.Syntax) {
			return typedPkg.Syntax[i]
		}
	}

	return nil
}

// mutateFile parses one file and runs every mutation every operator offers for
// it.
//
// One call owns its file completely: it is the unit of concurrency, and the
// only state it shares with its siblings is the collector, which serialises
// itself, and the read-only mutator set.
//
// The FileSet is per file rather than per run: positions only ever have to be
// consistent within the file currently being printed, and a fresh set keeps
// memory flat over a large module.
//
// Exactly one of sink/tally is non-nil — see [execute]'s doc comment; both
// are threaded straight through to [visitNode], the only place either is
// read.
func mutateFile(ctx context.Context, run *runner, job *fileJob, sink *collector, tally *estimateTally) error {
	// Checked before parsing, not just inside the walk: with a bounded worker
	// pool most jobs start after cancellation rather than before it, and
	// parsing a file whose mutants will never run is pure waste.
	if err := ctx.Err(); err != nil {
		return err
	}

	path := job.path

	var (
		fset *token.FileSet
		file *ast.File
	)

	if job.typedSyntax != nil {
		// A package-bound TypedMutator's info.Uses/info.Defs lookups are
		// keyed by *ast.Ident pointer identity, so this walk must run
		// against the exact tree that was type-checked (see fileJob's doc
		// comment) rather than a fresh parse of the same source text.
		// packages.Load's own default parse mode includes parser.ParseComments,
		// so //nomutant suppression still works against this tree.
		fset, file = job.typedFset, job.typedSyntax
	} else {
		fset = token.NewFileSet()

		// ParseComments is what makes //nomutant suppression possible: file.Comments
		// is only populated with it, and re-parsing every file a second time to get
		// them would double the engine's parse cost.
		var err error

		file, err = parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("mutate: parsing %s: %w", path, err)
		}
	}

	// Scanned once per file rather than per node: the directives are a property
	// of the source text, not of the walk.
	suppressed := scanSuppressions(fset, file)

	// The baseline for the no-op check is the *printed* pristine AST, not the
	// bytes on disk: go/printer normalises formatting, so comparing against
	// disk would flag every mutant in an unformatted file as changed and every
	// printer-masked mutation as changed too. Printing both sides through the
	// same pipeline makes the comparison mean exactly "did this mutation change
	// the generated source".
	var baseline bytes.Buffer
	if err := printer.Fprint(&baseline, fset, file); err != nil {
		return fmt.Errorf("mutate: printing %s: %w", path, err)
	}

	spec := mutant{
		fset:        fset,
		file:        file,
		path:        path,
		baseline:    baseline.Bytes(),
		moduleDir:   job.moduleDir,
		pkgDir:      filepath.Dir(path),
		scope:       job.scope,
		tceBaseline: job.tceBaseline,
		closureDirs: job.closure,
	}

	// Computed once per file rather than per node/mutation: it depends only on
	// path and moduleDir, both fixed for the whole walk. Falls back to the
	// absolute path if it cannot be made relative (different volume, say) —
	// still stable and unique, just less portable across machines than the
	// relative form.
	relPath, err := filepath.Rel(job.moduleDir, path)
	if err != nil {
		relPath = path
	}

	var walkErr error

	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil || walkErr != nil {
			return false
		}

		if err := ctx.Err(); err != nil {
			walkErr = err

			return false
		}

		visit, err := visitNode(ctx, run, job, relPath, suppressed, sink, tally, &spec, node)
		if err != nil {
			walkErr = err

			return false
		}

		return visit
	})

	return walkErr
}

// mutantID computes a mutation's stable, content-hashed identifier: the tuple
// of the mutated file's path relative to its module root, the mutated node's
// line and column, the operator's registry name, and the mutation's index
// within that node's Mutate() slice (needed because one node/operator pair
// can offer more than one mutation — expression/remove returns two, one per
// operand — and those need distinct IDs despite sharing every other
// coordinate).
//
// Stable across re-runs of unchanged source, matching -fuzz's corpus-hash
// precedent — but not across edits to the file above the mutated line, since
// what's hashed is a position in a specific AST, not self-contained bytes the
// way -fuzz's input hashing is. Truncated to 12 hex characters, the length of
// a git short SHA: enough to be practically collision-free within one run,
// short enough to read in a report or paste into a comment.
func mutantID(relPath string, line, col int, operator string, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s\x00%d", relPath, line, col, operator, index))

	return hex.EncodeToString(sum[:])[:12]
}

// visitNode handles one node of mutateFile's walk: function-pattern
// filtering, //nomutant suppression, and running every mutation every
// applicable operator in job.mutators offers for node. It reports whether
// ast.Inspect should descend into node's children.
//
// spec is the walk's shared mutant template — mutateFile's per-file fields
// (fset, file, path, baseline, moduleDir, pkgDir, scope) are already set; this
// function only fills in the per-node/per-mutation fields before handing a
// copy to run.run. relPath is path relative to its module root, computed once
// per file by the caller — see [mutantID].
//
// Exactly one of sink/tally is non-nil (see [execute]'s doc comment). sink
// is nil-checked before every use, since a suppression can be found — and
// reported, in a real run — regardless of which mode this is; tally being
// non-nil is what actually makes this an [Estimate] walk rather than a real
// one: the branch just above run.run below tallies the mutation and moves
// on, never touching run (which the estimate path never even constructs).
func visitNode(ctx context.Context, run *runner, job *fileJob, relPath string, suppressed suppressions, sink *collector, tally *estimateTally, spec *mutant, node ast.Node) (bool, error) {
	// Returning false here skips this function's body entirely, the same
	// cascade mechanism suppression uses below — a function whose name does
	// not match FuncPattern (and everything nested inside it) is simply
	// never visited, rather than visited-and-rejected node by node.
	// Package-level declarations outside any function are not *ast.FuncDecl
	// and so are never filtered by this check at all — there is no function
	// name for the pattern to match against them.
	//
	// job.funcPattern is nil for any fileJob built without going through
	// plan() (a directly-constructed test fixture, say); nil is treated the
	// same as an empty, always-matching pattern rather than a panic,
	// consistent with FuncPattern's own "empty means everything" zero value.
	if fn, ok := node.(*ast.FuncDecl); ok && job.funcPattern != nil && !job.funcPattern.MatchString(fn.Name.Name) {
		return false, nil
	}

	// Returning false is what makes suppression cascade: ast.Inspect skips the
	// node's children but carries on with its siblings, so a directive on a
	// compound statement covers everything nested inside it without the walk
	// having to carry any "am I inside a suppressed subtree" state.
	if reason, ok := suppressed.anchored(spec.fset, node); ok {
		if sink != nil {
			sink.suppression(SuppressionResult{
				File:   spec.path,
				Line:   spec.fset.Position(node.Pos()).Line,
				Reason: reason,
			})
		}

		return false, nil
	}

	for _, m := range job.mutators {
		if !m.Applies(node) {
			continue
		}

		if err := visitMutations(ctx, run, job, relPath, sink, tally, spec, m, node); err != nil {
			return false, err
		}
	}

	return true, nil
}

// visitMutations runs every mutation m offers for node — either tallying it
// ([Estimate]'s walk, when tally is non-nil) or classifying it via
// run.run (a real run) — recording the per-node bookkeeping (operator,
// line, covering) every one of m's mutations shares. Split out of
// [visitNode] purely to keep that function's own branching manageable; the
// split changes nothing about behaviour, only where it's written.
func visitMutations(ctx context.Context, run *runner, job *fileJob, relPath string, sink *collector, tally *estimateTally, spec *mutant, m mutator.Mutator, node ast.Node) error {
	pos := spec.fset.Position(node.Pos())

	spec.operator = m.Name()
	spec.line = pos.Line
	// The before/after fallback for any [mutator.Mutation] that leaves
	// Node nil — see runner.go's renderNode.
	spec.node = node
	// Looked up per node rather than per mutant: every mutation of a node
	// shares its line, and the map is only consulted at all under
	// ScopeImpact (covering reports nil for a nil map).
	spec.covering = job.cover.covering(spec.path, spec.line)

	for i, mutation := range m.Mutate(node) {
		if err := ctx.Err(); err != nil {
			return err
		}

		spec.mutation = mutation
		spec.id = mutantID(relPath, pos.Line, pos.Column, spec.operator, i)

		// job.mutantID replays exactly one mutant (see [Options.MutantID]):
		// the walk still reaches every node and computes every ID — that
		// part is cheap — but only a matching mutation is ever handed to
		// the runner.
		if job.mutantID != "" && spec.id != job.mutantID {
			continue
		}

		// Estimate's walk-only counting mode (ROADMAP.md gap 11a): tally
		// records that a real run would classify this mutation, without
		// ever calling run.run — the one branch that makes this whole
		// walk a preview rather than a run. run is never touched here;
		// walkForEstimate never even constructs one.
		if tally != nil {
			tally.count(job.pkgPath, spec.moduleDir, spec.pkgDir)

			continue
		}

		res, ok, equivalent, err := run.run(ctx, *spec)
		if err != nil {
			return err
		}

		switch {
		case ok:
			sink.mutant(res)
		case equivalent:
			sink.equivalent(EquivalentResult{
				File:        res.File,
				Line:        res.Line,
				Operator:    res.Operator,
				Description: res.Description,
			})
		}
	}

	return nil
}
