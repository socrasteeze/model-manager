package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

type fixture struct {
	st        *store.Store
	modelRoot string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{st: st, modelRoot: filepath.Join(base, "models")}
	if err := os.MkdirAll(f.modelRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) addModel(t *testing.T, sha, filename string, obs []store.FieldObservation, tags []string) string {
	t.Helper()
	path := filepath.Join(f.modelRoot, filename)
	if err := os.WriteFile(path, []byte("model "+sha), 0o644); err != nil {
		t.Fatal(err)
	}
	run, _ := f.st.BeginScanRun(f.modelRoot)
	if err := f.st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 10, Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: path, Root: f.modelRoot,
			Device: 1, Inode: fnv(sha), Size: 10, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
	if len(obs) > 0 {
		if err := f.st.RecordObservations(sha, provenance.SourceCivitai, obs); err != nil {
			t.Fatal(err)
		}
	}
	if len(tags) > 0 {
		if err := f.st.SetTags(sha, provenance.SourceCivitai, tags); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}
	return path
}

func fnv(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func standardObs() []store.FieldObservation {
	weight := 0.75
	return []store.FieldObservation{
		{Field: provenance.FieldName, Value: "Cinematic Style"},
		{Field: provenance.FieldType, Value: "lora"},
		{Field: provenance.FieldBaseModel, Value: "SDXL"},
		{Field: provenance.FieldVersion, Value: "v2.0"},
		{Field: provenance.FieldDescription, Value: "Adds drama"},
		{Field: provenance.FieldTriggerWords, Value: []string{"cinelight", "dramatic"}},
		{Field: provenance.FieldRecommendedWeight, Value: weight},
		{Field: provenance.FieldNSFW, Value: false},
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return out
}

func TestStabilityMatrixDialect(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), []string{"style"})

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix},
	}); err != nil {
		t.Fatal(err)
	}

	body := readJSON(t, filepath.Join(f.modelRoot, "model.cm-info.json"))
	if body["ModelName"] != "Cinematic Style" {
		t.Errorf("ModelName = %v", body["ModelName"])
	}
	if body["BaseModel"] != "SDXL" {
		t.Errorf("BaseModel = %v", body["BaseModel"])
	}
	if body["ModelType"] != "Lora" {
		t.Errorf("ModelType = %v, want the dialect's own spelling", body["ModelType"])
	}
	// This dialect asks the NSFW question the other way round.
	if body["IsSfw"] != true {
		t.Errorf("IsSfw = %v, want true for a non-NSFW model", body["IsSfw"])
	}
	words, _ := body["TrainedWords"].([]any)
	if len(words) != 2 {
		t.Errorf("TrainedWords = %v", body["TrainedWords"])
	}
}

func TestA1111Dialect(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	if _, err := Run(context.Background(), f.st, Options{Targets: []Target{TargetA1111}}); err != nil {
		t.Fatal(err)
	}

	body := readJSON(t, filepath.Join(f.modelRoot, "model.json"))
	if body["activation text"] != "cinelight, dramatic" {
		t.Errorf("activation text = %v", body["activation text"])
	}
	if body["preferred weight"] != 0.75 {
		t.Errorf("preferred weight = %v", body["preferred weight"])
	}
	if body["sd version"] != "SDXL" {
		t.Errorf("sd version = %v", body["sd version"])
	}
}

func TestSwarmAndLoRAManagerDialects(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), []string{"style"})

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetSwarmUI, TargetLoRAManager},
	}); err != nil {
		t.Fatal(err)
	}

	swarm := readJSON(t, filepath.Join(f.modelRoot, "model.json"))
	if swarm["trigger_phrase"] != "cinelight, dramatic" {
		t.Errorf("trigger_phrase = %v", swarm["trigger_phrase"])
	}
	if swarm["architecture"] != "stable-diffusion-xl-v1-base" {
		t.Errorf("architecture = %v", swarm["architecture"])
	}

	lm := readJSON(t, filepath.Join(f.modelRoot, "model.metadata.json"))
	if lm["base_model"] != "SDXL" {
		t.Errorf("base_model = %v", lm["base_model"])
	}
	// usage_tips is a JSON document stored as a string in this dialect.
	tips, _ := lm["usage_tips"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tips), &parsed); err != nil {
		t.Fatalf("usage_tips is not JSON: %q", tips)
	}
	if parsed["strength"] != 0.75 {
		t.Errorf("usage_tips strength = %v", parsed["strength"])
	}
}

// SwarmUI and A1111 both claim <stem>.json. Projecting both into one directory
// would have the second silently overwrite the first.
func TestConflictingTargetsAreDetectable(t *testing.T) {
	if got := ConflictingTargets([]Target{TargetSwarmUI, TargetA1111}); len(got) != 2 {
		t.Fatalf("the swarm/a1111 filename collision was not reported: %v", got)
	}
	if got := ConflictingTargets([]Target{TargetSwarmUI, TargetStabilityMatrix}); got != nil {
		t.Fatalf("a false conflict was reported: %v", got)
	}
}

