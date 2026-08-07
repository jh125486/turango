// Minimal stand-in for internal/goarch, for this isolated mutation-testing
// fixture. GOARCH is set to the actual runtime architecture so
// implementations.go's per-arch dispatch behaves correctly.
package goarch

import "runtime"

var GOARCH = runtime.GOARCH

const (
	IsAmd64   = 0
	IsArm64   = 0
	IsPpc64   = 0
	IsPpc64le = 0
	BigEndian = 0
)
