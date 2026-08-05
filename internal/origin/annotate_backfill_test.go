package origin

import (
	"encoding/json"
	"testing"

	"github.com/socrasteeze/model-manager/internal/store"
)

const bfSHA = "2222222222222222222222222222222222222222222222222222222222222222"

// The backfill turns archived responses into persisted, joinable identity.
func TestBackfillModelOriginDecodesArchivedResponses(t *testing.T) {
	st := testStore(t)
	seed(t, st, bfSHA, false)

	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, bfSHA,
		json.RawMessage(`{"id":100,"modelId":42,"name":"v1"}`), 200); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillModelOrigin(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want 1", n)
	}

	got, err := st.ModelOrigins(bfSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelID != "42" || got[0].VersionID != "100" {
		t.Fatalf("persisted identity = %+v, want model 42 version 100", got)
	}

	// Idempotent: a second run has nothing left to do.
	if n, err = BackfillModelOrigin(st); err != nil || n != 0 {
		t.Errorf("second backfill wrote %d rows (err %v), want 0", n, err)
	}
}

// A body the decoder cannot read is skipped, not fatal -- it is one model's
// identity, and failing the whole backfill over it would take the update sweep
// with it.
func TestBackfillSkipsUndecodableBodies(t *testing.T) {
	st := testStore(t)
	seed(t, st, bfSHA, false)

	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, bfSHA,
		json.RawMessage(`{"nothing":"useful"}`), 200); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillModelOrigin(st)
	if err != nil {
		t.Fatalf("an undecodable body failed the whole backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("wrote %d rows for an undecodable body, want 0", n)
	}
}

// An archived response can outlive the file it described -- the archive is
// deliberately never pruned -- and model_origin.sha256 is a foreign key.
func TestBackfillSkipsOrphanedArchiveRows(t *testing.T) {
	st := testStore(t)
	// No seed(): the archive knows this hash, the library does not.
	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, bfSHA,
		json.RawMessage(`{"id":100,"modelId":42,"name":"v1"}`), 200); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillModelOrigin(st)
	if err != nil {
		t.Fatalf("an orphaned archive row failed the backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("wrote %d rows for a file not in the library, want 0", n)
	}
}

// The persisted rows are a real fast path, not a fallback that never fires:
// with identity stored, ownership survives the archive being gone.
func TestBuildLocalIndexUsesPersistedIdentity(t *testing.T) {
	st := testStore(t)
	seed(t, st, bfSHA, false)

	if err := st.PutModelOrigin(store.ModelOrigin{
		SHA256: bfSHA, Provider: ProviderCivitaiID,
		ModelID: "42", VersionID: "100", VersionName: "v1",
	}); err != nil {
		t.Fatal(err)
	}

	idx, err := BuildLocalIndex(st)
	if err != nil {
		t.Fatal(err)
	}
	owned := idx.OwnedModelIDs(ProviderCivitaiID)
	if len(owned) != 1 || owned[0] != "42" {
		t.Fatalf("owned models = %v, want [42] from the persisted row alone", owned)
	}
	if v := idx.OwnedVersionIDs(ProviderCivitaiID, "42"); len(v) != 1 || v[0] != "100" {
		t.Errorf("owned versions = %v, want [100]", v)
	}
}

// The archive fallback must not double-count what the persisted rows already
// cover, or "the version you have" gets duplicate entries.
func TestPersistedAndArchivedIdentityAreNotDoubleCounted(t *testing.T) {
	st := testStore(t)
	seed(t, st, bfSHA, false)

	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, bfSHA,
		json.RawMessage(`{"id":100,"modelId":42,"name":"v1"}`), 200); err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillModelOrigin(st); err != nil {
		t.Fatal(err)
	}

	idx, err := BuildLocalIndex(st)
	if err != nil {
		t.Fatal(err)
	}
	if v := idx.OwnedVersionIDs(ProviderCivitaiID, "42"); len(v) != 1 {
		t.Errorf("owned versions = %v, want exactly one entry", v)
	}
}
