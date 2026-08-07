package mutate

import (
	"bytes"
	"context"
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
	"sync"
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
		return "unknown"
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

	timeout, err := resolveTimeout(ctx, opts, goTestSuite(goBin, opts.Dir, opts.patterns()))
	if err != nil {
		return nil, err
	}

	run := &runner{goBin: goBin, testTimeout: timeout}

	jobs, err := plan(ctx, goBin, opts, pkgs, mutators, typedPkgs, funcPattern)
	if err != nil {
		return &Result{}, err
	}

	sink := &collector{result: &Result{}}
	runErr := execute(ctx, run, jobs, opts.parallel(), sink)

	return sink.sorted(), runErr
}

// execute drives the file workers, bounded at parallel files in flight.
//
// The group's context is derived from ctx, so the first failing file cancels
// the rest instead of leaving a doomed run to grind through every remaining
// mutant — each of which costs a full `go test`.
func execute(ctx context.Context, run *runner, jobs []fileJob, parallel int, sink *collector) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(parallel)

	for _, job := range jobs {
		group.Go(func() error {
			return mutateFile(groupCtx, run, job, sink)
		})
	}

	return group.Wait()
}

// collector serialises the file workers' appends into one Result.
//
// Results are accumulated unordered and sorted once at the end rather than
// being slotted into a pre-sized per-file position: how many mutants a file
// yields is only known once its walk is finished.
type collector struct {
	mu     sync.Mutex
	result *Result
}

// mutant records one mutant's verdict.
func (c *collector) mutant(res MutantResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.result.Mutants = append(c.result.Mutants, res)
}

// suppression records one //nomutant hit.
func (c *collector) suppression(res SuppressionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.result.Suppressions = append(c.result.Suppressions, res)
}

// sorted restores the deterministic report order and returns the Result.
func (c *collector) sorted() *Result {
	c.mu.Lock()
	defer c.mu.Unlock()

	sort.SliceStable(c.result.Mutants, func(i, j int) bool {
		a, b := c.result.Mutants[i], c.result.Mutants[j]

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

	sort.SliceStable(c.result.Suppressions, func(i, j int) bool {
		a, b := c.result.Suppressions[i], c.result.Suppressions[j]

		if a.File != b.File {
			return a.File < b.File
		}

		return a.Line < b.Line
	})

	return c.result
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

// fileJob is one file's share of a run: everything a worker needs to mutate it
// without consulting any state shared with the other workers.
type fileJob struct {
	// moduleDir is the absolute root of the module holding path.
	moduleDir string

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
func plan(ctx context.Context, goBin string, opts Options, pkgs []*packages.Package, mutators []mutator.Mutator, typedPkgs map[string]*packages.Package, funcPattern *regexp.Regexp) ([]fileJob, error) {
	var jobs []fileJob

	for _, pkg := range pkgs {
		pkgJobs, err := planPackage(ctx, goBin, opts, pkg, mutators, typedPkgs, funcPattern)
		if err != nil {
			return nil, err
		}

		jobs = append(jobs, pkgJobs...)
	}

	return jobs, nil
}

// planPackage builds pkg's file jobs, or nil if pkg has nothing mutable (no
// module info, or no non-test .go files). See [plan] for the parameters.
func planPackage(ctx context.Context, goBin string, opts Options, pkg *packages.Package, mutators []mutator.Mutator, typedPkgs map[string]*packages.Package, funcPattern *regexp.Regexp) ([]fileJob, error) {
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

	scope, cover := opts.Scope, (*impactMap)(nil)

	if scope == ScopeImpact {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var err error

		cover, err = buildImpact(ctx, goBin, pkg.Module.Dir, filepath.Dir(files[0]), pkg.GoFiles)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// Fail soft, per package. Impact scope is an optimisation over
			// package scope, and every way of building the map can fail on
			// a package that is nonetheless perfectly mutable: an
			// unparseable `go test -list` output, a coverage profile a
			// future toolchain writes differently, a test that only passes
			// when run alongside its siblings. Demoting this one package to
			// package scope costs time and nothing else, whereas failing
			// the run would throw away every other package's work.
			scope, cover = ScopePackage, nil
		}
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
			path:        path,
			scope:       scope,
			cover:       cover,
			mutators:    pkgMutators,
			funcPattern: funcPattern,
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
func mutateFile(ctx context.Context, run *runner, job fileJob, sink *collector) error {
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
		fset:      fset,
		file:      file,
		path:      path,
		baseline:  baseline.Bytes(),
		moduleDir: job.moduleDir,
		pkgDir:    filepath.Dir(path),
		scope:     job.scope,
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

		visit, err := visitNode(ctx, run, job, suppressed, sink, &spec, node)
		if err != nil {
			walkErr = err

			return false
		}

		return visit
	})

	return walkErr
}

// visitNode handles one node of mutateFile's walk: function-pattern
// filtering, //nomutant suppression, and running every mutation every
// applicable operator in job.mutators offers for node. It reports whether
// ast.Inspect should descend into node's children.
//
// spec is the walk's shared mutant template — mutateFile's per-file fields
// (fset, file, path, baseline, moduleDir, pkgDir, scope) are already set; this
// function only fills in the per-node/per-mutation fields before handing a
// copy to run.run.
func visitNode(ctx context.Context, run *runner, job fileJob, suppressed suppressions, sink *collector, spec *mutant, node ast.Node) (bool, error) {
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
		sink.suppression(SuppressionResult{
			File:   spec.path,
			Line:   spec.fset.Position(node.Pos()).Line,
			Reason: reason,
		})

		return false, nil
	}

	for _, m := range job.mutators {
		if !m.Applies(node) {
			continue
		}

		spec.operator = m.Name()
		spec.line = spec.fset.Position(node.Pos()).Line
		// Looked up per node rather than per mutant: every mutation of a node
		// shares its line, and the map is only consulted at all under
		// ScopeImpact (covering reports nil for a nil map).
		spec.covering = job.cover.covering(spec.path, spec.line)

		for _, mutation := range m.Mutate(node) {
			if err := ctx.Err(); err != nil {
				return false, err
			}

			spec.mutation = mutation

			res, ok, err := run.run(ctx, *spec)
			if err != nil {
				return false, err
			}

			if ok {
				sink.mutant(res)
			}
		}
	}

	return true, nil
}
