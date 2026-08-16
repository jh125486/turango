// Whitebox: cache.go exports no identifiers at all — cacheKey, cacheRecord,
// cacheFingerprint, cacheIndex, cacheStore and the rest are all
// unexported — so every test here needs direct package access, the same
// justification runner_internal_test.go's own header already gives for
// itself. resolveToolchain, the one function here that shells out to the
// real Go toolchain, is tested separately in
// cache_integration_internal_test.go, behind the "integration" build tag —
// see that file's header for why, mirroring
// runner_integration_internal_test.go's split from runner_internal_test.go.
package mutate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCachePath covers the one thing main.go's -mutatecache flag actually
// depends on: the cache always lives at a fixed, predictable name inside
// the directory the user named.
func TestCachePath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dir  string
		want string
	}{
		"ordinary directory": {dir: "/tmp/mycache", want: "/tmp/mycache/mutate-cache.jsonl"},
		"relative directory": {dir: "cache", want: "cache/mutate-cache.jsonl"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := cachePath(tt.dir); got != filepath.FromSlash(tt.want) {
				t.Errorf("cachePath(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

// writeFingerprintModule builds a minimal, real module — go.mod plus one
// package file — under a fresh temp directory and returns its root. Used
// by TestCacheFingerprint's whole-module (dirs == nil) cases, which need
// two genuinely separate temp-dir copies the way
// TestCompileDisassemblyReproducible does for the analogous TCE claim.
func writeFingerprintModule(t *testing.T, src string) string {
	t.Helper()

	root := t.TempDir()

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"): "module example.com/fingerprint\n\ngo 1.23\n",
		filepath.Join(root, "p.go"):   src,
	})

	return root
}

// narrowFixture builds a module with two same-module packages, "a" (the
// closure) and "b" (a sibling the closure does not include) — the concrete
// stand-in for "this mutant's package never imports that sibling package"
// that makes a narrow-scope fingerprint's whole point checkable by
// TestCacheFingerprint's second table below.
func narrowFixture(t *testing.T) (root string, dirs map[string]bool) {
	t.Helper()

	root = t.TempDir()

	writeFiles(t, map[string]string{
		filepath.Join(root, "go.mod"):    "module example.com/fingerprint\n\ngo 1.23\n",
		filepath.Join(root, "a", "a.go"): "package a\n\nconst X = 1\n",
		filepath.Join(root, "b", "b.go"): "package b\n\nconst Y = 1\n",
	})

	return root, map[string]bool{filepath.Join(root, "a"): true}
}

// TestCacheFingerprint covers the persistent cache's central correctness
// claim at the fingerprint layer: identical content hashes equal
// regardless of which temp directory it happens to live in, any change
// inside the relevant file set changes the digest, and — the scope
// restriction that is the entire point of not always fingerprinting the
// whole module — a change to a file outside a narrow closure leaves the
// digest untouched.
//
// Split into two tables rather than one, since the two groups genuinely
// differ in shape: the first compares two independently-built module
// copies (no shared fixture to edit in place), the second edits one fixed
// fixture between two calls (either the whole module, or a narrow closure
// within it) — flattening both into a single table would need a
// least-common-denominator shape neither group actually has.
func TestCacheFingerprint(t *testing.T) {
	t.Parallel()

	t.Run("whole module: two independently-built copies", func(t *testing.T) {
		t.Parallel()

		tests := map[string]struct {
			srcA, srcB string
			wantEqual  bool
		}{
			"identical content is equal": {
				srcA: "package p\n\nconst X = 1\n", srcB: "package p\n\nconst X = 1\n", wantEqual: true,
			},
			"a changed byte is different": {
				srcA: "package p\n\nconst X = 1\n", srcB: "package p\n\nconst X = 2\n", wantEqual: false,
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				rootA := writeFingerprintModule(t, tt.srcA)
				rootB := writeFingerprintModule(t, tt.srcB)

				fpA, err := cacheFingerprint(rootA, nil)
				if err != nil {
					t.Fatalf("cacheFingerprint(A) error = %v", err)
				}

				fpB, err := cacheFingerprint(rootB, nil)
				if err != nil {
					t.Fatalf("cacheFingerprint(B) error = %v", err)
				}

				if fpA == "" {
					t.Fatal("cacheFingerprint() returned an empty digest")
				}

				if got := fpA == fpB; got != tt.wantEqual {
					t.Errorf("cacheFingerprint(A) == cacheFingerprint(B) = %v, want %v", got, tt.wantEqual)
				}
			})
		}
	})

	t.Run("a change between two calls against the same fixture", func(t *testing.T) {
		t.Parallel()

		tests := map[string]struct {
			narrow      bool // narrow (dirs != nil, copyClosure's file set) vs whole-module (dirs == nil) fingerprinting
			edit        func(root string) map[string]string
			wantChanged bool
		}{
			"whole module: adding .git is excluded, mirroring copyTree": {
				edit: func(root string) map[string]string {
					return map[string]string{filepath.Join(root, ".git", "HEAD"): "ref: refs/heads/main\n"}
				},
				wantChanged: false,
			},
			"narrow scope: a change inside the closure changes the fingerprint": {
				narrow: true,
				edit: func(root string) map[string]string {
					return map[string]string{filepath.Join(root, "a", "a.go"): "package a\n\nconst X = 2\n"}
				},
				wantChanged: true,
			},
			"narrow scope: a change to go.mod changes the fingerprint": {
				narrow: true,
				edit: func(root string) map[string]string {
					return map[string]string{filepath.Join(root, "go.mod"): "module example.com/fingerprint\n\ngo 1.24\n"}
				},
				wantChanged: true,
			},
			"narrow scope: a change outside the closure leaves the fingerprint unchanged": {
				narrow: true,
				edit: func(root string) map[string]string {
					return map[string]string{filepath.Join(root, "b", "b.go"): "package b\n\nconst Y = 2\n"}
				},
				wantChanged: false,
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var (
					root string
					dirs map[string]bool
				)

				if tt.narrow {
					root, dirs = narrowFixture(t)
				} else {
					root = writeFingerprintModule(t, "package p\n\nconst X = 1\n")
				}

				before, err := cacheFingerprint(root, dirs)
				if err != nil {
					t.Fatalf("cacheFingerprint() (before) error = %v", err)
				}

				writeFiles(t, tt.edit(root))

				after, err := cacheFingerprint(root, dirs)
				if err != nil {
					t.Fatalf("cacheFingerprint() (after) error = %v", err)
				}

				if changed := before != after; changed != tt.wantChanged {
					t.Errorf("cacheFingerprint() changed = %v, want %v (before = %q, after = %q)", changed, tt.wantChanged, before, after)
				}
			})
		}
	})

	t.Run("a nonexistent directory is a real error, not a silent empty digest", func(t *testing.T) {
		t.Parallel()

		if _, err := cacheFingerprint(filepath.Join(t.TempDir(), "does-not-exist"), nil); err == nil {
			t.Error("cacheFingerprint() error = nil for a nonexistent directory, want an error")
		}
	})

	t.Run("a fingerprintFile failure on one walked file propagates out of cacheFingerprint itself", func(t *testing.T) {
		t.Parallel()

		cacheSkipIfPrivileged(t)

		root := writeFingerprintModule(t, "package p\n\nconst X = 1\n")
		secret := filepath.Join(root, "secret.go")

		if err := os.WriteFile(secret, []byte("package p\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", secret, err)
		}

		if err := os.Chmod(secret, 0o000); err != nil {
			t.Fatalf("Chmod(%s) error = %v", secret, err)
		}

		t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

		if _, err := cacheFingerprint(root, nil); err == nil {
			t.Error("cacheFingerprint() error = nil for a module containing an unreadable file, want the fingerprintFile error to propagate")
		}
	})
}

