// Stand-in for crypto/internal/constanttime, avoiding a dependency on that
// stdlib-internal package for this isolated mutation-testing fixture.
// Same simple boolean semantics as the real function.
package constanttime

func ByteEq(x, y byte) int {
	if x == y {
		return 1
	}
	return 0
}
