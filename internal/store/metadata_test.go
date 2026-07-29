package store

import (
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

func metaStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "master.db"), Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	run, err := s.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	sha := "modelhash"
	err = s.UpsertFileAndPath(
		ModelFile{SHA256: sha, ProbeSHA256: "p", Size: 100, Format: "safetensors"},
		FilePath{SHA256: sha, Path: "/models/m.safetensors", Root: "/models",
			Device: 1, Inode: 1, Size: 100, MtimeNs: 1, ScanRunID: run})
	if err != nil {
		t.Fatal(err)
	}
	return s, sha
}

func obs(field string, value any) []FieldObservation {
	return []FieldObservation{{Field: field, Value: value}}
}

func TestManualIsNeverOverwrittenByIngest(t *testing.T) {
	s, sha := metaStore(t)

	if err := s.RecordObservations(sha, provenance.SourceManual,
		obs(provenance.FieldBaseModel, "Anima 2B")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.ResolveModel(sha)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BaseModel != "Anima 2B" {
		t.Fatalf("BaseModel = %q, want Anima 2B", rec.BaseModel)
	}

	// Every lower tier now insists otherwise. None of them may win.
	for _, src := range []string{
		provenance.SourceCivitai, provenance.SourceSwarmUI,
		provenance.SourceStabilityMatrix, provenance.SourceSafetensorsHeader,
	} {
		if err := s.RecordObservations(sha, src, obs(provenance.FieldBaseModel, "SDXL")); err != nil {
			t.Fatal(err)
		}
	}
	rec, err = s.ResolveModel(sha)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BaseModel != "Anima 2B" {
		t.Fatalf("BaseModel = %q after ingest, want the manual value to survive", rec.BaseModel)
	}
}

// Self-trained LoRAs have no remote record at all, so manual is the only source
// for a real slice of the library. Every field type must survive the round trip.
func TestManualCoversEveryFieldType(t *testing.T) {
	s, sha := metaStore(t)

	err := s.RecordObservations(sha, provenance.SourceManual, []FieldObservation{
		{Field: provenance.FieldName, Value: "My Anima LoRA"},
		{Field: provenance.FieldType, Value: "lora"},
		{Field: provenance.FieldTriggerWords, Value: []string{"animastyle", "aniv2"}},
		{Field: provenance.FieldRecommendedWeight, Value: 0.75},
		{Field: provenance.FieldNSFW, Value: false},
		{Field: provenance.FieldOrigin, Value: "self-trained"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}

	rec, err := s.GetModelRecord(sha)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "My Anima LoRA" || rec.Type != "lora" || rec.Origin != "self-trained" {
		t.Fatalf("scalar fields did not round-trip: %+v", rec)
	}
	if len(rec.TriggerWords) != 2 || rec.TriggerWords[0] != "animastyle" {
		t.Fatalf("TriggerWords = %v", rec.TriggerWords)
	}
	if rec.RecommendedWeight == nil || *rec.RecommendedWeight != 0.75 {
		t.Fatalf("RecommendedWeight = %v", rec.RecommendedWeight)
	}
	// A false NSFW flag is a real answer, and must not be indistinguishable from
	// never having been asked.
	if rec.NSFW == nil || *rec.NSFW != false {
		t.Fatalf("NSFW = %v, want an explicit false", rec.NSFW)
	}
}

// Without an explicit clear, a mistyped manual value is unfixable by ingest
// forever (§7.1).
func TestClearManualLetsLowerTiersResolveAgain(t *testing.T) {
	s, sha := metaStore(t)

	if err := s.RecordObservations(sha, provenance.SourceCivitai,
		obs(provenance.FieldBaseModel, "SDXL")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservations(sha, provenance.SourceManual,
		obs(provenance.FieldBaseModel, "typo")); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.ResolveModel(sha)
	if rec.BaseModel != "typo" {
		t.Fatalf("BaseModel = %q, want the manual value", rec.BaseModel)
	}

	if err := s.ClearManualField(sha, provenance.FieldBaseModel); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.ResolveModel(sha)
	if rec.BaseModel != "SDXL" {
		t.Fatalf("BaseModel = %q after clearing, want the origin value to resurface", rec.BaseModel)
	}
}

func TestOriginDisagreementRaisesAndWithdrawsSuggestions(t *testing.T) {
	s, sha := metaStore(t)

	if err := s.RecordObservations(sha, provenance.SourceManual,
		obs(provenance.FieldBaseModel, "Anima 2B")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservations(sha, provenance.SourceCivitai,
		obs(provenance.FieldBaseModel, "SDXL")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingSuggestions(sha, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending suggestions, want 1", len(pending))
	}
	if pending[0].Field != provenance.FieldBaseModel {
		t.Fatalf("suggestion on the wrong field: %+v", pending[0])
	}

	// The user edits their value to agree. The suggestion has nothing left to
	// say and must go away rather than linger as permanent noise.
	if err := s.RecordObservations(sha, provenance.SourceManual,
		obs(provenance.FieldBaseModel, "SDXL")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingSuggestions(sha, 0)
	if len(pending) != 0 {
		t.Fatalf("%d pending suggestions after the conflict resolved, want 0", len(pending))
	}
}

func TestAcceptSuggestionMakesTheValueManual(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceManual, obs(provenance.FieldName, "old name"))
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "Real Name v2"))
	if _, err := s.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}

	pending, _ := s.PendingSuggestions(sha, 0)
	if len(pending) != 1 {
		t.Fatalf("%d suggestions, want 1", len(pending))
	}
	if err := s.AcceptSuggestion(pending[0].ID); err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}

	rec, _ := s.GetModelRecord(sha)
	if rec.Name != "Real Name v2" {
		t.Fatalf("Name = %q after accepting, want Real Name v2", rec.Name)
	}

	// Accepted means adopted as manual -- a later ingest still cannot move it.
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "renamed upstream"))
	if _, err := s.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.GetModelRecord(sha)
	if rec.Name != "Real Name v2" {
		t.Fatalf("Name = %q; an accepted value must be sticky", rec.Name)
	}
}