// TestLoadCacheIndex covers loadCacheIndex/cacheStore's shared contract:
// a missing cache file is a clean, empty first run rather than an error; a
// prior run's complete records round-trip through cacheStore and back
// through loadCacheIndex intact; and — the load-bearing recovery
// property — a torn trailing write left behind by a killed process
// (simulated by appending a truncated, syntactically-invalid line directly
// to the file, bypassing cacheStore entirely) is silently dropped and the
// file physically truncated on disk, after which a fresh cacheStore can
// still append further records cleanly.
func TestLoadCacheIndex(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T){
		"missing file is an empty index, not an error":                              testLoadCacheMissing,
		"nil index answers every lookup with ok=false":                              testLoadCacheNil,
		"complete records round-trip, and recovery truncates a torn trailing write": testLoadCacheRecovery,
		"a later record for the same key wins":                                      testLoadCacheLatestRecord,
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t)
		})
	}
}

func testLoadCacheMissing(t *testing.T) {
	t.Helper()

	idx, err := loadCacheIndex(filepath.Join(t.TempDir(), "mutate-cache.jsonl"))
	if err != nil {
		t.Fatalf("loadCacheIndex() error = %v", err)
	}
	if _, ok := idx.get(cacheKey{MutantID: "whatever"}); ok {
		t.Error("loadCacheIndex() on a missing file answered a lookup with ok=true, want false")
	}
}

