//go:build !windows

package goproxy

import (
	"os"
	"syscall"
)

// isExecutable reports whether path is a regular file with at least one
// execute bit set.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}

	return info.Mode().Perm()&0o111 != 0
}

// Run replaces the current process with the real Go toolchain.
//
// syscall.Exec hands the kernel our pid, so the toolchain inherits stdin,
// stdout, stderr, the working directory, the environment, the process group and
// every signal disposition for free — its exit code becomes ours with no
// translation, and Ctrl+C reaches it directly. On success this never returns;
// the (int, error) signature exists so callers can share one code path with the
// Windows implementation.
func Run(goBin string, args []string) (int, error) {
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, goBin)
	argv = append(argv, args...)

	if err := syscall.Exec(goBin, argv, os.Environ()); err != nil {
		return 1, execError(goBin, err)
	}

	// Unreachable: syscall.Exec only returns on failure.
	return 0, nil
}
