// Command turango is a drop-in replacement for the go command that adds
// mutation testing as a `go test` flag.
//
//	turango test -mutate=. ./...   # run the mutation engine
//	turango build ./...            # forwarded verbatim to the real go toolchain
//
// -mutate behaves like -run/-bench/-fuzz: its value is a regular expression
// matched against function/method names, not a package selector. Package
// selection is the ordinary trailing package arguments, exactly as with
// those three flags — "-mutate=." (mirroring "-bench=.") means "match every
// function," with ./... supplying which packages to look in.
//
// Every invocation that is not `test` with a -mutate= flag is handed to the
// real Go toolchain unchanged, so turango behaves exactly like go plus one new
// flag.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jh125486/turango/internal/goproxy"
	"github.com/jh125486/turango/internal/mutate"
)

// aliasEnvVar opts a user in to running turango under the name "go".
const aliasEnvVar = "TURANGO_EXPERIMENTAL_ALIAS"

// Exit codes. 0/1/2 follow the go command's own convention closely enough not to
// surprise a script that already handles it; 3 is turango's own, so a CI job can
// tell "the mutation score is too low" (an assertion about the code under test)
// apart from "turango failed" (a broken invocation or toolchain).
const (
	exitOK             = 0
	exitFailure        = 1
	exitUsage          = 2
	exitBelowThreshold = 3
)

// The turango flags, without their leading dashes. Every one of them mandates
// the "=" form; see splitMutateFlag.
const (
	flagMutate    = "mutate"
	flagScope     = "mutatescope"
	flagOperators = "mutateoperators"
	flagParallel  = "mutateparallel"
	flagTimeout   = "mutatetimeout"
	flagOutput    = "mutateoutput"
	flagMin       = "mutatemin"
	flagMutant    = "mutatemutant"
	flagTCE       = "mutatetce"
	flagWorkspace = "mutateworkspace"
)

// flagArgsSeparator is go test's own "-args" boundary: everything after it
// belongs to the compiled test binary, not to go test (or turango) to
// interpret. See parseMutateFlags.
const flagArgsSeparator = "-args"

// mutateFlags is the recognised set, in the order they are documented.
var mutateFlags = []string{
	flagMutate, flagScope, flagOperators, flagParallel, flagTimeout, flagOutput, flagMin, flagMutant, flagTCE,
	flagWorkspace,
}

// reportFile is the name of the JSON report written into -mutateoutput.
const reportFile = "mutate-report.json"

func main() {
	os.Exit(mainRun())
}

// mainRun is main's body, split out so the signal-handler `defer stop()`
// below actually runs before exit instead of being skipped by os.Exit.
func mainRun() int {
	// The engine's unit of work is a full `go test` run, so a Ctrl+C that only
	// killed turango would leave the child toolchain running and throw away
	// however many hours of mutants had already been tested. Cancelling the
	// context instead unwinds both: in-flight mutants are killed with their
	// process group, and Run returns what it has.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, os.Args, os.Stdout, os.Stderr)
}

// run is main's testable body. args is the full argv, including argv[0].
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "turango: missing argv[0]")

		return exitUsage
	}

	if err := checkAlias(args[0]); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)

		return exitUsage
	}

	rest := args[1:]

	if len(rest) > 0 && rest[0] == "test" {
		cfg, found, err := parseMutateFlags(rest[1:])
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)

			return exitUsage
		}

		if found {
			return mutateRun(ctx, &cfg, stdout, stderr)
		}
	}

	// Everything else is the real go command's business. On Unix this replaces
	// the process and never returns.
	code, err := goproxy.Forward(rest)
	if err != nil {
		fmt.Fprintf(stderr, "turango: %v\n", err)
	}

	return code
}

// mutateRun executes the engine and reports what it found.
//
// A cancelled run reports its partial results through exactly the same path a
// completed one does, because losing hours of finished mutants to a Ctrl+C is
// the failure mode this whole path exists to prevent.
func mutateRun(ctx context.Context, cfg *mutateConfig, stdout, stderr io.Writer) int {
	result, runErr := mutate.Run(ctx, cfg.options)

	interrupted := errors.Is(runErr, context.Canceled)

	switch {
	case interrupted:
		fmt.Fprintln(stderr, "turango: interrupted; reporting the mutants completed so far")
	case runErr != nil:
		fmt.Fprintf(stderr, "turango: %v\n", runErr)
	}

	if result == nil {
		return exitFailure
	}

	writeSummary(stdout, result)

	code := exitOK
	if runErr != nil {
		code = exitFailure
	}

	if cfg.output != "" {
		if err := writeReport(cfg.output, result); err != nil {
			fmt.Fprintf(stderr, "turango: %v\n", err)

			code = exitFailure
		}
	}

	// A failed or interrupted run's score is measured against however much of
	// the module it got through, so gating on it would report a threshold
	// breach that says nothing about the code.
	if code != exitOK {
		return code
	}

	return gate(stderr, cfg, result)
}

