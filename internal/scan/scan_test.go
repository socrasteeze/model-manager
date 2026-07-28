package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// writeModel writes a minimally well-formed safetensors file.
func writeModel(t *testing.T, dir, name string, tensors []byte) string {
	t.Helper()
	header := `{"w":{"dtype":"F16","shape":[1],"data_offsets":[0,2]}}`
	var b bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(header)))
	b.Write(n[:])
	b.WriteString(header)
	b.Write(tensors)

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runScan(t *testing.T, s *store.Store, opts Options) *Result {
	t.Helper()
	res, err := Run(context.Background(), s, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func countRow(t *testing.T, s *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func TestScanRecordsModelFiles(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "a.safetensors", []byte{1, 2, 3})
	writeModel(t, dir, "sub/b.safetensors", []byte{4, 5, 6})
	writeModel(t, dir, "c.gguf", []byte{7})

	// Files that are not models must not enter the index at all.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 2})

	if got := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); got != 3 {
		t.Fatalf("recorded %d paths, want 3 (non-model files must be ignored)", got)
	}
	if res.Roots[0].Counters.FilesHashed != 3 {
		t.Fatalf("hashed %d files, want 3", res.Roots[0].Counters.FilesHashed)
	}
	if res.Roots[0].Status != store.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Roots[0].Status)
	}

	// The .gguf here is not valid GGUF -- it has safetensors framing. It must
	// still be hashed and recorded; only its weights hash is unavailable.
	var format string
	var weights any
	if err := s.DB().QueryRow(
		`SELECT f.format, f.weights_sha256 FROM model_file f
           JOIN model_file_path p USING (sha256) WHERE p.path LIKE '%c.gguf'`,
	).Scan(&format, &weights); err != nil {
		t.Fatalf("query gguf row: %v", err)
	}
	if format != "gguf" {
		t.Fatalf("format = %q, want gguf", format)
	}
	if weights != nil {
		t.Fatal("a file that failed GGUF framing must not get a weights hash")
	}
}

// The verification contract from spec §17: a second run should be near-instant
// via the cache and produce an identical distinct-hash count.
func TestRescanIsAllCacheHits(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeModel(t, dir, n+".safetensors", []byte(n))
	}
	s := newStore(t)

	first := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if first.Roots[0].Counters.FilesHashed != 3 {
		t.Fatalf("first run hashed %d, want 3", first.Roots[0].Counters.FilesHashed)
	}

	before := countRow(t, s, `SELECT COUNT(DISTINCT sha256) FROM model_file`)

	second := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if second.Roots[0].Counters.FilesHashed != 0 {
		t.Fatalf("rescan hashed %d files, want 0 -- the cache did not hit",
			second.Roots[0].Counters.FilesHashed)
	}
	if second.Roots[0].Counters.FilesCached != 3 {
		t.Fatalf("rescan cached %d files, want 3", second.Roots[0].Counters.FilesCached)
	}

	after := countRow(t, s, `SELECT COUNT(DISTINCT sha256) FROM model_file`)
	if before != after {
		t.Fatalf("distinct hash count changed across runs: %d -> %d", before, after)
	}
}

// The premise of the whole design: paths churn. A rename within a filesystem
// preserves the inode, so it must cost nothing -- no re-read -- and the metadata
// must follow the content to its new path.
func TestRenameWithinFilesystemCostsNothing(t *testing.T) {
	dir := t.TempDir()
	old := writeModel(t, dir, "old-name.safetensors", []byte{9, 9, 9})
	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	var sha string
	if err := s.DB().QueryRow(`SELECT sha256 FROM model_file_path WHERE path = ?`, old).Scan(&sha); err != nil {
		t.Fatalf("query original: %v", err)
	}

	renamed := filepath.Join(dir, "new-name.safetensors")
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}

	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if res.Roots[0].Counters.FilesHashed != 0 {
		t.Fatalf("rename forced %d re-hash(es); the inode-keyed cache should have hit",
			res.Roots[0].Counters.FilesHashed)
	}

	var newSha string
	if err := s.DB().QueryRow(`SELECT sha256 FROM model_file_path WHERE path = ?`, renamed).Scan(&newSha); err != nil {
		t.Fatalf("renamed path not recorded: %v", err)
	}
	if newSha != sha {
		t.Fatalf("metadata did not follow the rename: %s != %s", newSha, sha)
	}

	// The old path is gone from disk but must survive as a not-present row rather
	// than being deleted, so the index can still answer what it once knew.
	var present int
	if err := s.DB().QueryRow(`SELECT present FROM model_file_path WHERE path = ?`, old).Scan(&present); err != nil {
		t.Fatalf("old path row was deleted rather than marked absent: %v", err)
	}
	if present != 0 {
		t.Fatal("old path is still marked present after the file moved away")
	}
}

