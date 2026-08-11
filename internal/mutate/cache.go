// Persistent mutant verdict cache — ROADMAP.md gap 12.
//
// turango has no resume capability without this: a hard kill (SIGKILL, an
// OOM reaper, a killed background job) or even a graceful Ctrl+C throws away
// every verdict already computed, and re-running the same sweep starts over
// from mutant zero. cache.go is the on-disk half of the fix — a JSON-Lines
// file of (key, verdict) records, appended to as soon as each verdict is
// produced and loaded once, read-only, before a run's workers start.
//
// The central risk this file exists to close, not just to speed things up,
// is a *wrong* cache hit: serving a stale verdict for a mutation that looks
// the same by mutantID but is not, in fact, the same code. See
// [cacheFingerprint]'s doc comment and ROADMAP.md gap 12a for the concrete
// example (a same-width literal edit at the same file/line/column) that
// rules out mutantID alone as a safe key.
package mutate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// cacheFile is the fixed name of the persistent verdict cache written
// inside [Options.CacheDir], mirroring main.go's existing reportFile
// constant-naming convention — there is no meaningful "cache enabled with a
// custom filename" state to support, so the name itself is not
// configurable.
const cacheFile = "mutate-cache.jsonl"

// cachePath joins dir and [cacheFile].
func cachePath(dir string) string {
	return filepath.Join(dir, cacheFile)
}

// cacheKey identifies exactly which prior run a cache record answers for.
// Every field must match the current run's own value for a lookup to be
// trusted — see ROADMAP.md gap 12a for why each one is here, not just
// mutantID alone:
//
//   - MutantID hashes a *position* in a specific AST (file, line, column,
//     operator, mutation index), not the mutated node's own bytes. Two
//     genuinely different mutations at that exact same position — a
//     same-width literal or identifier edit is the ordinary way this
//     happens, not a contrived corner case — produce an identical
//     MutantID. Fingerprint is what tells them apart.
//   - Fingerprint is a content hash of exactly the file set this mutant's
//     `go test` run would actually depend on (see [cacheFingerprint]),
//     computed at plan time and threaded onto every mutant of the package
//     it was computed for.
//   - Scope matters because the identical mutant can classify differently
//     under [ScopeFull] versus [ScopePackage]/[ScopeImpact] — a
//     neighbouring package's test can kill it under the former and never
//     even run under the latter.
//   - TCE matters because a mutant recorded [cacheRecord.Equivalent] under
//     TCE=true was never run against the suite at all; replaying that as a
//     verdict under TCE=false, which needs a real classification, would be
//     exactly the kind of silent wrong answer this whole design exists to
//     rule out.
//   - Toolchain matters because a compiled/tested verdict can change across
//     a Go version or cross-compilation target even when every source byte
//     is identical.
//
// Options.Workspace, Options.Parallel and Options.TestTimeout are
// deliberately absent — see ROADMAP.md gap 12a for why each is safe to
// leave out (a construction-strategy choice, a scheduling-only knob, and an
// explicit, honestly-stated risk that only ever biases toward Killed,
// respectively).
type cacheKey struct {
	Toolchain   string // resolveToolchain: go version + GOOS/GOARCH
	Scope       string // Scope.String()
	TCE         bool
	Fingerprint string // cacheFingerprint's full hex SHA-256
	MutantID    string // mutantID, unmodified
}

// cacheRecord is one line of the on-disk cache: a key plus the verdict it
// answers for.
//
// Equivalent mirrors [EquivalentResult]'s own shape: when true, Status and
// Output are meaningless zero values — the mutant this key names was never
// run against the suite at all (Trivial Compiler Equivalence filtered it
// before r.goTest), so there is no Status/Output to have cached. A reader
// must check Equivalent first, exactly the way [runner.run]'s own ok/
// equivalent return pair already requires callers to.
type cacheRecord struct {
	Key        cacheKey
	Equivalent bool
	Status     Status
	Output     string
}

