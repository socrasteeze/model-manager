package tier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/store"
)

type fixture struct {
	st        *store.Store
	mgr       *Manager
	modelRoot string
	cacheRoot string
}

func fnvHash(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func newFixture(t *testing.T, capacity int64) *fixture {
	t.Helper()
	base := t.TempDir()

	st, err := store.Open(filepath.Join(base, "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{
		st:        st,
		modelRoot: filepath.Join(base, "models"),
		cacheRoot: filepath.Join(base, "ssd"),
	}
	if err := os.MkdirAll(f.modelRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(st, f.cacheRoot, capacity)
	if err != nil {
		t.Fatal(err)
	}
	f.mgr = mgr
	return f
}

// addModel writes a real file and indexes it under its real hash, so staging
// and verification exercise the same identity the rest of the system uses.
func (f *fixture) addModel(t *testing.T, name string, size int, provisional bool) string {
	t.Helper()

	// Derive the bytes from the name itself, not from its length: two names of
	// equal length would otherwise produce identical content and therefore the
	// same hash, which silently turns a three-model test into a two-model one.
	data := make([]byte, size)
	for i := range data {
		data[i] = name[i%len(name)] ^ byte(i)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	path := filepath.Join(f.modelRoot, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)

	run, _ := f.st.BeginScanRun(f.modelRoot)
	if err := f.st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha[:8], Size: int64(size), Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: path, Root: f.modelRoot,
			Device: 1, Inode: fnvHash(name), Size: int64(size),
			MtimeNs: info.ModTime().UnixNano(), Provisional: provisional, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
	return sha
}

// An SSD copy is a second path on the same hash -- no new concept (§16.3).
func TestStageRecordsASecondPathOnTheSameHash(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 4096, false)

	entry, err := f.mgr.Stage(context.Background(), sha, StageOptions{Verify: true})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := os.Stat(entry.CachePath); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}

	paths, err := f.st.PathsFor(sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("%d paths for the hash, want the original plus the staged copy", len(paths))
	}

	var foundCache bool
	for _, p := range paths {
		if p.Path == entry.CachePath {
			foundCache = true
			if !p.Present {
				t.Error("the staged copy was recorded as not present")
			}
		}
	}
	if !foundCache {
		t.Fatal("the staged copy is not in the index")
	}
}

// The whole design rests on a path meaning the content its hash claims. A tier
// copy that silently differs would serve wrong weights, so verification has to
// catch a source that changed since it was indexed.
func TestVerificationRejectsAMismatchedCopy(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 2048, false)
	original := filepath.Join(f.modelRoot, "model.safetensors")

	// The file changed on disk after being indexed -- a tool rewrote it, or the
	// bytes rotted. The index still claims the old hash.
	if err := os.WriteFile(original, []byte("entirely different content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.mgr.Stage(context.Background(), sha, StageOptions{Verify: true}); err == nil {
		t.Fatal("a copy whose hash does not match the index was admitted")
	}

	// Nothing may be left behind in the cache.
	entries, _ := f.mgr.entries()
	if len(entries) != 0 {
		t.Fatalf("failed staging left %d manifest entries", len(entries))
	}
	staged, _ := filepath.Glob(filepath.Join(f.cacheRoot, "*", "*"))
	if len(staged) != 0 {
		t.Fatalf("failed staging left files behind: %v", staged)
	}
}

// Without verification the same mismatch is admitted, which is why the CLI turns
// verification on.
func TestUnverifiedStagingAcceptsWhateverIsThere(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 2048, false)
	if err := os.WriteFile(filepath.Join(f.modelRoot, "model.safetensors"),
		[]byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.mgr.Stage(context.Background(), sha, StageOptions{Verify: false}); err != nil {
		t.Fatalf("unverified staging failed: %v", err)
	}
}

// Staging presents copied bytes as a model, which is a write-side decision, and
// §10.1 bars a probe-bound path from those.
func TestProvisionalPathsCannotBeStaged(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 1024, true)

	if _, err := f.mgr.Stage(context.Background(), sha, StageOptions{}); err == nil {
		t.Fatal("a provisional path was staged")
	}
}

// Unstaging must never touch the original -- that is what makes a tier copy
// disposable.
func TestUnstageLeavesTheOriginalAlone(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 2048, false)
	original := filepath.Join(f.modelRoot, "model.safetensors")

	entry, err := f.mgr.Stage(context.Background(), sha, StageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(original)

	if err := f.mgr.Unstage(sha); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if _, err := os.Stat(entry.CachePath); !os.IsNotExist(err) {
		t.Fatal("the staged copy survived unstaging")
	}

	after, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("the original was removed: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the original was modified")
	}

	// The index should stop claiming a copy that is not there.
	paths, _ := f.st.PathsFor(sha)
	for _, p := range paths {
		if p.Path == entry.CachePath && p.Present {
			t.Fatal("the index still reports the removed copy as present")
		}
	}
}

// A corrupted record must not become a way to delete an original.
func TestUnstageRefusesPathsOutsideTheCache(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 512, false)
	if _, err := f.mgr.Stage(context.Background(), sha, StageOptions{}); err != nil {
		t.Fatal(err)
	}

	// Corrupt the manifest to point at the original.
	entry, _ := f.mgr.entry(sha)
	entry.CachePath = filepath.Join(f.modelRoot, "model.safetensors")
	if err := f.mgr.save(*entry); err != nil {
		t.Fatal(err)
	}

	if err := f.mgr.Unstage(sha); err == nil {
		t.Fatal("Unstage accepted a path outside the cache root")
	}
	if _, err := os.Stat(entry.CachePath); err != nil {
		t.Fatal("the original was deleted through a corrupted record")
	}
}

