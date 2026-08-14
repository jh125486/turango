//go:build windows

package mutate

import "errors"

// runnerMkfifo always errors on Windows, which has no named-pipe-on-the-
// filesystem equivalent to Unix mkfifo — see its callers in
// runner_internal_test.go, which already treat a non-nil error as
// "unsupported on this platform," not a test failure.
func runnerMkfifo(string, uint32) error {
	return errors.New("mkfifo not supported on windows")
}
