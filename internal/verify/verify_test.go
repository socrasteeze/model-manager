package verify

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/scan"
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
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func scanDir(t *testing.T, s *store.Store, dir string, useProbe bool) {
	t.Helper()
	if _, err := scan.Run(context.Background(), s, scan.Options{
		Roots: []string{dir}, WorkersPerDevice: 1, UseProbe: useProbe,
	}); err != nil {
		t.Fatalf("scan.Run: %v", err)
	}
}

func TestVerifyMatchesCleanIndex(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "a.safetensors", []byte{1, 2, 3})
	writeModel(t, dir, "b.safetensors", []byte{4, 5, 6})

	s := newStore(t)
	scanDir(t, s, dir, false)

	res, err := Run(context.Background(), s, Options{Sample: 0, Workers: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 2 || res.Matched != 2 {
		t.Fatalf("checked %d matched %d, want 2/2", res.Checked, res.Matched)
	}
	if res.Mismatched != 0 || res.Errors != 0 {
		t.Fatalf("mismatched %d errors %d, want 0/0", res.Mismatched, res.Errors)
	}
}

// A file rewritten behind the index's back is exactly what verification is for.
// The mismatch must be reported *and* the index corrected -- reporting alone
// would leave the database knowingly wrong.
func TestVerifyDetectsAndCorrectsChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeModel(t, dir, "a.safetensors", []byte{1, 2, 3})

	s := newStore(t)
	scanDir(t, s, dir, false)

	var recorded string
	if err := s.DB().QueryRow(`SELECT sha256 FROM model_file_path WHERE path = ?`, path).Scan(&recorded); err != nil {
		t.Fatal(err)
	}

	writeModel(t, dir, "a.safetensors", []byte{9, 9, 9, 9})

	res, err := Run(context.Background(), s, Options{Sample: 0, Workers: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Mismatched != 1 {
		t.Fatalf("mismatched = %d, want 1", res.Mismatched)
	}
	if len(res.Mismatches) != 1 || res.Mismatches[0].RecordedSHA != recorded {
		t.Fatalf("mismatch not reported against the recorded hash: %+v", res.Mismatches)
	}
	if res.Mismatches[0].WasProbe {
		t.Error("a fully-hashed path was reported as probe-bound")
	}

	var now string
	if err := s.DB().QueryRow(`SELECT sha256 FROM model_file_path WHERE path = ?`, path).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now == recorded {
		t.Fatal("the index was left pointing at the old hash after a detected mismatch")
	}
	if now != res.Mismatches[0].ActualSHA {
		t.Fatalf("index holds %s, want the actual hash %s", now, res.Mismatches[0].ActualSHA)
	}
}

// This is the promise that makes the probe fast path safe to offer at all: what
// it guesses gets confirmed by a full read before anything is allowed to rely
// on it (spec §10.1).
func TestVerifyConfirmsProvisionalPaths(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "a.safetensors", bytes.Repeat([]byte{7}, 2048))
	s := newStore(t)
	scanDir(t, s, dir, false)

	src, err := os.ReadFile(filepath.Join(dir, "a.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy.safetensors")
	if err := os.WriteFile(copyPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir(t, s, dir, true)

	var provisional int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM model_file_path WHERE provisional = 1`).Scan(&provisional); err != nil {
		t.Fatal(err)
	}
	if provisional != 1 {
		t.Fatalf("%d provisional paths before verification, want 1", provisional)
	}

	res, err := Run(context.Background(), s, Options{ProvisionalOnly: true, Workers: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 1 {
		t.Fatalf("confirmed %d, want 1", res.Confirmed)
	}
	// The copy really was identical, so the probe was right and nothing was
	// misbound.
	if res.ProbeMisbindings != 0 {
		t.Fatalf("ProbeMisbindings = %d for a correct probe match", res.ProbeMisbindings)
	}

	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM model_file_path WHERE provisional = 1`).Scan(&provisional); err != nil {
		t.Fatal(err)
	}
	if provisional != 0 {
		t.Fatalf("%d provisional paths remain after confirmation", provisional)
	}
}

// A probe that guessed wrong is the failure the provisional flag exists to
// contain. Verification has to catch it and correct the binding.
func TestVerifyCatchesAWrongProbeBinding(t *testing.T) {
	dir := t.TempDir()
	real := writeModel(t, dir, "real.safetensors", []byte{1, 1, 1})
	impostor := writeModel(t, dir, "impostor.safetensors", []byte{2, 2, 2})

	s := newStore(t)
	scanDir(t, s, dir, false)

	var realSHA string
	if err := s.DB().QueryRow(`SELECT sha256 FROM model_file_path WHERE path = ?`, real).Scan(&realSHA); err != nil {
		t.Fatal(err)
	}
	// Force the bad state a false-positive probe would produce: the impostor's
	// path bound to the real file's hash, flagged provisional.
	if _, err := s.DB().Exec(
		`UPDATE model_file_path SET sha256 = ?, provisional = 1 WHERE path = ?`,
		realSHA, impostor); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), s, Options{ProvisionalOnly: true, Workers: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProbeMisbindings != 1 {
		t.Fatalf("ProbeMisbindings = %d, want 1", res.ProbeMisbindings)
	}
	if res.Mismatched != 1 {
		t.Fatalf("mismatched = %d, want 1", res.Mismatched)
	}

	var bound string
	var provisional int
	if err := s.DB().QueryRow(
		`SELECT sha256, provisional FROM model_file_path WHERE path = ?`, impostor,
	).Scan(&bound, &provisional); err != nil {
		t.Fatal(err)
	}
	if bound == realSHA {
		t.Fatal("the wrong binding survived verification")
	}
	if provisional != 0 {
		t.Fatal("the path is still provisional after a full-hash verification")
	}
}

func TestVerifyMarksVanishedFilesAbsent(t *testing.T) {
	dir := t.TempDir()
	path := writeModel(t, dir, "a.safetensors", []byte{1})
	writeModel(t, dir, "b.safetensors", []byte{2})

	s := newStore(t)
	scanDir(t, s, dir, false)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), s, Options{Sample: 0, Workers: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Missing != 1 {
		t.Fatalf("missing = %d, want 1", res.Missing)
	}
	if res.Errors != 0 {
		t.Fatalf("errors = %d; a vanished file is a fact, not a failure", res.Errors)
	}

	var present int
	if err := s.DB().QueryRow(`SELECT present FROM model_file_path WHERE path = ?`, path).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatal("the vanished file's path is still marked present")
	}
}

func TestVerifySampleLimitsWork(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		writeModel(t, dir, n+".safetensors", []byte(n))
	}
	s := newStore(t)
	scanDir(t, s, dir, false)

	res, err := Run(context.Background(), s, Options{Sample: 2, Workers: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 2 {
		t.Fatalf("checked %d, want 2", res.Checked)
	}
}

func TestVerifyOnEmptyIndex(t *testing.T) {
	s := newStore(t)
	res, err := Run(context.Background(), s, Options{Sample: 0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 0 {
		t.Fatalf("checked %d on an empty index", res.Checked)
	}
}
