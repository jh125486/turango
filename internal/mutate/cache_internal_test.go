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
	"encoding/json"
	"os"
	"path/filepath"
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