// gate applies -mutatemin.
func gate(stderr io.Writer, cfg *mutateConfig, result *mutate.Result) int {
	if !cfg.hasMin {
		return exitOK
	}

	score, ok := result.Score()
	if !ok {
		// Not a pass and not a failure: there was nothing to measure. Exiting
		// zero in silence would let a CI job that mutates the wrong pattern —
		// and so produces no viable mutants at all — report a clean gate
		// forever.
		fmt.Fprintf(stderr,
			"turango: -%s=%v not evaluated: the run produced no viable mutants to score\n",
			flagMin, cfg.min)

		return exitOK
	}

	if score < cfg.min {
		fmt.Fprintf(stderr, "turango: mutation score %.4f is below -%s=%v\n", score, flagMin, cfg.min)

		return exitBelowThreshold
	}

	return exitOK
}

// writeSummary prints the run's counts, score, suppression ratio and surviving
// mutants.
//
// The formatting itself belongs to the report package, next to the types being
// formatted; what the command owns is the one thing the package cannot know —
// which directory the paths should be shown relative to.
func writeSummary(w io.Writer, result *mutate.Result) {
	result.WriteSummary(w, displayBase())
}

// displayBase is the directory report paths are rendered relative to: the
// working directory, which is where the user invoked turango and so what any
// path they are about to open in an editor is relative to.
//
// An unavailable working directory is not worth failing a finished run over; the
// empty string leaves paths absolute, which is still correct, just longer.
func displayBase() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	return dir
}

