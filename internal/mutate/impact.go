package mutate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/cover"
)

// impactMap answers "which of this package's tests execute this line?".
//
// It is built once per package, before any mutant of that package runs, and
// then only read — including concurrently, by every worker mutating a file of
// that package — so it carries no lock and must not be modified after
// [buildImpact] returns it.
type impactMap struct {
	// lines maps an absolute source file path to a line number to the names of
	// the test functions that executed that line on unmutated source, in the
	// order the tests were listed.
	lines map[string]map[int][]string
}

// covering reports the tests that execute file:line.
//
// A nil map covers nothing, which is what lets callers hold a *impactMap
// unconditionally and only consult it under [ScopeImpact].
func (m *impactMap) covering(file string, line int) []string {
	if m == nil {
		return nil
	}

	return m.lines[file][line]
}

// testFuncName matches the names `go test -list` prints for things -run can
// select. Benchmarks are deliberately excluded: -run does not start them, so
// running one would measure nothing and cost a full build.
var testFuncName = regexp.MustCompile(`^(Test|Fuzz|Example)\S*$`)

// buildImpact measures which tests cover which lines of one package.
//
// A single aggregate -coverprofile cannot answer that: it merges every test's
// coverage into one profile, so it reports that the package's suite covers a
// line, never which test did. The only way to attribute coverage to individual
// tests with the standard toolchain is to run them individually, which is what
// this does — one `go test -run=^Name$ -coverprofile` per test function.
//
// That is expensive, and deliberately paid once per package rather than once
// per mutant: it costs O(tests) test runs to save O(mutants) of them, and on any
// package worth mutating there are far more mutants than tests.
//
// Everything here runs against the original, unmutated sources in place. No
// mutation has been applied to disk at this point (mutants are only ever
// written into throwaway module copies), so there is nothing to isolate.
func buildImpact(ctx context.Context, goBin, moduleDir, pkgDir string, goFiles []string) (*impactMap, error) {
	pattern, err := packagePattern(moduleDir, pkgDir)
	if err != nil {
		return nil, err
	}

	tests, err := listTests(ctx, goBin, moduleDir, pattern)
	if err != nil {
		return nil, err
	}

	m := &impactMap{lines: make(map[string]map[int][]string)}

	// A package with no test functions is not a failure to measure — it is a
	// complete measurement whose answer is "nothing covers anything". Every
	// mutant in it is a survivor, and reporting that without running a single
	// `go test` is exactly the outcome package scope would reach the slow way.
	if len(tests) == 0 {
		return m, nil
	}

	dir, err := os.MkdirTemp("", "turango-impact-")
	if err != nil {
		return nil, fmt.Errorf("mutate: creating coverage workspace: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	for i, name := range tests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		profile := filepath.Join(dir, fmt.Sprintf("cover%d.out", i))

		//nolint:gosec // running "go test" against the module under mutation is turango's core function, not attacker-controlled input
		cmd := exec.CommandContext(ctx, goBin,
			"test",
			"-count=1",
			"-vet=off",
			"-run=^"+regexp.QuoteMeta(name)+"$",
			// Explicit rather than implied: the default instruments only the
			// package under test as seen from its own test binary, which
			// attributes nothing to p when the tests live in an external
			// p_test package that imports p.
			"-coverpkg="+pattern,
			"-coverprofile="+profile,
			pattern,
		)
		cmd.Dir = moduleDir

		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("mutate: measuring coverage of %s: %w\n%s", name, err, truncate(string(out)))
		}

		if err := m.merge(profile, name, goFiles); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// merge folds one test's coverage profile into the map.
func (m *impactMap) merge(profilePath, testName string, goFiles []string) error {
	profiles, err := cover.ParseProfiles(profilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// A test that is skipped before it runs anything can leave no
			// profile behind. It covered nothing, which is already what the map
			// says.
			return nil
		}

		return fmt.Errorf("mutate: parsing coverage profile for %s: %w", testName, err)
	}

	for _, profile := range profiles {
		file, ok := matchProfileFile(profile.FileName, goFiles)
		if !ok {
			continue
		}

		m.mergeBlocks(file, testName, profile.Blocks)
	}

	return nil
}

// mergeBlocks folds one profile's blocks for one file into the map.
func (m *impactMap) mergeBlocks(file, testName string, blocks []cover.ProfileBlock) {
	byLine := m.lines[file]
	if byLine == nil {
		byLine = make(map[int][]string)
		m.lines[file] = byLine
	}

	for _, block := range blocks {
		// Count is the number of times the block ran. Zero means this test
		// compiled the line and never executed it, which is exactly the
		// case impact scope is here to skip.
		if block.Count == 0 {
			continue
		}

		for line := block.StartLine; line <= block.EndLine; line++ {
			byLine[line] = appendTest(byLine[line], testName)
		}
	}
}

// appendTest adds name to tests unless it is already the most recent entry.
//
// Profiles are merged one test at a time, so a name can only ever repeat
// against blocks of the test currently being merged — overlapping blocks of the
// same test, which is common around a multi-line statement. Checking the tail is
// therefore enough, and avoids a set allocation per line.
func appendTest(tests []string, name string) []string {
	if len(tests) > 0 && tests[len(tests)-1] == name {
		return tests
	}

	return append(tests, name)
}

// matchProfileFile maps a coverage profile's file name to the absolute path of
// the package file it refers to.
//
// Profiles name files by import path ("example.com/m/mathx/mathx.go"), which is
// not a filesystem path and cannot be resolved as one. Matching on the base name
// is sound here because the candidates are the files of a single package, and Go
// packages live in a single directory where names are necessarily unique.
func matchProfileFile(profileName string, goFiles []string) (string, bool) {
	base := path.Base(profileName)

	for _, file := range goFiles {
		if filepath.Base(file) == base {
			return file, true
		}
	}

	return "", false
}

// listTests enumerates the test functions of one package.
//
// `go test -list` prints one name per line plus a trailing "ok <pkg> <time>"
// summary, so the output is filtered by shape rather than trusted wholesale.
func listTests(ctx context.Context, goBin, dir, pattern string) ([]string, error) {
	cmd := exec.CommandContext(ctx, goBin, "test", "-count=1", "-vet=off", "-list=.*", pattern)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mutate: listing tests in %s: %w\n%s", pattern, err, truncate(stderr.String()))
	}

	return parseTestList(stdout.String()), nil
}

// parseTestList picks the test function names out of `go test -list` output.
func parseTestList(out string) []string {
	var names []string

	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); testFuncName.MatchString(line) {
			names = append(names, line)
		}
	}

	return names
}