// Dismissing means "I know, stop asking". It must stay dismissed on re-ingest of
// the same value, or dismissal would achieve nothing.
func TestDismissedSuggestionStaysDismissedUntilTheOfferChanges(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceManual, obs(provenance.FieldName, "mine"))
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "theirs"))
	_, _ = s.ResolveModel(sha)

	pending, _ := s.PendingSuggestions(sha, 0)
	if err := s.DismissSuggestion(pending[0].ID); err != nil {
		t.Fatal(err)
	}

	// Same offer again.
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "theirs"))
	_, _ = s.ResolveModel(sha)
	if p, _ := s.PendingSuggestions(sha, 0); len(p) != 0 {
		t.Fatalf("%d pending after re-ingesting the same value, want 0", len(p))
	}

	// A genuinely new offer deserves to be asked again.
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "something new"))
	_, _ = s.ResolveModel(sha)
	if p, _ := s.PendingSuggestions(sha, 0); len(p) != 1 {
		t.Fatalf("%d pending after a changed value, want 1", len(p))
	}
}

// Adapters routinely emit blank fields for keys their sidecar did not have.
// Storing those would let an absence outrank a real value from a lower tier.
func TestEmptyObservationsAreNotRecorded(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceSwarmUI, obs(provenance.FieldName, "a real name"))
	err := s.RecordObservations(sha, provenance.SourceCivitai, []FieldObservation{
		{Field: provenance.FieldName, Value: ""},
		{Field: provenance.FieldTriggerWords, Value: []string{}},
		{Field: provenance.FieldDescription, Value: nil},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, _ := s.ResolveModel(sha)
	if rec.Name != "a real name" {
		t.Fatalf("Name = %q; an empty higher-tier value overrode a real one", rec.Name)
	}
}

// Re-ingesting from one source must refresh only that source's opinion.
func TestReingestDoesNotDisturbOtherSources(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceSwarmUI, obs(provenance.FieldName, "swarm name"))
	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "civitai name"))
	_ = s.RecordObservations(sha, provenance.SourceSwarmUI, obs(provenance.FieldName, "swarm name v2"))

	cands, err := s.Candidates(sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("%d candidates, want 2 (one per source)", len(cands))
	}
	rec, _ := s.ResolveModel(sha)
	if rec.Name != "civitai name" {
		t.Fatalf("Name = %q, want the origin value", rec.Name)
	}
}

