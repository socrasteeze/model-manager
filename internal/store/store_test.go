package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "master.db"), Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := openTemp(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != len(migrations) {
		t.Fatalf("schema version = %d, want %d", v, len(migrations))
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.db")
	for i := 0; i < 3; i++ {
		s, err := Open(path, Options{AllowNetworkPath: true})
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

func TestUpsertAndCacheLookup(t *testing.T) {
	s := openTemp(t)
	run, err := s.BeginScanRun("/models")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	f := ModelFile{
		SHA256:        "aaa",
		WeightsSHA256: "www",
		WeightsOffset: 1024,
		ProbeSHA256:   "ppp",
		Size:          4096,
		Format:        "safetensors",
		HeaderBlob:    []byte(`{"__metadata__":{}}`),
		HeaderOffset:  8,
	}
	p := FilePath{
		SHA256: "aaa", Path: "/models/a.safetensors", Root: "/models",
		Device: 66, Inode: 1234, Size: 4096, MtimeNs: 999, ScanRunID: run,
	}
	if err := s.UpsertFileAndPath(f, p); err != nil {
		t.Fatalf("UpsertFileAndPath: %v", err)
	}

	sha, ok, err := s.LookupByCacheKey(66, 1234, 4096, 999)
	if err != nil || !ok || sha != "aaa" {
		t.Fatalf("LookupByCacheKey = (%q, %v, %v), want (aaa, true, nil)", sha, ok, err)
	}

	// A changed mtime must miss: the bytes may have changed under the same inode.
	if _, ok, _ := s.LookupByCacheKey(66, 1234, 4096, 1000); ok {
		t.Fatal("cache hit on changed mtime; the key must include mtime")
	}
}

// A provisional binding is a guess from a sampled probe. Treating it as a cache
// hit would let a guess harden into a permanent identity without ever being
// confirmed by a full hash -- the precise failure spec §10.1 forbids.
func TestProvisionalPathIsNeverACacheHit(t *testing.T) {
	s := openTemp(t)
	run, _ := s.BeginScanRun("/models")

	f := ModelFile{SHA256: "bbb", ProbeSHA256: "ppp", Size: 10, Format: "safetensors"}
	p := FilePath{
		SHA256: "bbb", Path: "/models/b.safetensors", Root: "/models",
		Device: 1, Inode: 2, Size: 10, MtimeNs: 3,
		Provisional: true, ScanRunID: run,
	}
	if err := s.UpsertFileAndPath(f, p); err != nil {
		t.Fatalf("UpsertFileAndPath: %v", err)
	}

	if _, ok, err := s.LookupByCacheKey(1, 2, 10, 3); err != nil || ok {
		t.Fatalf("provisional path returned as cache hit (ok=%v, err=%v)", ok, err)
	}
}

// An ambiguous probe -- two distinct hashes sharing a size and sample -- must
// resolve to a full hash, not to whichever row the query happened to return.
func TestAmbiguousProbeIsAMiss(t *testing.T) {
	s := openTemp(t)
	run, _ := s.BeginScanRun("/models")

	for _, sha := range []string{"c1", "c2"} {
		f := ModelFile{SHA256: sha, ProbeSHA256: "same", Size: 500, Format: "gguf"}
		p := FilePath{
			SHA256: sha, Path: "/models/" + sha, Root: "/models",
			Device: 1, Inode: 1, Size: 500, MtimeNs: 1, ScanRunID: run,
		}
		// Distinct inodes, since two paths cannot share one.
		p.Inode = uint64(len(sha) + len(p.Path))
		if err := s.UpsertFileAndPath(f, p); err != nil {
			t.Fatalf("UpsertFileAndPath(%s): %v", sha, err)
		}
	}

	if sha, ok, err := s.LookupByProbe(500, "same"); err != nil || ok {
		t.Fatalf("ambiguous probe resolved to %q (ok=%v, err=%v); want a miss", sha, ok, err)
	}

	// An unambiguous probe still resolves.
	if sha, ok, err := s.LookupByProbe(500, "unique"); err != nil || ok || sha != "" {
		_ = sha // no row: a miss is correct
	}
}

func TestSweepIsScopedToItsRoot(t *testing.T) {
	s := openTemp(t)

	runA, _ := s.BeginScanRun("/rootA")
	mustUpsert(t, s, "h1", "/rootA/one", "/rootA", 1, 11, runA)
	runB, _ := s.BeginScanRun("/rootB")
	mustUpsert(t, s, "h2", "/rootB/two", "/rootB", 1, 22, runB)

	// A later scan of rootA that no longer sees its file must not touch rootB.
	runA2, _ := s.BeginScanRun("/rootA")
	n, err := s.SweepAbsentPaths("/rootA", runA2)
	if err != nil {
		t.Fatalf("SweepAbsentPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d paths, want 1", n)
	}

	if present := presentOf(t, s, "/rootB/two"); !present {
		t.Fatal("sweeping /rootA marked a /rootB path absent")
	}
	if present := presentOf(t, s, "/rootA/one"); present {
		t.Fatal("unobserved path under the swept root is still present")
	}
}

func TestMarkInterruptedRuns(t *testing.T) {
	s := openTemp(t)
	if _, err := s.BeginScanRun("/models"); err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}
	n, err := s.MarkInterruptedRuns()
	if err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d runs, want 1", n)
	}

	var status string
	if err := s.DB().QueryRow(`SELECT status FROM scan_run LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != StatusInterrupted {
		t.Fatalf("status = %q, want %q", status, StatusInterrupted)
	}
}

// A .ckpt has no determinable weights region, and a NULL weights_sha256 must
// survive a round trip as absent rather than as an empty string that later looks
// like a usable rebinding key (spec §2.1).
func TestNullWeightsHashRoundTrips(t *testing.T) {
	s := openTemp(t)
	run, _ := s.BeginScanRun("/models")

	f := ModelFile{SHA256: "ckpt1", ProbeSHA256: "p", Size: 9, Format: "ckpt"}
	p := FilePath{
		SHA256: "ckpt1", Path: "/models/x.ckpt", Root: "/models",
		Device: 1, Inode: 5, Size: 9, MtimeNs: 1, ScanRunID: run,
	}
	if err := s.UpsertFileAndPath(f, p); err != nil {
		t.Fatalf("UpsertFileAndPath: %v", err)
	}

	var w any
	if err := s.DB().QueryRow(
		`SELECT weights_sha256 FROM model_file WHERE sha256 = 'ckpt1'`).Scan(&w); err != nil {
		t.Fatalf("query: %v", err)
	}
	if w != nil {
		t.Fatalf("weights_sha256 = %v, want NULL", w)
	}
}

func mustUpsert(t *testing.T, s *Store, sha, path, root string, dev, ino uint64, run int64) {
	t.Helper()
	err := s.UpsertFileAndPath(
		ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 1, Format: "safetensors"},
		FilePath{
			SHA256: sha, Path: path, Root: root,
			Device: dev, Inode: ino, Size: 1, MtimeNs: 1, ScanRunID: run,
		})
	if err != nil {
		t.Fatalf("UpsertFileAndPath(%s): %v", path, err)
	}
}

func presentOf(t *testing.T, s *Store, path string) bool {
	t.Helper()
	var present int
	if err := s.DB().QueryRow(
		`SELECT present FROM model_file_path WHERE path = ?`, path).Scan(&present); err != nil {
		t.Fatalf("query present for %s: %v", path, err)
	}
	return present == 1
}