func testLoadCacheNil(t *testing.T) {
	t.Helper()
	var idx *cacheIndex
	if _, ok := idx.get(cacheKey{}); ok {
		t.Error("(*cacheIndex)(nil).get() ok = true, want false")
	}
}

func testLoadCacheRecovery(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mutate-cache.jsonl")
	store, err := newCacheStore(path)
	if err != nil {
		t.Fatalf("newCacheStore() error = %v", err)
	}
	want := []cacheRecord{
		{Key: cacheKey{MutantID: "a", Fingerprint: "f1"}, Status: Killed, Output: "boom"},
		{Key: cacheKey{MutantID: "b", Fingerprint: "f1"}, Status: Survived},
		{Key: cacheKey{MutantID: "c", Fingerprint: "f1"}, Equivalent: true},
	}
	for _, rec := range want {
		store.record(rec)
	}
	if err := store.close(); err != nil {
		t.Fatalf("cacheStore.close() error = %v", err)
	}
	clean, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	appendCacheTornWrite(t, path)

	idx, err := loadCacheIndex(path)
	if err != nil {
		t.Fatalf("loadCacheIndex() error = %v", err)
	}
	assertCacheRecords(t, idx, want)
	if _, ok := idx.get(cacheKey{MutantID: "d"}); ok {
		t.Error("loadCacheIndex() recovered the torn trailing record, want it dropped")
	}
	gotFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if !bytes.Equal(gotFile, clean) {
		t.Errorf("loadCacheIndex() did not truncate torn write: got %q want %q", gotFile, clean)
	}

	store2, err := newCacheStore(path)
	if err != nil {
		t.Fatalf("newCacheStore() (second open) error = %v", err)
	}
	store2.record(cacheRecord{Key: cacheKey{MutantID: "e"}, Status: Killed})
	if err := store2.close(); err != nil {
		t.Fatalf("cacheStore.close() (second store) error = %v", err)
	}
	idx2, err := loadCacheIndex(path)
	if err != nil {
		t.Fatalf("loadCacheIndex() (after second append) error = %v", err)
	}
	assertCacheRecords(t, idx2, want)
	if _, ok := idx2.get(cacheKey{MutantID: "e"}); !ok {
		t.Error("loadCacheIndex() did not see record appended after recovery")
	}
}

