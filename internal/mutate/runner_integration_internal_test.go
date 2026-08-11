//go:build integration

// Integration + whitebox: compileDisassembly, isTCEEquivalent and
// runner.run itself shell out to the real Go toolchain (go build
// -gcflags=-S, go test), so these need the "integration" build tag (see
// engine_integration_test.go's header), and they need unexported access,
// so they're whitebox.
package mutate

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/mutator"
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

// literalFixture bundles everything TestRunClosesSameMutantIDDifferentContentCollision
// needs to build a [mutant] by hand against a real, throwaway module —
// [literalMutationModule]'s return shape, grouped into one value rather
// than returned as a long tuple.
type literalFixture struct {
	root, path string
	fset       *token.FileSet
	file       *ast.File
	baseline   []byte
	mutation   mutator.Mutation
}

// literalMutationModule builds a real, tiny module containing one function
// `func F() int { return <literal> }`, optionally with a test asserting
// F() == <literal>, and returns a [literalFixture] carrying a
// [mutator.Mutation] that flips the literal's printed value to
// <literal>+1 and back — the same in-place BasicLit.Value edit idiom
// literal/number.go's own shiftBy uses, hand-built here (rather than going
// through the real operator) so the test controls exactly what changes,
// matching TestRunSkipsNoOpMutation's own precedent for constructing a
// mutation by hand.
//
// withTest selects whether f_test.go asserts F()'s value: with it, the
// literal flip is a real behavioral change a test notices (Killed);
// without it, the package has no test files at all, so nothing could ever
// notice any mutation (Survived) — [classify]'s own documented handling of
// a package with no tests.
func literalMutationModule(t *testing.T, literal int, withTest bool) literalFixture {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/collide\n\ngo 1.26\n",
		filepath.Join(root, "f.go"):   fmt.Sprintf("package p\n\nfunc F() int {\n\treturn %d\n}\n", literal),
	}

	if withTest {
		files[filepath.Join(root, "f_test.go")] = fmt.Sprintf(
			"package p\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != %d {\n\t\tt.Fatalf(\"F() != %d\")\n\t}\n}\n",
			literal, literal)
	}

	writeFiles(t, files)

	path := filepath.Join(root, "f.go")
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile(%s) error = %v", path, err)
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, file); err != nil {
		t.Fatalf("Fprint() error = %v", err)
	}

	baseline := buf.Bytes()

	var lit *ast.BasicLit

	ast.Inspect(file, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.INT {
			lit = bl

			return false
		}

		return true
	})

	if lit == nil {
		t.Fatalf("literalMutationModule: no int literal found in %s", path)
	}

	original := lit.Value
	shifted := fmt.Sprintf("%d", literal+1)

	mutation := mutator.Mutation{
		Description: original + " -> " + shifted,
		Apply:       func() { lit.Value = shifted },
		Revert:      func() { lit.Value = original },
	}

	return literalFixture{root: root, path: path, fset: fset, file: file, baseline: baseline, mutation: mutation}
}