// cacheFingerprint hashes exactly the file set a mutant in this scope would
// actually be tested against — i.e. the same file set [runner.workspaceFor]
// would copy to execute it — which is the only file set a cache key can
// safely be built from. Two narrower alternatives were considered and
// rejected (ROADMAP.md gap 12a): fingerprinting only the mutated file is
// unsafe (a sibling package's test file can flip a verdict without the
// mutated file changing at all), and always fingerprinting the whole module
// is safe but needlessly coarse under a scope narrower than [ScopeFull].
//
// dirs == nil means "the whole module" — every file [copyModule] itself
// would copy, walked from dir (the module root), skipping only a top-level
// .git directory exactly as [copyTree] does. This is the correct fingerprint
// whenever gap 5's dependency-closure resolution declined for this package
// (a vendor/ directory, a //go:embed directive, an unsafe replace target)
// or scope is [ScopeFull] itself, where a forward closure cannot support
// ScopeFull's cross-package kill detection (see [resolveClosure]'s own doc
// comment) — under ScopeFull, any file in the module could contain a test
// that starts or stops covering the mutated line, so "every file that could
// affect the verdict" genuinely is "every file in the module."
//
// dirs != nil means exactly go.mod, go.sum, and every file directly inside
// each directory in dirs, rooted at dir (the module root) — the identical
// file set [copyClosure] copies. An edit to a file outside that set cannot
// affect this mutant's `go test` outcome under a scope this narrow, so it
// correctly leaves the fingerprint — and therefore the cache entry —
// unchanged.
//
// The digest is the full, untruncated 64-character hex SHA-256, unlike
// [mutantID]'s user-facing 12-character truncation: mutantID's truncation
// is fine because a collision there is merely a (vanishingly unlikely, and
// harmless) ambiguity in a report or a -mutatemutant= replay; a collision
// here would be a silent false cache hit gating every lookup — a much worse
// class of bug — so nothing is thrown away.
func cacheFingerprint(dir string, dirs map[string]bool) (string, error) {
	var (
		paths []string
		err   error
	)

	if dirs == nil {
		paths, err = wholeModulePaths(dir)
	} else {
		paths, err = closurePaths(dir, dirs)
	}

	if err != nil {
		return "", err
	}

	// Sorted so the digest is a pure function of file set + content,
	// independent of filepath.WalkDir's or os.ReadDir's own (already
	// deterministic, but not the point here — this is about not depending
	// on that) iteration order.
	sort.Strings(paths)

	h := sha256.New()

	for _, p := range paths {
		if err := fingerprintFile(h, dir, p); err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// wholeModulePaths lists every file [copyModule] would copy from dir,
// skipping only a top-level .git directory exactly as [copyTree] does —
// the dirs == nil half of [cacheFingerprint]'s file-set selection.
func wholeModulePaths(dir string) ([]string, error) {
	var paths []string

	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if entry.Name() == ".git" && path != dir {
				return fs.SkipDir
			}

			return nil
		}

		paths = append(paths, path)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("mutate: walking %s for cache fingerprint: %w", dir, err)
	}

	return paths, nil
}

// closurePaths lists exactly go.mod, go.sum, and every file directly
// inside each directory in dirs — the identical file set [copyClosure]
// copies, and the dirs != nil half of [cacheFingerprint]'s file-set
// selection.
func closurePaths(dir string, dirs map[string]bool) ([]string, error) {
	var paths []string

	for _, name := range []string{goModFile, "go.sum"} {
		p := filepath.Join(dir, name)

		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("mutate: statting %s for cache fingerprint: %w", p, err)
		}
	}

	for d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("mutate: reading %s for cache fingerprint: %w", d, err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			paths = append(paths, filepath.Join(d, entry.Name()))
		}
	}

	return paths, nil
}

// fingerprintFile feeds one file's path (relative to base) and content into
// h, each terminated by a NUL byte — the same delimiter idiom [mutantID]
// already uses to keep concatenated fields from colliding across a
// boundary. A symlink's target string stands in for "content": that is
// what [copyTree]/[copyDirFiles] actually place in a workspace copy, not
// the bytes at the far end of the link.
func fingerprintFile(h io.Writer, base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		rel = path
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("mutate: reading %s for cache fingerprint: %w", path, err)
	}

	var content []byte

	if info.Mode()&fs.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("mutate: reading %s for cache fingerprint: %w", path, err)
		}

		content = []byte(link)
	} else {
		content, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("mutate: reading %s for cache fingerprint: %w", path, err)
		}
	}

	if _, err := h.Write([]byte(rel)); err != nil {
		return err
	}

	if _, err := h.Write([]byte{0}); err != nil {
		return err
	}

	if _, err := h.Write(content); err != nil {
		return err
	}

	_, err = h.Write([]byte{0})

	return err
}