func TestLRUEviction(t *testing.T) {
	// Room for two 1KiB models, not three.
	f := newFixture(t, 2500)

	first := f.addModel(t, "first.safetensors", 1024, false)
	second := f.addModel(t, "second.safetensors", 1024, false)
	third := f.addModel(t, "third.safetensors", 1024, false)

	if _, err := f.mgr.Stage(context.Background(), first, StageOptions{}); err != nil {
		t.Fatal(err)
	}
	// Ensure a distinct LastUsed ordering.
	time.Sleep(2 * time.Millisecond)
	if _, err := f.mgr.Stage(context.Background(), second, StageOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := f.mgr.Stage(context.Background(), third, StageOptions{}); err != nil {
		t.Fatal(err)
	}

	status, err := f.mgr.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Entries) != 2 {
		t.Fatalf("%d entries, want 2 after eviction", len(status.Entries))
	}
	for _, e := range status.Entries {
		if e.SHA256 == first {
			t.Fatal("the least recently used entry survived eviction")
		}
	}
}

func TestPinnedEntriesAreNeverEvicted(t *testing.T) {
	f := newFixture(t, 2500)

	pinned := f.addModel(t, "pinned.safetensors", 1024, false)
	second := f.addModel(t, "second.safetensors", 1024, false)
	third := f.addModel(t, "third.safetensors", 1024, false)

	if _, err := f.mgr.Stage(context.Background(), pinned, StageOptions{Pin: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := f.mgr.Stage(context.Background(), second, StageOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := f.mgr.Stage(context.Background(), third, StageOptions{}); err != nil {
		t.Fatal(err)
	}

	status, _ := f.mgr.Status()
	found := false
	for _, e := range status.Entries {
		if e.SHA256 == pinned {
			found = true
		}
	}
	if !found {
		t.Fatal("a pinned entry was evicted despite being the least recently used")
	}
}

// A cache full of pinned entries cannot make room, and saying so beats silently
// evicting something the user pinned.
func TestFullyPinnedCacheReportsRatherThanEvicting(t *testing.T) {
	f := newFixture(t, 1200)

	first := f.addModel(t, "first.safetensors", 1024, false)
	second := f.addModel(t, "second.safetensors", 1024, false)

	if _, err := f.mgr.Stage(context.Background(), first, StageOptions{Pin: true}); err != nil {
		t.Fatal(err)
	}
	_, err := f.mgr.Stage(context.Background(), second, StageOptions{})
	if err == nil {
		t.Fatal("staging succeeded despite a fully pinned cache")
	}

	status, _ := f.mgr.Status()
	if len(status.Entries) != 1 {
		t.Fatalf("the pinned entry was disturbed: %d entries", len(status.Entries))
	}
}

// Re-staging must not recopy gigabytes.
func TestStagingTwiceIsCheap(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 4096, false)

	first, err := f.mgr.Stage(context.Background(), sha, StageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info1, _ := os.Stat(first.CachePath)

	time.Sleep(5 * time.Millisecond)
	second, err := f.mgr.Stage(context.Background(), sha, StageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(second.CachePath)

	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("the file was recopied on a second stage")
	}
	status, _ := f.mgr.Status()
	if len(status.Entries) != 1 {
		t.Fatalf("%d entries after staging the same model twice", len(status.Entries))
	}
}

// A record claiming a staged copy that is gone must restage rather than serve a
// missing file.
func TestMissingStagedFileIsRestaged(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 1024, false)

	entry, err := f.mgr.Stage(context.Background(), sha, StageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entry.CachePath); err != nil {
		t.Fatal(err)
	}

	again, err := f.mgr.Stage(context.Background(), sha, StageOptions{})
	if err != nil {
		t.Fatalf("restage after a vanished copy: %v", err)
	}
	if _, err := os.Stat(again.CachePath); err != nil {
		t.Fatal("the model was not restaged")
	}
}

func TestStatusSummary(t *testing.T) {
	f := newFixture(t, 10<<30)
	sha := f.addModel(t, "model.safetensors", 2048, false)
	if _, err := f.mgr.Stage(context.Background(), sha, StageOptions{Pin: true}); err != nil {
		t.Fatal(err)
	}

	status, err := f.mgr.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.UsedBytes != 2048 || status.Pinned != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Summary() == "" {
		t.Fatal("empty summary")
	}
}

func TestStageCancellation(t *testing.T) {
	f := newFixture(t, 0)
	sha := f.addModel(t, "model.safetensors", 1<<20, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.mgr.Stage(ctx, sha, StageOptions{}); err == nil {
		t.Fatal("a cancelled stage reported success")
	}
	staged, _ := filepath.Glob(filepath.Join(f.cacheRoot, "*", "*"))
	if len(staged) != 0 {
		t.Fatalf("cancellation left files behind: %v", staged)
	}
}

func TestStageUnknownModel(t *testing.T) {
	f := newFixture(t, 0)
	if _, err := f.mgr.Stage(context.Background(), "nosuchhash", StageOptions{}); err == nil {
		t.Fatal("staging an unknown hash reported success")
	}
}