func appendCacheTornWrite(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%s) error = %v", path, err)
	}
	if _, err := f.WriteString("{\"Key\":{\"MutantID\":\"d\"},\"Stat"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func assertCacheRecords(t *testing.T, idx *cacheIndex, want []cacheRecord) {
	t.Helper()
	for _, rec := range want {
		got, ok := idx.get(rec.Key)
		if !ok {
			t.Errorf("loadCacheIndex() missing record for key %+v", rec.Key)
			continue
		}
		if got != rec {
			t.Errorf("loadCacheIndex() record for key %+v = %+v, want %v", rec.Key, got, rec)
		}
	}
}

func testLoadCacheLatestRecord(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mutate-cache.jsonl")
	store, err := newCacheStore(path)
	if err != nil {
		t.Fatalf("newCacheStore() error = %v", err)
	}
	key := cacheKey{MutantID: "same", Fingerprint: "f1"}
	store.record(cacheRecord{Key: key, Status: Survived})
	store.record(cacheRecord{Key: key, Status: Killed, Output: "later"})
	if err := store.close(); err != nil {
		t.Fatalf("cacheStore.close() error = %v", err)
	}
	idx, err := loadCacheIndex(path)
	if err != nil {
		t.Fatalf("loadCacheIndex() error = %v", err)
	}
	got, ok := idx.get(key)
	if !ok {
		t.Fatal("loadCacheIndex() missing the record entirely")
	}
	if got.Status != Killed || got.Output != "later" {
		t.Errorf("loadCacheIndex() kept earlier record: got %+v", got)
	}
}

/*

	t.Run("missing file is an empty index, not an error", func(t *testing.T) {
		t.Parallel()

		idx, err := loadCacheIndex(filepath.Join(t.TempDir(), "mutate-cache.jsonl"))
		if err != nil {
			t.Fatalf("loadCacheIndex() error = %v", err)
		}

		if _, ok := idx.get(cacheKey{MutantID: "whatever"}); ok {
			t.Error("loadCacheIndex() on a missing file answered a lookup with ok=true, want false")
		}
	})

	t.Run("nil index answers every lookup with ok=false", func(t *testing.T) {
		t.Parallel()

		var idx *cacheIndex

		if _, ok := idx.get(cacheKey{}); ok {
			t.Error("(*cacheIndex)(nil).get() ok = true, want false")
		}
	})

	t.Run("complete records round-trip, and recovery truncates a torn trailing write", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mutate-cache.jsonl")

		store, err := newCacheStore(path)
		if err != nil {
			t.Fatalf("newCacheStore() error = %v", err)
		}

		want := []cacheRecord{
			{Key: cacheKey{MutantID: "a", Fingerprint: "f1"}, Status: Killed, Output: "boom"},
			{Key: cacheKey{MutantID: "b", Fingerprint: "f1"}, Status: Survived},
			{Key: cacheKey{MutantID: "c", Fingerprint: "f1"}, Equivalent: true},
		}

		for _, rec := range want {
			store.record(rec)
		}

		if err := store.close(); err != nil {
			t.Fatalf("cacheStore.close() error = %v", err)
		}

		clean, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}

		// Simulate a kill mid-write: append a truncated, syntactically
		// invalid trailing line directly to the file, bypassing cacheStore
		// entirely — this is deliberately not "a well-formed 4th record,"
		// it's exactly the shape a process killed mid-os.File.Write would
		// leave behind (per cacheStore.consume's own doc comment: never two
		// interleaved records, only the last one torn).
		torn := "{\"Key\":{\"MutantID\":\"d\"},\"Stat"

		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("OpenFile(%s) error = %v", path, err)
		}

		if _, err := f.WriteString(torn); err != nil {
			t.Fatalf("WriteString() error = %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		idx, err := loadCacheIndex(path)
		if err != nil {
			t.Fatalf("loadCacheIndex() error = %v", err)
		}

		for _, rec := range want {
			got, ok := idx.get(rec.Key)
			if !ok {
				t.Errorf("loadCacheIndex() missing record for key %+v", rec.Key)

				continue
			}

			if got != rec {
				t.Errorf("loadCacheIndex() record for key %+v = %+v, want %+v", rec.Key, got, rec)
			}
		}

		if _, ok := idx.get(cacheKey{MutantID: "d"}); ok {
			t.Error("loadCacheIndex() recovered the torn trailing record, want it dropped")
		}

		gotFile, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}

		if !bytes.Equal(gotFile, clean) {
			t.Errorf("loadCacheIndex() did not truncate the torn write back to the clean N-record file:\ngot:  %q\nwant: %q", gotFile, clean)
		}

		// A fresh cacheStore on the now-truncated file must still append
		// cleanly — the recovery must leave the on-disk invariant
		// (complete lines, nothing else) genuinely intact, not merely
		// readable once.
		store2, err := newCacheStore(path)
		if err != nil {
			t.Fatalf("newCacheStore() (second open) error = %v", err)
		}

		store2.record(cacheRecord{Key: cacheKey{MutantID: "e"}, Status: Killed})

		if err := store2.close(); err != nil {
			t.Fatalf("cacheStore.close() (second store) error = %v", err)
		}

		idx2, err := loadCacheIndex(path)
		if err != nil {
			t.Fatalf("loadCacheIndex() (after second append) error = %v", err)
		}

		for _, rec := range want {
			if _, ok := idx2.get(rec.Key); !ok {
				t.Errorf("loadCacheIndex() lost a pre-existing record for key %+v after a further append", rec.Key)
			}
		}

		if _, ok := idx2.get(cacheKey{MutantID: "e"}); !ok {
			t.Error("loadCacheIndex() did not see the record appended after recovery")
		}
	})

	t.Run("a later record for the same key wins", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "mutate-cache.jsonl")

		store, err := newCacheStore(path)
		if err != nil {
			t.Fatalf("newCacheStore() error = %v", err)
		}

		key := cacheKey{MutantID: "same", Fingerprint: "f1"}

		store.record(cacheRecord{Key: key, Status: Survived})
		store.record(cacheRecord{Key: key, Status: Killed, Output: "later"})

		if err := store.close(); err != nil {
			t.Fatalf("cacheStore.close() error = %v", err)
		}

		idx, err := loadCacheIndex(path)
		if err != nil {
			t.Fatalf("loadCacheIndex() error = %v", err)
		}

		got, ok := idx.get(key)
		if !ok {
			t.Fatal("loadCacheIndex() missing the record entirely")
		}

		if got.Status != Killed || got.Output != "later" {
			t.Errorf("loadCacheIndex() kept the earlier record: got %+v, want the later Killed/\"later\" one", got)
		}
	})
}
*/

// TestCacheRecordJSONRoundTrip pins the wire shape loadCacheIndex/cacheStore
// depend on: a record — in both its ordinary-verdict and Equivalent
// forms — must decode back to exactly what was encoded, the same
// round-trip property report_test.go's TestResultJSONRoundTrip already
// establishes for MutantResult.
func TestCacheRecordJSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := map[string]cacheRecord{
		"ordinary verdict": {
			Key:    cacheKey{Toolchain: "go1.26 darwin/arm64", Scope: ScopeFull.String(), TCE: true, Fingerprint: "abc123", MutantID: "deadbeef0000"},
			Status: Killed,
			Output: "--- FAIL: TestX",
		},
		"equivalent": {
			Key:        cacheKey{Toolchain: "go1.26 darwin/arm64", Scope: ScopePackage.String(), Fingerprint: "def456", MutantID: "0000deadbeef"},
			Equivalent: true,
		},
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			var got cacheRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}

			if got != want {
				t.Errorf("round-tripped record = %+v, want %+v", got, want)
			}
		})
	}
}