// writeReport dumps the result as JSON into dir.
//
// The paths in the JSON are relativised exactly as the console summary's are, so
// a report produced in CI is readable after it is downloaded somewhere else, and
// so a line in the summary and its row in the report are the same string.
//
//nolint:gosec // dir is -mutateoutput, a path the invoking user typed on their own command line, not external/tainted input
func writeReport(dir string, result *mutate.Result) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(result.Relativize(displayBase()), "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the report: %w", err)
	}

	path := filepath.Join(dir, reportFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// checkAlias enforces the experimental gate on running turango under the name
// "go".
//
// Renaming or symlinking turango to "go" and putting it first in PATH routes
// every tool on the machine that shells out to go — gopls, IDEs, CI,
// Makefiles, go generate, other linters — through turango's passthrough. That
// is the highest blast-radius mode turango has, so it is opt-in only and not
// an advertised v1 feature. Invoked under any other name (the normal
// `turango ...` case), no gate applies.
func checkAlias(argv0 string) error {
	base := strings.ToLower(filepath.Base(argv0))
	if base != "go" && base != "go.exe" {
		return nil
	}

	if os.Getenv(aliasEnvVar) == "1" {
		return nil
	}

	//nolint:revive,staticcheck // this is a direct top-level CLI message, printed as-is and never wrapped with %w; the usual lowercase/no-punctuation error-string convention is for errors that get wrapped and re-logged elsewhere.
	return fmt.Errorf(`turango: refusing to run as %q.

Running turango under the name "go" (rename or symlink, placed ahead of the
real toolchain in PATH) is an experimental, opt-in mode: it routes every tool
on this machine that invokes "go" through turango's passthrough, so a single
unhandled verb or flag breaks your whole toolchain, not just mutation testing.

Use the supported form instead:

    turango test -mutate=. ./...

or, if you understand the blast radius, set %s=1 to enable alias mode.`, base, aliasEnvVar)
}

// mutateConfig is a parsed `turango test -mutate=...` command line.
type mutateConfig struct {
	options mutate.Options

	// output is the -mutateoutput directory, empty when no report was asked
	// for.
	output string

	// min is the -mutatemin threshold, meaningful only when hasMin is set. A
	// pair rather than a *float64 so that -mutatemin=0 — a legitimate "any
	// score passes, but tell me it was evaluated" — is distinguishable from the
	// flag being absent.
	min    float64
	hasMin bool
}

// parseMutateFlags scans the arguments that follow `test` for turango's own
// flags plus, exactly as with `-run`/`-bench`/`-fuzz`, ordinary trailing
// package arguments.
//
// found reports whether -mutate was actually given. When it is false the
// arguments are not a mutation request at all — the ordinary
// `turango test ./...` case — which is what sends the invocation on to the
// real go command untouched; cfg is meaningless in that case.
//
// The scan stops at a literal "-args", matching go test's own rule that
// everything after -args belongs to the compiled test binary and is not go
// test's (or turango's) to interpret. A -mutate= appearing after -args is left
// alone and passed straight through.
func parseMutateFlags(args []string) (cfg mutateConfig, found bool, err error) {
	cfg = mutateConfig{
		options: mutate.Options{
			Scope: mutate.ScopeFull,
			// GOMAXPROCS rather than NumCPU so that a container CPU limit, or
			// an explicit GOMAXPROCS, is respected: turango is scheduling
			// whole `go test` processes, and oversubscribing them is worse than
			// oversubscribing goroutines.
			Parallel: runtime.GOMAXPROCS(0),
		},
	}

	var leftover []string

	for _, arg := range args {
		if arg == flagArgsSeparator || arg == "--args" {
			break
		}

		name, value, ok, splitErr := splitMutateFlag(arg)
		if splitErr != nil {
			return mutateConfig{}, false, splitErr
		}

		if !ok {
			leftover = append(leftover, arg)

			continue
		}

		if setErr := cfg.set(name, value); setErr != nil {
			return mutateConfig{}, false, setErr
		}

		if name == flagMutate {
			found = true
		}
	}

	// Nothing is validated against leftover until we know -mutate was
	// actually given: an ordinary `turango test -v -race ./...` (no -mutate
	// at all) must forward untouched to the real go command exactly as
	// before, including whatever real go test flags it carries — those are
	// none of turango's business unless mutation mode was actually
	// requested.
	if !found {
		return mutateConfig{}, false, nil
	}

	var packages []string

	for _, arg := range leftover {
		// A bare, dash-less argument is a legitimate positional package
		// pattern — exactly what -run/-bench/-fuzz already leave to go
		// test's own trailing arguments. Anything else starting with "-" is
		// an unrecognised go test flag: guessing what it should do to a
		// mutant's test run is the alternative, and the guesses are all
		// bad, so it is rejected rather than silently ignored or
		// misapplied.
		if strings.HasPrefix(arg, "-") {
			return mutateConfig{}, false, fmt.Errorf(
				"turango: unsupported flag %q alongside -%s: mutation mode understands only the -mutateXXX= flags (%s) plus trailing package patterns",
				arg, flagMutate, strings.Join(mutateFlags, ", "))
		}

		packages = append(packages, arg)
	}

	cfg.options.Packages = packages

	return cfg, true, nil
}

// splitMutateFlag classifies one argument.
//
// ok reports that arg is one of turango's flags, with name stripped of its
// leading dashes and value everything after the "=". Anything else is not an
// error here — it may be a perfectly ordinary `go test` argument — and is
// reported with ok false for the caller to judge in context.
func splitMutateFlag(arg string) (name, value string, ok bool, err error) {
	if !strings.HasPrefix(arg, "-") {
		return "", "", false, nil
	}

	body := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")

	name, value, hasEq := strings.Cut(body, "=")

	if !isMutateFlag(name) {
		// A misspelling ("-mutateoperator=", "-mutatescopes=") is worth
		// catching: it is unambiguously aimed at turango, and passing it
		// through to go test would report it as go's unknown flag, which sends
		// the user looking in the wrong manual.
		if strings.HasPrefix(name, flagMutate) {
			return "", "", false, fmt.Errorf(
				"turango: unknown flag %q (turango's flags are: -%s)",
				arg, strings.Join(mutateFlags, ", -"))
		}

		return "", "", false, nil
	}

	if !hasEq {
		return "", "", false, fmt.Errorf(
			"turango: -%s requires the = form (-%s=VALUE); a bare -%s is ambiguous with the value that follows it",
			name, name, name)
	}

	return name, value, true, nil
}

// isMutateFlag reports whether name is one of turango's flags.
func isMutateFlag(name string) bool {
	return slices.Contains(mutateFlags, name)
}

// set applies one parsed flag.
func (c *mutateConfig) set(name, value string) error {
	switch name {
	case flagMutate:
		return c.setMutate(value)
	case flagScope:
		return c.setScope(value)
	case flagOperators:
		// Unknown names are the engine's to reject: it owns the registry, and
		// its error already lists what is registered.
		c.options.Operators = splitList(value)
	case flagParallel:
		return c.setParallel(value)
	case flagTimeout:
		return c.setTimeout(value)
	case flagOutput:
		return c.setOutput(value)
	case flagMin:
		return c.setMin(value)
	case flagMutant:
		return c.setMutant(value)
	case flagTCE:
		return c.setTCE(value)
	case flagWorkspace:
		return c.setWorkspace(value)
	}

	return nil
}

// setMutate validates and stores -mutate.
//
// Validated here, at parse time, rather than left for Run() to reject later —
// the same early-fail shape setScope's ParseScope call already has. -mutate's
// value is a regexp matched against function names (mirroring
// -run/-bench/-fuzz), not a package pattern: package selection is the
// ordinary trailing positional arguments, collected separately in
// parseMutateFlags.
func (c *mutateConfig) setMutate(value string) error {
	if _, err := regexp.Compile(value); err != nil {
		return fmt.Errorf("turango: -%s=%q: %w", flagMutate, value, err)
	}

	c.options.FuncPattern = value

	return nil
}

// setScope validates and stores -mutatescope.
func (c *mutateConfig) setScope(value string) error {
	scope, err := mutate.ParseScope(value)
	if err != nil {
		// Not re-wrapped with the flag name and value: ParseScope's message
		// already names both the value it rejected and the ones it accepts.
		return fmt.Errorf("turango: %w", err)
	}

	c.options.Scope = scope

	return nil
}

// setParallel validates and stores -mutateparallel.
func (c *mutateConfig) setParallel(value string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return fmt.Errorf("turango: -%s=%q: want a positive integer", flagParallel, value)
	}

	c.options.Parallel = n

	return nil
}