func TestResolveAllRematerializesEverything(t *testing.T) {
	s, sha := metaStore(t)

	run, _ := s.BeginScanRun("/models")
	second := "otherhash"
	if err := s.UpsertFileAndPath(
		ModelFile{SHA256: second, ProbeSHA256: "p2", Size: 10, Format: "safetensors"},
		FilePath{SHA256: second, Path: "/models/n.safetensors", Root: "/models",
			Device: 1, Inode: 2, Size: 10, MtimeNs: 1, ScanRunID: run}); err != nil {
		t.Fatal(err)
	}

	_ = s.RecordObservations(sha, provenance.SourceCivitai, obs(provenance.FieldName, "one"))
	_ = s.RecordObservations(second, provenance.SourceCivitai, obs(provenance.FieldName, "two"))

	n, err := s.ResolveAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("resolved %d models, want 2", n)
	}
	for _, h := range []string{sha, second} {
		rec, _ := s.GetModelRecord(h)
		if rec == nil || rec.Name == "" {
			t.Fatalf("%s was not materialized", h)
		}
	}
}

func TestGetModelRecordMissing(t *testing.T) {
	s, _ := metaStore(t)
	rec, err := s.GetModelRecord("nonexistent")
	if err != nil {
		t.Fatalf("GetModelRecord: %v", err)
	}
	if rec != nil {
		t.Fatal("a missing record returned a value")
	}
}

// A derived source recomputes everything it knows on every run, so a field it
// stops producing is a stale artifact of an older rule -- not a surviving
// opinion. Merging would mean an interpretation bug could never be fully fixed.
func TestReplaceObservationsRetractsStaleFields(t *testing.T) {
	s, sha := metaStore(t)

	if err := s.ReplaceObservations(sha, provenance.SourcePathHeuristic, []FieldObservation{
		{Field: provenance.FieldName, Value: "ckpt 0"},
		{Field: provenance.FieldVersion, Value: "0"},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.ResolveModel(sha)
	if rec.Version != "0" {
		t.Fatalf("setup failed: version = %q", rec.Version)
	}

	// An improved rule no longer treats a bare trailing digit as a version.
	if err := s.ReplaceObservations(sha, provenance.SourcePathHeuristic, []FieldObservation{
		{Field: provenance.FieldName, Value: "ckpt 0"},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.ResolveModel(sha)
	if rec.Version != "" {
		t.Fatalf("version = %q; the stale value survived an improved rule", rec.Version)
	}
	if rec.Name != "ckpt 0" {
		t.Fatalf("name = %q; replacing dropped a field it should have kept", rec.Name)
	}
}

// Replacing one source must not disturb another's opinion.
func TestReplaceObservationsIsScopedToItsSource(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceCivitai,
		obs(provenance.FieldVersion, "v3.0"))
	_ = s.ReplaceObservations(sha, provenance.SourcePathHeuristic,
		obs(provenance.FieldVersion, "0"))
	_ = s.ReplaceObservations(sha, provenance.SourcePathHeuristic,
		obs(provenance.FieldName, "just a name"))

	rec, _ := s.ResolveModel(sha)
	if rec.Version != "v3.0" {
		t.Fatalf("version = %q; replacing a derived source clobbered the origin", rec.Version)
	}
}

// An external sidecar's silence is not evidence: a tool that crashes halfway
// through writing one must not be able to erase a value.
func TestRecordObservationsStillMerges(t *testing.T) {
	s, sha := metaStore(t)

	_ = s.RecordObservations(sha, provenance.SourceSwarmUI, []FieldObservation{
		{Field: provenance.FieldName, Value: "From Swarm"},
		{Field: provenance.FieldVersion, Value: "v2"},
	})
	// A later read of a truncated sidecar mentions only the name.
	_ = s.RecordObservations(sha, provenance.SourceSwarmUI,
		obs(provenance.FieldName, "From Swarm"))

	rec, _ := s.ResolveModel(sha)
	if rec.Version != "v2" {
		t.Fatalf("version = %q; a partial sidecar erased a value", rec.Version)
	}
}