func TestDeletedFileIsMarkedAbsentNotDeleted(t *testing.T) {
	dir := t.TempDir()
	keep := writeModel(t, dir, "keep.safetensors", []byte{1})
	gone := writeModel(t, dir, "gone.safetensors", []byte{2})
	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if res.Roots[0].SweptAbsent != 1 {
		t.Fatalf("swept %d paths, want 1", res.Roots[0].SweptAbsent)
	}

	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); n != 2 {
		t.Fatalf("%d path rows, want 2 -- absent paths must be kept", n)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path WHERE present = 1`); n != 1 {
		t.Fatalf("%d present paths, want 1", n)
	}
	if n := countRow(t, s, `SELECT present FROM model_file_path WHERE path = ?`, keep); n != 1 {
		t.Fatal("the surviving file was marked absent")
	}
}

// Duplication is the number Phase 0 exists to produce: identical content in two
// places is one record with two paths, not two records.
func TestDuplicateContentIsOneRecordTwoPaths(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "one/dup.safetensors", []byte{42, 42})
	writeModel(t, dir, "two/dup-copy.safetensors", []byte{42, 42})

	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file`); n != 1 {
		t.Fatalf("%d model_file rows for identical content, want 1", n)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); n != 2 {
		t.Fatalf("%d path rows, want 2", n)
	}
}

// A hardlink is a second name for one inode. The cache must recognize the second
// name without a second read, and record it as another path on the same content.
func TestHardlinkIsCachedNotRehashed(t *testing.T) {
	dir := t.TempDir()
	orig := writeModel(t, dir, "orig.safetensors", []byte{5, 5, 5})
	link := filepath.Join(dir, "link.safetensors")
	if err := os.Link(orig, link); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}

	s := newStore(t)
	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	if res.Roots[0].Counters.FilesHashed != 1 {
		t.Fatalf("hashed %d files, want 1 -- the hardlink should have hit the cache",
			res.Roots[0].Counters.FilesHashed)
	}
	if res.Roots[0].Counters.FilesCached != 1 {
		t.Fatalf("cached %d files, want 1", res.Roots[0].Counters.FilesCached)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); n != 2 {
		t.Fatalf("%d path rows, want 2", n)
	}
}