// cacheSkipIfPrivileged skips a permission-dependent subtest under root
// (where every permission check trivially passes, defeating the point of the
// test) and on Windows (where filepath.Rel/os.Chmod's Unix permission-bit
// semantics this file relies on do not hold).
func cacheSkipIfPrivileged(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("permission-bit semantics this test relies on are Unix-specific")
	}

	if cacheRunningAsRoot() {
		t.Skip("running as root: permission checks this test relies on always pass")
	}
}

// cacheFailAfterNWriter is a minimal io.Writer stub that succeeds its first n
// Write calls and fails every call after — the only way to reach
// fingerprintFile's three h.Write error-propagation branches, since the real
// sha256.Writer [cacheFingerprint] always passes it never itself errors.
type cacheFailAfterNWriter struct {
	n     int
	calls int
}

func (w *cacheFailAfterNWriter) Write(p []byte) (int, error) {
	w.calls++

	if w.calls > w.n {
		return 0, errors.New("cacheFailAfterNWriter: injected write error")
	}

	return len(p), nil
}

// TestFingerprintFile covers fingerprintFile directly (rather than only
// through cacheFingerprint's whole-file-set callers), which is the only way
// to reach: a mismatched relative/absolute (base, path) pair, where
// filepath.Rel errors and rel silently falls back to path rather than
// fingerprintFile failing outright; a path that is a directory rather than a
// file, where os.ReadFile fails with something other than "not a symlink";
// and all three h.Write error-propagation branches, unreachable through the
// real sha256.Writer every other caller uses.
func TestFingerprintFile(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*testing.T){"fingerprint file cases": runFingerprintFileCases}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test(t)
		})
	}
}