// resolveToolchain identifies the exact toolchain and target platform a
// cached verdict was produced under: `go version`'s own output, plus
// GOOS/GOARCH (an explicit environment override honoured first, falling
// back to runtime.GOOS/GOARCH when unset — the same precedence `go build`
// itself gives an explicit GOOS/GOARCH override). A compiled/tested verdict
// can change across a Go version or a cross-compilation target even when
// every source byte is identical, so this is a required field of
// [cacheKey], not an afterthought.
//
// Not exhaustive, stated honestly rather than solved: build tags,
// CGO_ENABLED and other `go env` values are not folded in for v1 — the same
// spirit as TCE's own reproducibility spike flagging what it did and did
// not verify. A cache directory shared across differently-configured
// machines is not proven safe by this design and should be treated the way
// $GOCACHE already implicitly is — machine/environment-scoped, not
// portable.
func resolveToolchain(ctx context.Context, goBin string) (string, error) {
	//nolint:gosec // goBin is the resolved real go toolchain binary (see goproxy.Resolve), not attacker-controlled input
	out, err := exec.CommandContext(ctx, goBin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("mutate: resolving toolchain version: %w", err)
	}

	goos := os.Getenv("GOOS")
	if goos == "" {
		goos = runtime.GOOS
	}

	goarch := os.Getenv("GOARCH")
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	return strings.TrimSpace(string(out)) + "\x00" + goos + "\x00" + goarch, nil
}

// cacheIndex is a run's read-only, in-memory view of a prior run's cache
// file.
//
// It is built once, sequentially, by [loadCacheIndex], entirely before
// [execute] starts spawning concurrent file workers — the identical
// "read-only, safe to share across Options.Parallel workers with zero
// synchronization" shape [fileJob.mutators]'s run-wide shared slice already
// relies on. Nothing mutates a *cacheIndex after loadCacheIndex returns it.
type cacheIndex struct {
	records map[cacheKey]cacheRecord
}

// get looks up key. A nil index answers every lookup with ok == false,
// which is what lets [runner.run] hold a *cacheIndex unconditionally and
// only actually consult it when caching was requested at all (mirroring
// [impactMap.covering]'s identical nil-is-"nothing to report" convention).
func (idx *cacheIndex) get(key cacheKey) (cacheRecord, bool) {
	if idx == nil {
		return cacheRecord{}, false
	}

	rec, ok := idx.records[key]

	return rec, ok
}

// loadCacheIndex reads path's JSON-Lines cache file into an immutable
// index, recovering from a torn write left behind by a killed process.
//
// A missing file is not an error — it means "first run, nothing to reuse
// yet," matching every other "nothing here yet" fail-soft case elsewhere in
// this project (e.g. TCE's per-package baseline compile failing soft to
// "run without TCE").
//
// Recovery is a load-time truncate, not a lazy skip: per [cacheStore]'s own
// write guarantee (each record is written with exactly one os.File.Write
// call to a file opened O_APPEND, so a killed process can only ever leave
// the *last* line in the file torn — never interleave two records'
// bytes) — the first line that fails to unmarshal is, by that guarantee,
// always the last line present, never a line with valid records after it.
// The file is physically truncated to end exactly after the last
// successfully-parsed record, so every future load and append sees the
// same simple invariant: the file is always either empty or a sequence of
// complete JSON lines, never "complete lines + garbage + more complete
// lines."
//
// Multiple records for the same key are expected, not an error (ROADMAP.md
// gap 12g: nothing actively removes a now-stale record when its
// fingerprint changes) — later records in file order simply overwrite
// earlier ones for the same key, which appending in write order already
// guarantees is "most recent wins."
func loadCacheIndex(path string) (*cacheIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cacheIndex{records: map[cacheKey]cacheRecord{}}, nil
		}

		return nil, fmt.Errorf("mutate: reading cache %s: %w", path, err)
	}

	idx := &cacheIndex{records: map[cacheKey]cacheRecord{}}

	rest := data
	validThrough := 0

	for len(rest) > 0 {
		lineLen := len(rest)
		line := rest

		if nl := indexByte(rest, '\n'); nl >= 0 {
			line = rest[:nl]
			lineLen = nl + 1
		}

		var rec cacheRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// A torn write: per the guarantee above, this can only ever be
			// the last line present. Stop here; this line and everything
			// after it (there is nothing after it, by that same guarantee)
			// is dropped.
			break
		}

		idx.records[rec.Key] = rec
		validThrough += lineLen
		rest = rest[lineLen:]
	}

	if validThrough < len(data) {
		if err := os.Truncate(path, int64(validThrough)); err != nil {
			return nil, fmt.Errorf("mutate: truncating torn write in cache %s: %w", path, err)
		}
	}

	return idx, nil
}

