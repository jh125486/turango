//go:build !windows

package mutate

import "syscall"

// runnerMkfifo creates a named pipe at path, isolated to a unix-only file
// since syscall.Mkfifo is undefined on Windows — see its callers in
// runner_internal_test.go for why the tests using it treat a non-nil error
// as "unsupported on this platform," not a test failure.
func runnerMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