func runFingerprintFileCases(t *testing.T) {
	t.Parallel()

	realFile := filepath.Join(t.TempDir(), "real.txt")
	if err := os.WriteFile(realFile, []byte("hello, fingerprint"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", realFile, err)
	}

	t.Run("error/no-error paths", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		tests := map[string]struct {
			base, path string
			wantErrStr string
		}{
			"mismatched relative base falls back to path, not an error": {
				base: "not/a/real/base", path: realFile, wantErrStr: "",
			},
			"nonexistent path is a real Lstat error": {
				base: dir, path: filepath.Join(dir, "does-not-exist"), wantErrStr: "mutate: reading",
			},
			"a directory path is a real ReadFile error": {
				base: dir, path: dir, wantErrStr: "mutate: reading",
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				err := fingerprintFile(io.Discard, tt.base, tt.path)

				if tt.wantErrStr == "" {
					if err != nil {
						t.Errorf("fingerprintFile() error = %v, want nil", err)
					}

					return
				}

				if err == nil || !strings.Contains(err.Error(), tt.wantErrStr) {
					t.Errorf("fingerprintFile() error = %v, want it to contain %q", err, tt.wantErrStr)
				}
			})
		}
	})

	t.Run("a symlink's target text stands in for content, matching copyTree's own placement", func(t *testing.T) {
		t.Parallel()

		// fingerprintFile hashes both the entry's base-relative name and its
		// content, so the symlink and the plain file below are placed in
		// separate subdirs under the same entry name ("entry") — matching
		// names is what isolates the comparison to content alone, which is
		// the concrete claim fingerprintFile's doc comment makes: a symlink's
		// target text stands in for content, matching copyTree's own
		// placement.
		dir := t.TempDir()
		linkDir := filepath.Join(dir, "as-symlink")
		fileDir := filepath.Join(dir, "as-plain-file")

		if err := os.Mkdir(linkDir, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}

		if err := os.Mkdir(fileDir, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}

		target := filepath.Join(dir, "target-does-not-need-to-exist")
		link := filepath.Join(linkDir, "entry")

		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}

		hLink := sha256.New()
		if err := fingerprintFile(hLink, linkDir, link); err != nil {
			t.Fatalf("fingerprintFile(symlink) error = %v", err)
		}

		asFile := filepath.Join(fileDir, "entry")
		if err := os.WriteFile(asFile, []byte(target), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		hFile := sha256.New()
		if err := fingerprintFile(hFile, fileDir, asFile); err != nil {
			t.Fatalf("fingerprintFile(plain file) error = %v", err)
		}

		if got, want := hLink.Sum(nil), hFile.Sum(nil); !bytes.Equal(got, want) {
			t.Errorf("fingerprintFile(symlink) = %x, want %x (same as plain file with identical name/content)", got, want)
		}
	})

	t.Run("h.Write error propagation", func(t *testing.T) {
		t.Parallel()

		tests := map[string]struct{ failAfter int }{
			"fails on the relative-name write":        {failAfter: 0},
			"fails on the delimiter write after name": {failAfter: 1},
			"fails on the content write":              {failAfter: 2},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				w := &cacheFailAfterNWriter{n: tt.failAfter}

				err := fingerprintFile(w, filepath.Dir(realFile), realFile)
				if err == nil || !strings.Contains(err.Error(), "injected write error") {
					t.Errorf("fingerprintFile() error = %v, want the injected write error", err)
				}
			})
		}
	})
}

// TestLoadCacheIndexErrors covers loadCacheIndex's two filesystem error
// paths TestLoadCacheIndex's happy/torn-write cases don't reach: a read
// failure that is not "file does not exist" (a directory given as the cache
// path), and a torn-write recovery whose corrective os.Truncate itself fails
// (a read-only cache file).
func TestLoadCacheIndexErrors(t *testing.T) {
	t.Parallel()

	t.Run("a directory as the cache path is a real, non-NotExist read error", func(t *testing.T) {
		t.Parallel()

		if _, err := loadCacheIndex(t.TempDir()); err == nil {
			t.Error("loadCacheIndex() error = nil for a directory path, want a read error")
		}
	})

	t.Run("a read-only file blocks the torn-write truncate recovery", func(t *testing.T) {
		t.Parallel()

		cacheSkipIfPrivileged(t)

		path := filepath.Join(t.TempDir(), "mutate-cache.jsonl")

		valid, err := json.Marshal(cacheRecord{Key: cacheKey{MutantID: "a"}, Status: Killed})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}

		content := append([]byte(nil), valid...)
		content = append(content, '\n')
		content = append(content, []byte(`{"Key":{"MutantID":"torn"},"Stat`)...)

		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}

		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatalf("Chmod(%s) error = %v", path, err)
		}

		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		if _, err := loadCacheIndex(path); err == nil {
			t.Error("loadCacheIndex() error = nil for a read-only file with a torn trailing write, want a truncate error")
		}
	})
}