// TestRunClosesSameMutantIDDifferentContentCollision is ROADMAP.md gap
// 12's single most important test: it directly disproves "mutantID alone
// is a safe cache key" by constructing exactly the dangerous case 12a's
// design reasoning describes — two mutations sharing an identical
// mutantID (here, forced directly onto the mutant value, the same way a
// same-position same-width literal edit would produce it via the real
// walk) but genuinely different content and genuinely different real
// verdicts — and asserts the second is never served the first's cached
// verdict.
//
// Variant A: a module where the mutation (a literal flip) is a real
// behavioral change a test catches — Killed. Variant B: a module,
// otherwise built the identical way, with no test file at all — nothing
// could ever catch any mutation — Survived. If a cache keyed on mutantID
// alone were in play, B's lookup would incorrectly hit A's cached Killed
// record; this test proves that does not happen: B's own cacheFingerprint
// (a real content hash of its own, different module) misses A's cache
// entry entirely, a real go test actually runs for B, and B's verdict is
// its own true Survived, not A's stale Killed.
func TestRunClosesSameMutantIDDifferentContentCollision(t *testing.T) {
	t.Parallel()

	const sharedID = "collide0000"

	fixtureA := literalMutationModule(t, 1, true)
	fixtureB := literalMutationModule(t, 1, false)

	fpA, err := cacheFingerprint(fixtureA.root, nil)
	if err != nil {
		t.Fatalf("cacheFingerprint(A) error = %v", err)
	}

	fpB, err := cacheFingerprint(fixtureB.root, nil)
	if err != nil {
		t.Fatalf("cacheFingerprint(B) error = %v", err)
	}

	if fpA == fpB {
		t.Fatal("test setup problem: variant A and B produced the same fingerprint, want different (they are different modules)")
	}

	cacheDir := t.TempDir()

	// "Populate the cache against variant A": a first runner, with no
	// pre-existing cache to read from, records A's real verdict.
	storeA, err := newCacheStore(cachePath(cacheDir))
	if err != nil {
		t.Fatalf("newCacheStore() error = %v", err)
	}

	rA := &runner{goBin: "go", testTimeout: time.Minute, toolchain: "test-toolchain", store: storeA}

	mA := mutant{
		fset: fixtureA.fset, file: fixtureA.file, path: fixtureA.path, baseline: fixtureA.baseline,
		moduleDir: fixtureA.root, pkgDir: fixtureA.root, scope: ScopeFull,
		id: sharedID, cacheFingerprint: fpA, mutation: fixtureA.mutation,
	}

	resA, okA, eqA, err := rA.run(t.Context(), mA)
	if err != nil {
		t.Fatalf("run(A) error = %v", err)
	}

	if !okA || eqA {
		t.Fatalf("run(A) ok = %v, equivalent = %v, want ok=true, equivalent=false", okA, eqA)
	}

	if resA.Status != Killed {
		t.Fatalf("run(A) Status = %v, want %v (test setup problem, not the property under test)", resA.Status, Killed)
	}

	if err := storeA.close(); err != nil {
		t.Fatalf("storeA.close() error = %v", err)
	}

	// "Run against variant B with the same CacheDir": a second runner loads
	// the index storeA just wrote, exactly as engine.Run does between two
	// separate invocations.
	idx, err := loadCacheIndex(cachePath(cacheDir))
	if err != nil {
		t.Fatalf("loadCacheIndex() error = %v", err)
	}

	storeB, err := newCacheStore(cachePath(cacheDir))
	if err != nil {
		t.Fatalf("newCacheStore() (second open) error = %v", err)
	}

	defer func() { _ = storeB.close() }()

	rB := &runner{goBin: "go", testTimeout: time.Minute, toolchain: "test-toolchain", cache: idx, store: storeB}

	mB := mutant{
		fset: fixtureB.fset, file: fixtureB.file, path: fixtureB.path, baseline: fixtureB.baseline,
		moduleDir: fixtureB.root, pkgDir: fixtureB.root, scope: ScopeFull,
		id: sharedID, cacheFingerprint: fpB, mutation: fixtureB.mutation,
	}

	before := execCalls.Load()

	resB, okB, eqB, err := rB.run(t.Context(), mB)
	if err != nil {
		t.Fatalf("run(B) error = %v", err)
	}

	after := execCalls.Load()

	if !okB || eqB {
		t.Fatalf("run(B) ok = %v, equivalent = %v, want ok=true, equivalent=false", okB, eqB)
	}

	if after <= before {
		t.Error("run(B) spawned no go test subprocess: it was served a cached verdict despite sharing A's mutantID and a different Fingerprint — mutantID alone would be an unsafe cache key")
	}

	if resB.Status != Survived {
		t.Errorf("run(B) Status = %v, want %v (its own true verdict) — %v would mean A's stale cached verdict was wrongly served", resB.Status, Survived, Killed)
	}
}
