package interpret

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

func runStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedModel(t *testing.T, s *store.Store, sha, path, format string, blob []byte) {
	t.Helper()
	run, err := s.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	err = s.UpsertFileAndPath(
		store.ModelFile{
			SHA256: sha, ProbeSHA256: "p" + sha, Size: 1000, Format: format,
			HeaderBlob: blob, HeaderOffset: 8,
		},
		store.FilePath{
			SHA256: sha, Path: path, Root: "/models",
			Device: 1, Inode: int64Hash(sha), Size: 1000, MtimeNs: 1, ScanRunID: run,
		})
	if err != nil {
		t.Fatal(err)
	}
}

func int64Hash(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// The whole bargain of Phase 0: headers were stored verbatim so this pass reads
// the database rather than 7.5TB of disk.
func TestRunInterpretsStoredHeadersWithoutTouchingDisk(t *testing.T) {
	s := runStore(t)

	meta := map[string]string{
		"ss_network_module":     "networks.lora",
		"ss_network_dim":        "16",
		"ss_base_model_version": "sdxl_base_v1-0",
		"ss_output_name":        "header_name",
	}
	seedModel(t, s, "aaa", "/models/loras/thing_v3.safetensors", "safetensors",
		header(t, []string{"lora_unet_x.lora_down.weight"}, meta))

	stats, err := Run(context.Background(), s, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Interpreted != 1 {
		t.Fatalf("interpreted %d, want 1", stats.Interpreted)
	}
	if stats.TrainingRecords != 1 {
		t.Fatalf("%d training records, want 1", stats.TrainingRecords)
	}

	rec, err := s.GetModelRecord("aaa")
	if err != nil || rec == nil {
		t.Fatalf("GetModelRecord: %v %v", rec, err)
	}
	// The header wins over the path heuristic for name and base model.
	if rec.Name != "header_name" {
		t.Errorf("Name = %q, want the header value to beat the filename", rec.Name)
	}
	if rec.BaseModel != "SDXL" {
		t.Errorf("BaseModel = %q", rec.BaseModel)
	}
	if rec.Type != "lora" {
		t.Errorf("Type = %q", rec.Type)
	}
	// The version came only from the filename, so the path heuristic still
	// contributes where the header is silent.
	if rec.Version != "v3" {
		t.Errorf("Version = %q, want v3 from the filename", rec.Version)
	}

	tr, err := s.GetTrainingRecord("aaa")
	if err != nil || tr == nil {
		t.Fatalf("GetTrainingRecord: %v %v", tr, err)
	}
	if tr.Base != "SDXL" {
		t.Errorf("training base = %q", tr.Base)
	}
}

// .ckpt has no parseable header by design. The path heuristics are the only
// metadata it will ever have before enrichment, so it must not be skipped.
func TestPickleFilesStillGetPathHeuristics(t *testing.T) {
	s := runStore(t)
	seedModel(t, s, "ckpt1", "/models/embeddings/badhands.pt", "pt", nil)

	if _, err := Run(context.Background(), s, Options{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetModelRecord("ckpt1")
	if rec == nil {
		t.Fatal("a pickle-format model was not interpreted at all")
	}
	if rec.Type != "embedding" || rec.Name != "badhands" {
		t.Fatalf("path heuristics did not apply: %+v", rec)
	}
}

func TestSkipPathHeuristics(t *testing.T) {
	s := runStore(t)
	seedModel(t, s, "aaa", "/models/loras/SDXL/thing.safetensors", "safetensors", nil)

	if _, err := Run(context.Background(), s, Options{SkipPathHeuristics: true}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetModelRecord("aaa")
	if rec != nil && (rec.Type != "" || rec.BaseModel != "" || rec.Name != "") {
		t.Fatalf("path heuristics ran despite being disabled: %+v", rec)
	}
}

// Re-running must be idempotent: the pass exists to be re-run whenever the
// interpretation rules improve.
func TestRunIsIdempotent(t *testing.T) {
	s := runStore(t)
	seedModel(t, s, "aaa", "/models/loras/thing.safetensors", "safetensors",
		header(t, []string{"lora_unet_x.lora_down.weight"}, nil))

	if _, err := Run(context.Background(), s, Options{}); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Candidates("aaa")

	if _, err := Run(context.Background(), s, Options{}); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Candidates("aaa")

	if len(first) != len(second) {
		t.Fatalf("candidate count changed on re-run: %d -> %d", len(first), len(second))
	}
}

// A manual value must survive a re-interpretation, and a manual training record
// must not be replaced by one derived from a header.
func TestInterpretationNeverOverwritesManual(t *testing.T) {
	s := runStore(t)
	meta := map[string]string{"ss_network_module": "networks.lora", "ss_output_name": "from_header"}
	seedModel(t, s, "aaa", "/models/loras/thing.safetensors", "safetensors",
		header(t, []string{"lora_unet_x.lora_down.weight"}, meta))

	if err := s.RecordObservations("aaa", provenance.SourceManual,
		[]store.FieldObservation{{Field: provenance.FieldName, Value: "my careful name"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrainingRecord(store.TrainingRecord{
		SHA256: "aaa", Source: "manual", Notes: "what worked and what did not",
		Dataset: "curated-v3",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), s, Options{}); err != nil {
		t.Fatal(err)
	}

	rec, _ := s.GetModelRecord("aaa")
	if rec.Name != "my careful name" {
		t.Fatalf("Name = %q; interpretation overwrote a manual value", rec.Name)
	}
	tr, _ := s.GetTrainingRecord("aaa")
	if tr.Notes != "what worked and what did not" || tr.Dataset != "curated-v3" {
		t.Fatalf("a manual training record was overwritten by the header pass: %+v", tr)
	}
}

func TestRunOnEmptyDatabase(t *testing.T) {
	s := runStore(t)
	stats, err := Run(context.Background(), s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Models != 0 || stats.Interpreted != 0 {
		t.Fatalf("unexpected stats on an empty database: %+v", stats)
	}
}

func TestGGUFInterpretation(t *testing.T) {
	// Build a GGUF header the same way the format package's own fixtures do,
	// through the public metadata reader rather than by trusting a hand-rolled
	// blob to be right.
	blob := buildGGUFBlob(t, map[string]any{
		"general.architecture":    "llama",
		"general.name":            "My Fine-Tune",
		"general.parameter_count": uint64(8_030_000_000),
	})

	s := runStore(t)
	seedModel(t, s, "gg", "/models/llm/model.gguf", "gguf", blob)

	if _, err := Run(context.Background(), s, Options{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetModelRecord("gg")
	if rec == nil {
		t.Fatal("GGUF model was not interpreted")
	}
	if rec.Name != "My Fine-Tune" {
		t.Errorf("Name = %q", rec.Name)
	}
	if rec.BaseModel != "Llama 8B" {
		t.Errorf("BaseModel = %q, want the architecture with its parameter count", rec.BaseModel)
	}
	if rec.Type != "checkpoint" {
		t.Errorf("Type = %q, want checkpoint", rec.Type)
	}
}

// buildGGUFBlob writes a minimal valid GGUF header containing the given keys.
func buildGGUFBlob(t *testing.T, kv map[string]any) []byte {
	t.Helper()
	var b []byte
	u32 := func(v uint32) { b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	u64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			b = append(b, byte(v>>(8*i)))
		}
	}
	str := func(s string) { u64(uint64(len(s))); b = append(b, s...) }

	u32(0x46554747) // "GGUF"
	u32(3)          // version
	u64(0)          // tensor count
	u64(uint64(len(kv)))

	// Deterministic order so the fixture is stable.
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sortStrings(keys)

	for _, k := range keys {
		str(k)
		switch v := kv[k].(type) {
		case string:
			u32(8) // STRING
			str(v)
		case uint64:
			u32(10) // UINT64
			u64(v)
		default:
			t.Fatalf("unsupported fixture value type %T", v)
		}
	}
	return b
}
