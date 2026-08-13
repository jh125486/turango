// Whitebox: every scenario here needs the unexported resolver struct to drive
// discovery-order edge cases (GOROOT priority, self-skip, PATH scanning) with
// fake goroot/path/fallback/self/binName fields instead of mutating the real
// process environment via t.Setenv, which would forbid t.Parallel(). The one
// test exercising the exported Resolve() wrapper also needs the unexported
// goBinaryName and isExecutable to check its result without duplicating
// platform-specific executable-detection logic (unix permission bits vs.
// Windows PATHEXT, see passthrough_unix.go/passthrough_windows.go) inside the
// test itself.
//
// Forward is deliberately not covered here at all, not even its failure
// path. Forward has no seam: it always calls the package-level Resolve(),
// which always builds its last-resort fallback from runtime.GOROOT(). That
// fallback is fixed at process start (runtime/env_posix.go snapshots the OS
// environment into an internal "envs" slice before any Go code runs, and
// runtime.GOROOT() reads only that snapshot — confirmed empirically that
// os.Setenv/t.Setenv during the test have no effect on it) and, in any
// environment where `go test` could build and launch this test binary at
// all, that fallback GOROOT is essentially guaranteed to contain a real,
// executable "go". So forcing Resolve() to fail deterministically from
// inside a test is not achievable through GOROOT/PATH env manipulation. The
// only way to force it would be to remove or rename the real, live GOROOT's
// bin/go on disk — a destructive, process-wide filesystem change (not scoped
// to this test binary) that could break any other test process or `go`
// invocation running concurrently on the same machine or CI runner, which is
// unacceptable collateral for a unit test. Worse, calling Forward at all
// without that guarantee risks Resolve() succeeding, in which case Forward
// reaches Run's successful syscall.Exec and replaces the test binary
// mid-run — exactly the catastrophic outcome this package's tests must never
// risk. So Forward is left untested; execError and Run's failure path (both
// exercised below) cover everything reachable without that risk.
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