// indexByte is a tiny local wrapper so loadCacheIndex reads as plainly as
// possible; strings.IndexByte/bytes.IndexByte are equivalent, this just
// avoids importing "bytes" into this file for a single call.
func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}

	return -1
}

// cacheStore is the cache's write half: a single-consumer goroutine, fed by
// one channel, that appends every completed verdict to disk as soon as it
// is produced.
//
// Structurally identical to [collector]/[estimateTally]: a record method
// that blocking-sends on a channel (the same effective backpressure
// [collector.mutant]'s doc comment already describes), one goroutine
// draining it and doing the actual write, and a close that closes the
// channel and blocks for the consumer to finish flushing.
//
// Deliberately a separate consumer from collector's own three channels, not
// folded into it: collector's job is building the in-memory *Result, a
// different failure mode from cache durability. A disk-full or permission
// error writing the cache must never lose or corrupt the run's real,
// in-memory Result, so keeping them as two independently-failing
// components means a cacheStore write error can be dropped without
// collector ever knowing anything went wrong — mirroring how every other
// cache failure in this file is fail-soft (see [loadCacheIndex]'s sibling
// treatment and [Run]'s wiring).
type cacheStore struct {
	records chan cacheRecord
	done    chan struct{}

	// werr is set by the consumer goroutine if a write ever fails. It is
	// only ever read after done is closed, so the channel close/receive
	// pair is what makes reading it from close safe without its own lock.
	werr error
}

// newCacheStore opens path for appending (creating it if necessary) and
// starts the consumer goroutine.
func newCacheStore(path string) (*cacheStore, error) {
	//nolint:gosec // path is derived from -mutatecache, a directory the invoking user named on their own command line, not external input
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mutate: opening cache %s: %w", path, err)
	}

	s := &cacheStore{
		records: make(chan cacheRecord),
		done:    make(chan struct{}),
	}

	go s.consume(f)

	return s, nil
}

// record appends one verdict. Blocks until the consumer goroutine receives
// it — the same effective backpressure [collector.mutant] provides.
func (s *cacheStore) record(rec cacheRecord) { s.records <- rec }

// consume drains records until the channel is closed, encoding each one as
// a single JSON Lines record with exactly one underlying os.File.Write call
// per record (json.Encoder marshals to an internal buffer first, then
// writes it in one call — never a bufio.Writer in front of f, which could
// coalesce several records into one write or delay one past when its
// mutant actually finished, defeating the "as soon as it's produced, not
// batched" guarantee this whole design promises). A single fsync happens
// once, on close, as a cheap belt-and-suspenders measure — not because the
// threat model here (a killed process, not a power-loss event) actually
// needs it: data handed to write() already survives a killed process via
// the OS page cache.
func (s *cacheStore) consume(f *os.File) {
	defer close(s.done)
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)

	for rec := range s.records {
		if s.werr != nil {
			continue // already failed once; drain and drop the rest rather than pile up more errors
		}

		if err := enc.Encode(rec); err != nil {
			s.werr = fmt.Errorf("mutate: writing cache record: %w", err)
		}
	}

	if s.werr == nil {
		if err := f.Sync(); err != nil {
			s.werr = fmt.Errorf("mutate: syncing cache: %w", err)
		}
	}
}

// close signals that no more records will be sent, and blocks until the
// consumer goroutine has finished flushing. It must be called exactly once,
// after every producer goroutine has already returned — the same contract
// [collector.close] documents for itself. The returned error, if non-nil,
// is a write/sync failure that a caller may log; it is never a reason to
// fail an otherwise-complete run (see [Run]'s wiring).
func (s *cacheStore) close() error {
	close(s.records)
	<-s.done

	return s.werr
}
