package goproxy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGo writes an executable stub named after the current platform's go
// binary into dir, creating dir first, and returns the stub's path.
func fakeGo(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	path := filepath.Join(dir, goBinaryName)
	body := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		body = "@echo off\r\nexit /b 0\r\n"
	}

	//nolint:gosec // this is the fake "go" binary the test executes; it needs the exec bit
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// joinPath builds a PATH-style list from dirs.
func joinPath(dirs ...string) string {
	return strings.Join(dirs, string(os.PathListSeparator))
}

// chdir switches the working directory for the duration of the test.
// (testing.T.Chdir would do this, but it needs Go 1.24 and this module's floor
// is 1.23.)
func chdir(t *testing.T, dir string) {
	t.Helper()

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	})
}

func TestResolvePrefersGOROOTEnvVar(t *testing.T) {
	root := t.TempDir()
	want := fakeGo(t, filepath.Join(root, "bin"))

	pathDir := t.TempDir()
	fakeGo(t, pathDir)

	fallbackRoot := t.TempDir()
	fakeGo(t, filepath.Join(fallbackRoot, "bin"))

	r := resolver{
		goroot:   root,
		path:     joinPath(pathDir),
		fallback: fallbackRoot,
		binName:  goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want the GOROOT env candidate %q", got, want)
	}
}

// TestResolvePrefersPATHOverRuntimeGOROOT is the regression test for the
// discovery-order risk called out in review: runtime.GOROOT() reports the
// toolchain turango was built with, so it must never win over the toolchain the
// user actually has on PATH.
func TestResolvePrefersPATHOverRuntimeGOROOT(t *testing.T) {
	pathDir := t.TempDir()
	want := fakeGo(t, pathDir)

	fallbackRoot := t.TempDir()
	notWant := fakeGo(t, filepath.Join(fallbackRoot, "bin"))

	r := resolver{
		goroot:   "", // GOROOT unset
		path:     joinPath(pathDir),
		fallback: fallbackRoot,
		binName:  goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got == notWant {
		t.Fatalf("resolve() = %q, but runtime.GOROOT() must only be a last resort", got)
	}
	if got != want {
		t.Errorf("resolve() = %q, want the PATH candidate %q", got, want)
	}
}

func TestResolveScansPATHInOrder(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()

	// Only the second directory holds a go binary.
	want := fakeGo(t, second)

	r := resolver{
		path:    joinPath(first, second),
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q", got, want)
	}
}

// TestResolveSkipsSelfOnPATH covers the alias-as-go case: turango renamed to
// "go" and placed first in PATH must not resolve to itself, or passthrough
// would recurse forever.
func TestResolveSkipsSelfOnPATH(t *testing.T) {
	selfDir := t.TempDir()
	self := fakeGo(t, selfDir)

	realDir := t.TempDir()
	want := fakeGo(t, realDir)

	r := resolver{
		path:    joinPath(selfDir, realDir),
		self:    self,
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got == self {
		t.Fatal("resolve() returned turango's own executable; passthrough would recurse")
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q", got, want)
	}
}

// TestResolveSkipsSelfViaSymlink checks that self-detection compares resolved
// paths, not literal strings: a symlink named "go" pointing at the turango
// binary is still ourselves.
func TestResolveSkipsSelfViaSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}

	binDir := t.TempDir()
	self := filepath.Join(binDir, "turango")
	//nolint:gosec // this is the fake "go" binary the test executes; it needs the exec bit
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write self: %v", err)
	}

	linkDir := t.TempDir()
	link := filepath.Join(linkDir, goBinaryName)
	if err := os.Symlink(self, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	realDir := t.TempDir()
	want := fakeGo(t, realDir)

	r := resolver{
		path:    joinPath(linkDir, realDir),
		self:    self,
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q (symlink to self must be skipped)", got, want)
	}
}

func TestResolveFallsBackToRuntimeGOROOT(t *testing.T) {
	emptyDir := t.TempDir()

	fallbackRoot := t.TempDir()
	want := fakeGo(t, filepath.Join(fallbackRoot, "bin"))

	r := resolver{
		path:     joinPath(emptyDir),
		fallback: fallbackRoot,
		binName:  goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want the runtime.GOROOT() fallback %q", got, want)
	}
}

// TestResolveIgnoresUnusableGOROOT covers GOROOT pointing somewhere that has no
// toolchain: discovery must continue to PATH rather than returning a path that
// cannot be executed.
func TestResolveIgnoresUnusableGOROOT(t *testing.T) {
	pathDir := t.TempDir()
	want := fakeGo(t, pathDir)

	r := resolver{
		goroot:  t.TempDir(), // exists, but has no bin/go
		path:    joinPath(pathDir),
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q", got, want)
	}
}

func TestResolveIgnoresEmptyPATHEntries(t *testing.T) {
	realDir := t.TempDir()
	want := fakeGo(t, realDir)

	// A "go" in the working directory must not be picked up via an empty PATH
	// entry.
	wd := t.TempDir()
	fakeGo(t, wd)
	chdir(t, wd)

	r := resolver{
		path:    joinPath("", realDir),
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q", got, want)
	}
}

func TestResolveNotFound(t *testing.T) {
	r := resolver{
		goroot:   t.TempDir(),
		path:     joinPath(t.TempDir()),
		fallback: t.TempDir(),
		binName:  goBinaryName,
	}

	if _, err := r.resolve(); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolve() error = %v, want ErrNotFound", err)
	}
}

func TestResolveSkipsDirectoriesNamedGo(t *testing.T) {
	pathDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pathDir, goBinaryName), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	realDir := t.TempDir()
	want := fakeGo(t, realDir)

	r := resolver{
		path:    joinPath(pathDir, realDir),
		binName: goBinaryName,
	}

	got, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if got != want {
		t.Errorf("resolve() = %q, want %q", got, want)
	}
}

// TestResolveUsesLiveEnvironment sanity-checks the exported wrapper against the
// real environment this test is running in, where a go toolchain is guaranteed
// to exist.
func TestResolveUsesLiveEnvironment(t *testing.T) {
	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if filepath.Base(got) != goBinaryName {
		t.Errorf("Resolve() = %q, want a path ending in %q", got, goBinaryName)
	}
	if !isExecutable(got) {
		t.Errorf("Resolve() = %q, which is not executable", got)
	}
}