// TestClosurePathsErrors covers closurePaths' branches TestCacheFingerprint's
// narrow-scope cases don't reach: a go.mod/go.sum stat failure that is not
// "file does not exist" (the containing directory is unreadable), a dirs
// entry that cannot be listed at all, and a subdirectory inside a dirs entry
// being skipped rather than included.
func TestClosurePathsErrors(t *testing.T) {
	t.Parallel()

	t.Run("a blocked module directory is a real, non-NotExist stat error", func(t *testing.T) {
		t.Parallel()

		cacheSkipIfPrivileged(t)

		parent := t.TempDir()
		blocked := filepath.Join(parent, "blocked")

		if err := os.Mkdir(blocked, 0o750); err != nil {
			t.Fatalf("Mkdir(%s) error = %v", blocked, err)
		}

		//nolint:gosec // restoring the directory's own prior (0750) permission bits so t.TempDir()'s cleanup can traverse and remove it; gosec's G302 threshold is written for regular files, not directories, which need their execute bit back to be removable at all
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

		if err := os.Chmod(blocked, 0o000); err != nil {
			t.Fatalf("Chmod(%s) error = %v", blocked, err)
		}

		if _, err := closurePaths(blocked, nil); err == nil {
			t.Error("closurePaths() error = nil for an unreadable module directory, want a stat error")
		}
	})

	t.Run("a dirs entry that cannot be read is a real error", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		if _, err := closurePaths(root, map[string]bool{filepath.Join(root, "does-not-exist"): true}); err == nil {
			t.Error("closurePaths() error = nil for an unreadable dirs entry, want a read error")
		}
	})

	t.Run("a subdirectory inside a dirs entry is skipped, its sibling file is not", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		writeFiles(t, map[string]string{
			filepath.Join(root, "a", "a.go"):           "package a\n",
			filepath.Join(root, "a", "nested", "b.go"): "package nested\n",
		})

		paths, err := closurePaths(root, map[string]bool{filepath.Join(root, "a"): true})
		if err != nil {
			t.Fatalf("closurePaths() error = %v", err)
		}

		for _, p := range paths {
			if filepath.Base(filepath.Dir(p)) == "nested" {
				t.Errorf("closurePaths() included a file inside a nested subdirectory: %s", p)
			}
		}

		var sawAGo bool

		for _, p := range paths {
			if p == filepath.Join(root, "a", "a.go") {
				sawAGo = true
			}
		}

		if !sawAGo {
			t.Errorf("closurePaths() = %v, want it to include %s", paths, filepath.Join(root, "a", "a.go"))
		}
	})
}

// TestNewCacheStoreOpenError covers newCacheStore's os.OpenFile error path: a
// cache path whose parent directory does not exist cannot be created by
// O_CREATE alone.
func TestNewCacheStoreOpenError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist", "mutate-cache.jsonl")

	if _, err := newCacheStore(path); err == nil {
		t.Error("newCacheStore() error = nil for a missing parent directory, want an open error")
	}
}

// TestCacheStoreConsumeErrors covers consume's two failure branches
// TestLoadCacheIndex's happy-path cacheStore usage never reaches: a write
// failure (an already-closed file) that both sets werr and, for every record
// after the first, takes the "already failed, drop it" branch; and a write
// that succeeds but whose final close-time fsync fails (a pipe, which
// supports Write but not fsync).
func TestCacheStoreConsumeErrors(t *testing.T) {
	t.Parallel()

	t.Run("a write failure sets werr, and a second record hits the already-failed branch", func(t *testing.T) {
		t.Parallel()

		f, err := os.CreateTemp(t.TempDir(), "closed-*.jsonl")
		if err != nil {
			t.Fatalf("CreateTemp() error = %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		s := &cacheStore{records: make(chan cacheRecord), done: make(chan struct{})}
		go s.consume(f)

		s.record(cacheRecord{Key: cacheKey{MutantID: "a"}})
		s.record(cacheRecord{Key: cacheKey{MutantID: "b"}})

		if err := s.close(); err == nil {
			t.Error("cacheStore.close() error = nil after writing to an already-closed file, want a write error")
		}
	})

	t.Run("a write that succeeds but cannot fsync (a pipe) surfaces a sync error", func(t *testing.T) {
		t.Parallel()

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("Pipe() error = %v", err)
		}

		t.Cleanup(func() { _ = r.Close() })

		s := &cacheStore{records: make(chan cacheRecord), done: make(chan struct{})}
		go s.consume(w)

		s.record(cacheRecord{Key: cacheKey{MutantID: "a"}, Status: Killed})

		if err := s.close(); err == nil {
			t.Error("cacheStore.close() error = nil for a pipe (fsync unsupported), want a sync error")
		}
	})
}