// TestResolverResolve covers resolver.resolve()'s discovery-order rules: each
// case builds a resolver from fake goroot/path/fallback/self fields (never the
// real process environment) and asserts on the result. All cases but
// "IgnoresEmptyPATHEntries" are independent of process-global state and run in
// parallel; that one calls chdir, which mutates the real working directory, so
// it stays serial per the same rule that forbids combining t.Setenv with
// t.Parallel().
func TestResolverResolve(t *testing.T) {
	cases := []struct {
		name     string
		parallel bool
		run      func(t *testing.T)
	}{
		{
			name:     "PrefersGOROOTEnvVar",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			// Regression test for the discovery-order risk called out in
			// review: runtime.GOROOT() reports the toolchain turango was
			// built with, so it must never win over the toolchain the user
			// actually has on PATH.
			name:     "PrefersPATHOverRuntimeGOROOT",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			name:     "ScansPATHInOrder",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			// Covers the alias-as-go case: turango renamed to "go" and placed
			// first in PATH must not resolve to itself, or passthrough would
			// recurse forever.
			name:     "SkipsSelfOnPATH",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			// Checks that self-detection compares resolved paths, not literal
			// strings: a symlink named "go" pointing at the turango binary is
			// still ourselves.
			name:     "SkipsSelfViaSymlink",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			name:     "FallsBackToRuntimeGOROOT",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			// GOROOT pointing somewhere that has no toolchain: discovery must
			// continue to PATH rather than returning a path that cannot be
			// executed.
			name:     "IgnoresUnusableGOROOT",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
		{
			name:     "IgnoresEmptyPATHEntries",
			parallel: false, // chdir mutates the real working directory
			run: func(t *testing.T) {
				realDir := t.TempDir()
				want := fakeGo(t, realDir)

				// A "go" in the working directory must not be picked up via
				// an empty PATH entry.
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
			},
		},
		{
			name:     "NotFound",
			parallel: true,
			run: func(t *testing.T) {
				r := resolver{
					goroot:   t.TempDir(),
					path:     joinPath(t.TempDir()),
					fallback: t.TempDir(),
					binName:  goBinaryName,
				}

				if _, err := r.resolve(); !errors.Is(err, ErrNotFound) {
					t.Errorf("resolve() error = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name:     "SkipsDirectoriesNamedGo",
			parallel: true,
			run: func(t *testing.T) {
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
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.parallel {
				t.Parallel()
			}
			tc.run(t)
		})
	}
}

// TestExecError covers execError's pure wrapping behaviour: the returned
// error's message must mention both the failing goBin and the original
// error, and %w must keep the chain reachable via errors.Is/errors.Unwrap so
// callers can still detect e.g. a wrapped os.ErrNotExist.
func TestExecError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		goBin string
		err   error
	}{
		{
			name:  "PlainError",
			goBin: "/usr/bin/go",
			err:   errors.New("boom"),
		},
		{
			name:  "WrappedSentinel",
			goBin: "/nonexistent/bin/go",
			err:   os.ErrNotExist,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := execError(tc.goBin, tc.err)

			if got == nil {
				t.Fatal("execError() = nil, want a non-nil error")
			}
			if !strings.Contains(got.Error(), tc.goBin) {
				t.Errorf("execError().Error() = %q, want it to contain goBin %q", got.Error(), tc.goBin)
			}
			if !strings.Contains(got.Error(), tc.err.Error()) {
				t.Errorf("execError().Error() = %q, want it to contain the original error %q", got.Error(), tc.err.Error())
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("errors.Is(execError(...), %v) = false, want true: execError must wrap with %%w", tc.err)
			}
			if unwrapped := errors.Unwrap(got); unwrapped != tc.err { //nolint:errorlint // asserting exact identity of the wrapped error, not matching against a chain
				t.Errorf("errors.Unwrap(execError(...)) = %v, want %v", unwrapped, tc.err)
			}
		})
	}
}

// TestRunExecFailure covers Run's failure path only: syscall.Exec (unix) /
// cmd.Start (windows) returning an error because goBin cannot be launched at
// all. This is safe to run in-process because exec only replaces the calling
// process on success (see the package's Run doc comment) — every case here
// is constructed so the underlying exec call cannot possibly succeed, so Run
// always returns normally with a non-zero code and a wrapped error instead of
// replacing the test binary.
//
// Run's success path (goBin exists, is executable, and syscall.Exec succeeds)
// is deliberately never exercised by any test in this package: on success it
// replaces the process that called it, which would tear down the test binary
// itself mid-run rather than merely fail the test.
func TestRunExecFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		goBin func(t *testing.T) string
	}{
		{
			name: "NonexistentBinary",
			goBin: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "no-such-go-binary")
			},
		},
		{
			name: "NotExecutable",
			goBin: func(t *testing.T) string {
				t.Helper()

				path := filepath.Join(t.TempDir(), "not-executable")
				if err := os.WriteFile(path, []byte("not a real binary\n"), 0o600); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}

				return path
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			goBin := tc.goBin(t)

			code, err := Run(goBin, nil)

			if err == nil {
				t.Fatalf("Run(%q, nil) error = nil, want a non-nil error: goBin cannot be executed", goBin)
			}
			if code == 0 {
				t.Errorf("Run(%q, nil) code = 0, want a non-zero exit code", goBin)
			}
			if !strings.Contains(err.Error(), goBin) {
				t.Errorf("Run(%q, nil) error = %q, want it to reference goBin (i.e. wrapped via execError)", goBin, err.Error())
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("Run(%q, nil) error = %q, want a wrapped error (execError uses %%w)", goBin, err.Error())
			}
		})
	}
}

// TestResolveUsesLiveEnvironment sanity-checks the exported wrapper against the
// real environment this test is running in, where a go toolchain is guaranteed
// to exist. It can't share TestResolverResolve's fake-resolver table shape
// because it deliberately exercises the live process environment rather than
// synthetic data, so it stands alone per rule 7's exception.
func TestResolveUsesLiveEnvironment(t *testing.T) {
	t.Parallel()

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