// setTimeout validates and stores -mutatetimeout.
func (c *mutateConfig) setTimeout(value string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("turango: -%s=%q: %w", flagTimeout, value, err)
	}

	if d <= 0 {
		return fmt.Errorf("turango: -%s=%q: want a positive duration, e.g. 30s", flagTimeout, value)
	}

	c.options.TestTimeout = d

	return nil
}

// setOutput validates and stores -mutateoutput.
func (c *mutateConfig) setOutput(value string) error {
	if value == "" {
		return fmt.Errorf("turango: -%s= requires a directory", flagOutput)
	}

	c.output = value

	return nil
}

// setMin validates and stores -mutatemin.
func (c *mutateConfig) setMin(value string) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("turango: -%s=%q: want a number between 0 and 1", flagMin, value)
	}

	c.min, c.hasMin = f, true

	return nil
}

// mutantIDPattern matches the lowercase-hex form every [mutate.MutantResult.ID]
// is printed in.
var mutantIDPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// setMutant validates and stores -mutatemutant: replay exactly one mutant by
// the ID a previous report's MutantResult.ID (or the console survivor
// listing) printed for it.
func (c *mutateConfig) setMutant(value string) error {
	if !mutantIDPattern.MatchString(value) {
		return fmt.Errorf("turango: -%s=%q: want a mutant ID from a previous report (lowercase hex)", flagMutant, value)
	}

	c.options.MutantID = value

	return nil
}

// setTCE validates and stores -mutatetce: whether Trivial Compiler
// Equivalence filters compiled-identical mutants before they reach the test
// suite. Off by default (mutate.Options{}'s zero value); -mutatetce=true
// opts in.
func (c *mutateConfig) setTCE(value string) error {
	tce, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("turango: -%s=%q: want true or false", flagTCE, value)
	}

	c.options.TCE = tce

	return nil
}

// setWorkspace validates and stores -mutateworkspace: whether a mutant's
// throwaway execution copy is built by filesystem copy (the default) or
// `git worktree add`. See ROADMAP.md gap 6 — worktree mode is strictly
// opt-in and falls back to the copy strategy on its own whenever the
// target isn't inside a clean git working tree, so requesting it is always
// safe.
func (c *mutateConfig) setWorkspace(value string) error {
	workspace, err := mutate.ParseWorkspace(value)
	if err != nil {
		// Not re-wrapped with the flag name and value: ParseWorkspace's
		// message already names both the value it rejected and the ones it
		// accepts.
		return fmt.Errorf("turango: %w", err)
	}

	c.options.Workspace = workspace

	return nil
}

// splitList parses a comma-separated flag value, dropping empty entries so that
// a trailing comma is not a third, blank item.
func splitList(value string) []string {
	var out []string

	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}
