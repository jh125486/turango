// Stand-in for the real indicator.go, which uses //go:linkname to reach a
// runtime-privileged per-goroutine slot only the actual crypto/internal/fips140
// package is allowed to link against. That's FIPS compliance bookkeeping, not
// part of AES's cipher logic, so a simple package-level variable is a
// sufficient substitute for this isolated mutation-testing fixture (test
// execution here is single-threaded, so goroutine-locality doesn't matter).
package fips140

var indicatorState uint8 = indicatorUnset

const (
	indicatorUnset uint8 = iota
	indicatorFalse
	indicatorTrue
)

func ResetServiceIndicator() { indicatorState = indicatorUnset }

func ServiceIndicator() bool { return indicatorState == indicatorTrue }

func RecordApproved() {
	if indicatorState == indicatorUnset {
		indicatorState = indicatorTrue
	}
}

func RecordNonApproved() { indicatorState = indicatorFalse }