// Symlinks are skipped. Following them would count a generated view (spec §9) as
// a second copy of a model already indexed.
func TestSymlinksAreNotFollowed(t *testing.T) {
	dir := t.TempDir()
	orig := writeModel(t, dir, "real/m.safetensors", []byte{3})
	if err := os.MkdirAll(filepath.Join(dir, "view"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(orig, filepath.Join(dir, "view", "m.safetensors")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); n != 1 {
		t.Fatalf("%d path rows, want 1 -- the symlinked view must not be counted", n)
	}
}

// The probe fast path trades a full read for a sampled one, and must mark what
// it binds as provisional so a later pass confirms it by full hash.
func TestProbeFallbackBindsProvisionally(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "a.safetensors", bytes.Repeat([]byte{8}, 4096))
	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	// A copy has identical content under a new inode: the cache misses, the probe
	// matches.
	src, err := os.ReadFile(filepath.Join(dir, "a.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy.safetensors")
	if err := os.WriteFile(copyPath, src, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1, UseProbe: true})
	if res.Roots[0].Counters.FilesProbed != 1 {
		t.Fatalf("probed %d files, want 1", res.Roots[0].Counters.FilesProbed)
	}
	if res.Roots[0].Counters.FilesHashed != 0 {
		t.Fatalf("hashed %d files, want 0 -- the probe should have avoided the read",
			res.Roots[0].Counters.FilesHashed)
	}

	var provisional int
	if err := s.DB().QueryRow(
		`SELECT provisional FROM model_file_path WHERE path = ?`, copyPath).Scan(&provisional); err != nil {
		t.Fatal(err)
	}
	if provisional != 1 {
		t.Fatal("a probe-bound path was recorded as confirmed; a sampled match is not identity")
	}
}

// With the probe off -- the default for a first pass -- the same copy is fully
// hashed and bound for real.
func TestProbeOffMeansFullHash(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "a.safetensors", bytes.Repeat([]byte{8}, 4096))
	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	src, _ := os.ReadFile(filepath.Join(dir, "a.safetensors"))
	if err := os.WriteFile(filepath.Join(dir, "copy.safetensors"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if res.Roots[0].Counters.FilesHashed != 1 {
		t.Fatalf("hashed %d files, want 1", res.Roots[0].Counters.FilesHashed)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path WHERE provisional = 1`); n != 0 {
		t.Fatalf("%d provisional rows with the probe disabled, want 0", n)
	}
}

// An interrupted scan observed only part of the tree. Sweeping on that evidence
// would mark most of the library missing.
func TestCancelledScanDoesNotSweep(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeModel(t, dir, n+".safetensors", []byte(n))
	}
	s := newStore(t)
	runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Run(ctx, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cancelled {
		t.Fatal("Result.Cancelled = false for a cancelled context")
	}
	if res.Roots[0].Status != store.StatusInterrupted {
		t.Fatalf("status = %q, want interrupted", res.Roots[0].Status)
	}
	if res.Roots[0].SweptAbsent != 0 {
		t.Fatalf("an interrupted scan swept %d paths; it must sweep none",
			res.Roots[0].SweptAbsent)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path WHERE present = 1`); n != 3 {
		t.Fatalf("%d present paths after an interrupted scan, want 3", n)
	}
}

func TestNestedRootsAreRejected(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)

	if _, err := Run(context.Background(), s, Options{Roots: []string{dir, inner}}); err == nil {
		t.Fatal("nested roots were accepted; the per-root sweep would be ambiguous")
	}
}

func TestSiblingRootsWithSharedPrefixAreAllowed(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "models")
	b := filepath.Join(base, "models2")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeModel(t, a, "x.safetensors", []byte{1})
	writeModel(t, b, "y.safetensors", []byte{2})

	s := newStore(t)
	res := runScan(t, s, Options{Roots: []string{a, b}, WorkersPerDevice: 1})
	if len(res.Roots) != 2 {
		t.Fatalf("scanned %d roots, want 2 -- /models2 is not inside /models", len(res.Roots))
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM model_file_path`); n != 2 {
		t.Fatalf("%d path rows, want 2", n)
	}
}

func TestUnreadableFileIsRecordedAndScanContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not restrict reads")
	}
	dir := t.TempDir()
	writeModel(t, dir, "good.safetensors", []byte{1})
	bad := writeModel(t, dir, "bad.safetensors", []byte{2})
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o644) })

	s := newStore(t)
	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})

	if res.Roots[0].Counters.Errors == 0 {
		t.Fatal("an unreadable file produced no recorded error")
	}
	if res.Roots[0].Counters.FilesHashed != 1 {
		t.Fatalf("hashed %d files, want 1 -- the scan must continue past a bad file",
			res.Roots[0].Counters.FilesHashed)
	}
	if n := countRow(t, s, `SELECT COUNT(*) FROM scan_error`); n == 0 {
		t.Fatal("no scan_error row was written")
	}
}

func TestEmptyRootCompletesCleanly(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	res := runScan(t, s, Options{Roots: []string{dir}, WorkersPerDevice: 1})
	if res.Roots[0].Status != store.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Roots[0].Status)
	}
	if res.Roots[0].Counters.FilesSeen != 0 {
		t.Fatalf("saw %d files in an empty root", res.Roots[0].Counters.FilesSeen)
	}
}

func TestMissingRootIsAnError(t *testing.T) {
	s := newStore(t)
	_, err := Run(context.Background(), s, Options{
		Roots: []string{filepath.Join(t.TempDir(), "does-not-exist")},
	})
	if err == nil {
		t.Fatal("a missing root was accepted")
	}
}

func TestIsUnder(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a" + sep + "b", "/a", true},
		{"/a", "/a", true},
		{"/ab", "/a", false},
		{"/a", "/a" + sep + "b", false},
	}
	for _, c := range cases {
		child := filepath.FromSlash(c.child)
		parent := filepath.FromSlash(c.parent)
		if got := isUnder(child, parent); got != c.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", child, parent, got, c.want)
		}
	}
}
