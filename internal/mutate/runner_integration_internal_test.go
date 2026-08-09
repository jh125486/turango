//go:build integration

// Integration + whitebox: compileDisassembly and isTCEEquivalent shell out
// to the real Go toolchain (go build -gcflags=-S), so these need the
// "integration" build tag (see engine_integration_test.go's header), and
// they need unexported access, so they're whitebox.
package mutate

import (
	"bytes"
	"path/filepath"
	"testing"
)

// tceFixture is a package with a genuine dead store: total's first write is
// immediately overwritten before ever being read, so a statement/remover
// mutant deleting it is textbook compiler-equivalent — the same fixture
// shape ROADMAP.md gap 2's spike validated by hand.
const tceFixture = `package deadstore

func Sum(vs []int) (total int) {
	total = 999
	total = 0
	for _, v := range vs {
		total += v
	}
	return
}
`

// tceFixtureDiff changes total's real initial value (0 -> 1), a genuine
// behavioral difference every build should still catch even after
// [positionComment] strips line numbers.
const tceFixtureDiff = `package deadstore

func Sum(vs []int) (total int) {
	total = 999
	total = 1
	for _, v := range vs {
		total += v
	}
	return
}
`

func tceModule(t *testing.T, src string) string {
	t.Helper()

	root := t.TempDir()

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"):       "module example.com/tce\n\ngo 1.26\n",
		filepath.Join(root, "deadstore.go"): src,
	})

	return root
}

// TestCompileDisassemblyReproducible pins the trimpath/fixed-buildid half of
// the spike: identical source, built from two different temp-dir copies,
// must produce identical normalized disassembly. This is the property TCE's
// whole reproducibility argument depends on.
func TestCompileDisassemblyReproducible(t *testing.T) {
	t.Parallel()

	rootA := tceModule(t, tceFixture)
	rootB := tceModule(t, tceFixture)

	asmA, err := compileDisassembly(t.Context(), "go", rootA, ".")
	if err != nil {
		t.Fatalf("compileDisassembly(A) error = %v", err)
	}

	asmB, err := compileDisassembly(t.Context(), "go", rootB, ".")
	if err != nil {
		t.Fatalf("compileDisassembly(B) error = %v", err)
	}

	if len(asmA) == 0 {
		t.Fatal("compileDisassembly() returned no output")
	}

	if !bytes.Equal(asmA, asmB) {
		t.Errorf("compileDisassembly() differed across two temp-dir copies of identical source")
	}
}

// TestIsTCEEquivalent covers the actual filter, both directions: a genuine
// dead-store removal must compare equal to the baseline, and a genuine
// behavioral change must not — the dual-mode assertion ROADMAP.md gap 2's
// Verification section calls for.
func TestIsTCEEquivalent(t *testing.T) {
	t.Parallel()

	baselineRoot := tceModule(t, tceFixture)

	baseline, err := compileDisassembly(t.Context(), "go", baselineRoot, ".")
	if err != nil {
		t.Fatalf("compileDisassembly(baseline) error = %v", err)
	}

	if len(baseline) == 0 {
		t.Fatal("compileDisassembly(baseline) returned no output")
	}

	t.Run("dead store removed", func(t *testing.T) {
		t.Parallel()

		// The mutant: total's dead first write deleted entirely, matching
		// what a real statement/remover mutation would leave behind.
		equivRoot := tceModule(t, `package deadstore

func Sum(vs []int) (total int) {
	total = 0
	for _, v := range vs {
		total += v
	}
	return
}
`)

		m := mutant{moduleDir: baselineRoot, pkgDir: baselineRoot, tceBaseline: baseline}
		if !isTCEEquivalent(t.Context(), "go", equivRoot, m) {
			t.Error("isTCEEquivalent() = false, want true: this is a textbook dead-store elimination")
		}
	})

	t.Run("real behavior change", func(t *testing.T) {
		t.Parallel()

		diffRoot := tceModule(t, tceFixtureDiff)

		m := mutant{moduleDir: baselineRoot, pkgDir: baselineRoot, tceBaseline: baseline}
		if isTCEEquivalent(t.Context(), "go", diffRoot, m) {
			t.Error("isTCEEquivalent() = true, want false: total's real initial value changed")
		}
	})

	t.Run("no baseline means inactive", func(t *testing.T) {
		t.Parallel()

		m := mutant{moduleDir: baselineRoot, pkgDir: baselineRoot, tceBaseline: nil}
		if isTCEEquivalent(t.Context(), "go", baselineRoot, m) {
			t.Error("isTCEEquivalent() = true with a nil baseline, want false (TCE inactive)")
		}
	})
}
