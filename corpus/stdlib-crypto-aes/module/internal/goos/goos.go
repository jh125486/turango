// Minimal stand-in for internal/goos, for this isolated mutation-testing
// fixture.
package goos

import "runtime"

var GOOS = runtime.GOOS
