// Package goproxy locates the real Go toolchain binary and forwards an
// invocation to it.
//
// Discovery order is deliberate and load-bearing (see the "Shim Architecture"
// section of the design):
//
//  1. The GOROOT environment variable, if set → $GOROOT/bin/go.
//  2. A PATH scan for "go", skipping any entry that resolves to turango's own
//     executable. That skip is what makes it safe to rename or symlink turango
//     to "go" and put it ahead of the real toolchain in PATH without recursing
//     into ourselves forever.
//  3. runtime.GOROOT()/bin/go, as a last-resort fallback only.
//
// runtime.GOROOT() is deliberately last: it reports the toolchain turango was
// *built* with, so preferring it would silently pin every user to the build
// machine's Go version instead of the one they have installed.
package goproxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNotFound is returned when no usable real "go" binary can be located.
var ErrNotFound = errors.New("goproxy: unable to locate the real go toolchain binary")

// goosWindows is runtime.GOOS's value on Windows.
const goosWindows = "windows"

// goBinaryName is the name of the Go toolchain's frontend binary.
var goBinaryName = func() string {
	if runtime.GOOS == goosWindows {
		return "go.exe"
	}

	return "go"
}()

// resolver holds everything discovery depends on, so tests can drive the
// ordering rules without mutating the real process environment.
type resolver struct {
	// goroot is the value of the GOROOT environment variable, empty if unset.
	goroot string
	// path is the value of the PATH environment variable.
	path string
	// self is the absolute path of turango's own executable. Empty if unknown,
	// in which case no PATH entry is skipped.
	self string
	// fallback is runtime.GOROOT(), used only as a last resort.
	fallback string
	// binName is the toolchain binary's filename ("go" or "go.exe").
	binName string
}

// newResolver builds a resolver from the live process environment.
func newResolver() resolver {
	self, err := os.Executable()
	if err != nil {
		// Not fatal: without a known self path we simply cannot skip ourselves
		// during the PATH scan, which only matters under the experimental
		// alias-as-go mode.
		self = ""
	}

	return resolver{
		goroot: os.Getenv("GOROOT"),
		path:   os.Getenv("PATH"),
		self:   self,
		//nolint:staticcheck // deliberate last-resort fallback, see the package doc comment above: runtime.GOROOT() is the toolchain turango was *built* with, used only when GOROOT and PATH both fail; "go env GOROOT" needs a "go" binary already found, which is circular here.
		fallback: runtime.GOROOT(),
		binName:  goBinaryName,
	}
}

// resolve implements the documented discovery order.
func (r resolver) resolve() (string, error) {
	// 1. GOROOT environment variable.
	if root := strings.TrimSpace(r.goroot); root != "" {
		candidate := filepath.Join(root, "bin", r.binName)
		if isExecutable(candidate) && !r.isSelf(candidate) {
			return candidate, nil
		}
	}

	// 2. PATH scan, skipping ourselves.
	for _, dir := range filepath.SplitList(r.path) {
		if dir == "" {
			// POSIX treats an empty PATH entry as the working directory. Go's
			// own exec.LookPath resolves it to ".", but picking up a "go" from
			// the current directory is a surprising (and unsafe) thing for a
			// toolchain shim to do, so skip it.
			continue
		}

		candidate := filepath.Join(dir, r.binName)
		if !isExecutable(candidate) {
			continue
		}
		if r.isSelf(candidate) {
			continue
		}

		return candidate, nil
	}

	// 3. runtime.GOROOT(), last resort.
	if r.fallback != "" {
		candidate := filepath.Join(r.fallback, "bin", r.binName)
		if isExecutable(candidate) && !r.isSelf(candidate) {
			return candidate, nil
		}
	}

	return "", ErrNotFound
}

// isSelf reports whether candidate resolves to turango's own executable.
// Comparison is done on fully resolved absolute paths so that a symlink named
// "go" pointing at the turango binary is still recognised as ourselves.
func (r resolver) isSelf(candidate string) bool {
	if r.self == "" {
		return false
	}

	return samePath(candidate, r.self)
}

// samePath reports whether two paths refer to the same file, comparing by
// resolved absolute path.
func samePath(a, b string) bool {
	ra, err := realPath(a)
	if err != nil {
		return false
	}

	rb, err := realPath(b)
	if err != nil {
		return false
	}

	if runtime.GOOS == goosWindows {
		return strings.EqualFold(ra, rb)
	}

	return ra == rb
}

// realPath returns the absolute, symlink-free form of p.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The path may not exist (or may not be readable); fall back to the
		// absolute form so callers still get a usable comparison key.
		return abs, nil //nolint:nilerr // best-effort resolution is intended
	}

	return resolved, nil
}

// Resolve locates the real Go toolchain binary using the documented discovery
// order. The returned path is suitable for direct execution.
func Resolve() (string, error) {
	return newResolver().resolve()
}

// Forward locates the real Go toolchain and hands the given arguments to it,
// verbatim, with the current environment, working directory and standard
// streams.
//
// On Unix this replaces the current process (execve), so on success Forward
// never returns. On Windows, which has no process-replacement primitive, it
// runs the toolchain as a child, forwards console interrupts to it, waits, and
// returns the child's exit code.
func Forward(args []string) (int, error) {
	goBin, err := Resolve()
	if err != nil {
		return 1, err
	}

	return Run(goBin, args)
}

// execError wraps a failure to start the real toolchain at all (missing
// binary, bad permissions) as distinct from the toolchain running and exiting
// non-zero.
func execError(goBin string, err error) error {
	return fmt.Errorf("goproxy: failed to execute %s: %w", goBin, err)
}