// The whole point: a tool mangles a sidecar, and regenerating puts it back.
func TestRegenerationRepairsAStompedSidecar(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix},
	}); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(f.modelRoot, "model.cm-info.json")

	// A tool's "pull metadata" button empties it.
	if err := os.WriteFile(sidecar, []byte(`{"_generated_by":"model-manager","ModelName":""}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix},
	}); err != nil {
		t.Fatal(err)
	}
	body := readJSON(t, sidecar)
	if body["ModelName"] != "Cinematic Style" {
		t.Fatalf("ModelName = %v; regeneration did not repair the stomped sidecar", body["ModelName"])
	}
}

// A file another tool authored may hold something master never captured.
// Replacing it silently would be the destructive stomp this project exists to
// stop.
func TestForeignSidecarsAreNotOverwritten(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	foreign := filepath.Join(f.modelRoot, "model.cm-info.json")
	original := `{"ModelName":"Written By Another Tool","SomethingWeNeverCaptured":42}`
	if err := os.WriteFile(foreign, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), f.st, Options{Targets: []Target{TargetStabilityMatrix}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 0 || stats.Skipped == 0 {
		t.Fatalf("stats = %+v; a foreign sidecar was overwritten", stats)
	}
	got, _ := os.ReadFile(foreign)
	if string(got) != original {
		t.Fatal("the foreign sidecar was modified")
	}

	// With --overwrite the user has said to do it anyway.
	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix}, Overwrite: true,
	}); err != nil {
		t.Fatal(err)
	}
	body := readJSON(t, foreign)
	if body["ModelName"] != "Cinematic Style" {
		t.Fatal("--overwrite did not replace the foreign sidecar")
	}
}

// Writing an empty sidecar is worse than writing none: a tool would read it as
// authoritative emptiness.
func TestModelsWithNoMetadataAreSkipped(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "bare", "bare.safetensors", nil, nil)

	stats, err := Run(context.Background(), f.st, Options{Targets: []Target{TargetStabilityMatrix}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 0 {
		t.Fatalf("wrote %d sidecars for a model with no metadata", stats.Written)
	}
	if _, err := os.Stat(filepath.Join(f.modelRoot, "bare.cm-info.json")); err == nil {
		t.Fatal("an empty sidecar was written")
	}
}

// §10.1 bars a probe-bound path from projection by name.
func TestProvisionalPathsAreNotProjected(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)
	if _, err := f.st.DB().Exec(`UPDATE model_file_path SET provisional = 1`); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), f.st, Options{Targets: []Target{TargetStabilityMatrix}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ModelsConsidered != 0 || stats.Written != 0 {
		t.Fatalf("stats = %+v; a provisional path was projected", stats)
	}
}

// §15: start with one tool, verify, then add others. There is deliberately no
// "everything" default.
func TestNoTargetsIsAnError(t *testing.T) {
	f := newFixture(t)
	if _, err := Run(context.Background(), f.st, Options{}); err == nil {
		t.Fatal("projection ran with no targets specified")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	stats, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix}, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 1 {
		t.Fatalf("dry run reported %d writes", stats.Written)
	}
	if _, err := os.Stat(filepath.Join(f.modelRoot, "model.cm-info.json")); err == nil {
		t.Fatal("dry run wrote a file")
	}
}

// Verifying one dialect on one model before letting it loose on a library.
func TestOnlySHARestrictsTheRun(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "one.safetensors", standardObs(), nil)
	f.addModel(t, "bbb", "two.safetensors", standardObs(), nil)

	stats, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix}, OnlySHA: "aaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 1 {
		t.Fatalf("wrote %d, want 1", stats.Written)
	}
	if _, err := os.Stat(filepath.Join(f.modelRoot, "two.cm-info.json")); err == nil {
		t.Fatal("a model outside the restriction was projected")
	}
}

// A tool reading concurrently must never see a half-written sidecar -- that is
// the disconnected-metadata symptom this project exists to eliminate.
func TestNoTemporaryFilesSurvive(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix, TargetA1111},
	}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.modelRoot)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".mm-tmp" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

// Nulls are dropped rather than written: a tool reading one may treat it as an
// authoritative "no value" and display nothing where it would otherwise have
// shown its own.
func TestEmptyValuesAreOmitted(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", []store.FieldObservation{
		{Field: provenance.FieldName, Value: "Only A Name"},
	}, nil)

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix},
	}); err != nil {
		t.Fatal(err)
	}
	body := readJSON(t, filepath.Join(f.modelRoot, "model.cm-info.json"))
	if _, present := body["ModelDescription"]; present {
		t.Error("an empty description was written")
	}
	if _, present := body["BaseModel"]; present {
		t.Error("an empty base model was written")
	}
	if body["ModelName"] != "Only A Name" {
		t.Errorf("ModelName = %v", body["ModelName"])
	}
}

// The marker is what lets regeneration be safe and foreign files be left alone.
func TestGeneratorMarkerIsWrittenAndRecognized(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaa", "model.safetensors", standardObs(), nil)

	if _, err := Run(context.Background(), f.st, Options{
		Targets: []Target{TargetStabilityMatrix},
	}); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(f.modelRoot, "model.cm-info.json")

	body := readJSON(t, sidecar)
	if body[markerKey] != generatorMarker {
		t.Fatalf("marker = %v", body[markerKey])
	}
	if !ourSidecar(sidecar) {
		t.Fatal("a sidecar this app wrote was not recognized as its own")
	}

	foreign := filepath.Join(f.modelRoot, "foreign.json")
	if err := os.WriteFile(foreign, []byte(`{"ModelName":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ourSidecar(foreign) {
		t.Fatal("a foreign file was claimed as ours")
	}
	if ourSidecar(filepath.Join(f.modelRoot, "does-not-exist.json")) {
		t.Fatal("a missing file was claimed as ours")
	}
}
