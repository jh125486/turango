//go:build windows

package mutate

// cacheRunningAsRoot always reports false on Windows: there is no direct
// analogue to a Unix euid==0 check, and cacheSkipIfPrivileged already skips
// unconditionally on Windows before this would matter — see its doc comment
// in cache_internal_test.go.
func cacheRunningAsRoot() bool {
	return false
}
