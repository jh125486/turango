//go:build !windows

package mutate

import "os"

// cacheRunningAsRoot reports whether the test process is root, isolated to a
// unix-only file since os.Geteuid is undefined on Windows — see
// cacheSkipIfPrivileged's own doc comment in cache_internal_test.go for why
// this check exists at all.
func cacheRunningAsRoot() bool {
	return os.Geteuid() == 0
}
