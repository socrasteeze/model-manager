package ingest

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func find(parsed []Parsed, source string) *Parsed {
	for i := range parsed {
		if parsed[i].Source == source {
			return &parsed[i]
		}
	}
	return nil
}

func value(p *Parsed, field string) any {
	if p == nil {
		return nil
	}
	for _, o := range p.Observations {
		if o.Field == field {
			return o.Value
		}
	}
	return nil
}

func TestStabilityMatrixSidecar(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "model.safetensors", "not really a model")
	write(t, dir, "model.cm-info.json", `{
        "ModelName": "Cinematic Style",
        "ModelDescription": "Adds drama",
        "VersionName": "v2.0",
        "BaseModel": "SDXL 1.0",
        "ModelType": "Lora",
        "TrainedWords": ["cinelight", "dramatic", "cinelight"],
        "IsSfw": true,
        "Tags": ["style", "lighting"]
    }`)

	p := find(Discover(filepath.Join(dir, "model.safetensors")), provenance.SourceStabilityMatrix)
	if p == nil {
		t.Fatal("stability matrix sidecar was not discovered")
	}
	if got := value(p, provenance.FieldName); got != "Cinematic Style" {
		t.Errorf("name = %v", got)
	}
	if got := value(p, provenance.FieldBaseModel); got != "SDXL" {
		t.Errorf("base_model = %v, want the normalized SDXL", got)
	}
	if got := value(p, provenance.FieldType); got != "lora" {
		t.Errorf("type = %v", got)
	}
	// IsSfw is the inverse of the flag this app stores.
	if got := value(p, provenance.FieldNSFW); got != false {
		t.Errorf("nsfw = %v, want false from IsSfw: true", got)
	}
	triggers, _ := value(p, provenance.FieldTriggerWords).([]string)
	if len(triggers) != 2 {
		t.Errorf("triggers = %v, want duplicates removed", triggers)
	}
	if len(p.Tags) != 2 {
		t.Errorf("tags = %v", p.Tags)
	}
}

func TestCivitaiInfoSidecar(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.civitai.info", `{
        "name": "v1.5",
        "baseModel": "Pony",
        "description": "<p>A <b>styled</b> LoRA</p>",
        "trainedWords": ["ponystyle"],
        "model": {"name": "Pony Style", "type": "LORA", "nsfw": true}
    }`)

	p := find(Discover(filepath.Join(dir, "m.safetensors")), provenance.SourceA1111)
	if p == nil {
		t.Fatal("civitai.info was not discovered")
	}
	if got := value(p, provenance.FieldName); got != "Pony Style" {
		t.Errorf("name = %v", got)
	}
	if got := value(p, provenance.FieldVersion); got != "v1.5" {
		t.Errorf("version = %v", got)
	}
	if got := value(p, provenance.FieldBaseModel); got != "Pony" {
		t.Errorf("base_model = %v", got)
	}
	if got := value(p, provenance.FieldNSFW); got != true {
		t.Errorf("nsfw = %v", got)
	}
	// Markup must be stripped: rendering it literally is ugly, and injecting it
	// into the UI is worse.
	if got := value(p, provenance.FieldDescription); got != "A styled LoRA" {
		t.Errorf("description = %q, want the markup stripped", got)
	}
}

func TestLoRAManagerSidecar(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.metadata.json", `{
        "model_name": "My LoRA",
        "file_name": "m",
        "base_model": "Flux.1 D",
        "tags": ["character"],
        "notes": "works best at low weight",
        "usage_tips": "{\"strength\": 0.65}",
        "civitai": {
            "name": "v3",
            "trainedWords": ["mychar"],
            "model": {"type": "LORA", "nsfw": false}
        }
    }`)

	p := find(Discover(filepath.Join(dir, "m.safetensors")), provenance.SourceComfyUILoRAMgr)
	if p == nil {
		t.Fatal("lora manager sidecar was not discovered")
	}
	if got := value(p, provenance.FieldBaseModel); got != "Flux" {
		t.Errorf("base_model = %v", got)
	}
	if got := value(p, provenance.FieldRecommendedWeight); got != 0.65 {
		t.Errorf("recommended_weight = %v, want 0.65 out of the nested usage_tips JSON", got)
	}
	if got := value(p, provenance.FieldDescription); got != "works best at low weight" {
		t.Errorf("description = %v", got)
	}
}

// SwarmUI and A1111 both write `<stem>.json`. Attributing one tool's data to the
// other corrupts the same-tier trust ordering §7.1 depends on.
func TestAmbiguousJSONIsDispatchedByContent(t *testing.T) {
	t.Run("swarmui", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "m.safetensors", "x")
		write(t, dir, "m.json", `{
            "model_name": "Swarm Model",
            "title": "Swarm Title",
            "description": "from swarm",
            "trigger_phrase": "swarmtrigger, second",
            "architecture": "stable-diffusion-xl-v1-base",
            "standard_width": 1024,
            "standard_height": 1024
        }`)

		all := Discover(filepath.Join(dir, "m.safetensors"))
		p := find(all, provenance.SourceSwarmUI)
		if p == nil {
			t.Fatalf("not attributed to SwarmUI: %+v", all)
		}
		if got := value(p, provenance.FieldName); got != "Swarm Title" {
			t.Errorf("name = %v", got)
		}
		if got := value(p, provenance.FieldBaseModel); got != "SDXL" {
			t.Errorf("base_model = %v", got)
		}
		triggers, _ := value(p, provenance.FieldTriggerWords).([]string)
		if len(triggers) != 2 {
			t.Errorf("triggers = %v", triggers)
		}
	})

	t.Run("a1111", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "m.safetensors", "x")
		write(t, dir, "m.json", `{
            "description": "my notes",
            "sd version": "SDXL",
            "activation text": "trigger one, trigger two",
            "preferred weight": 0.8,
            "notes": "extra"
        }`)

		all := Discover(filepath.Join(dir, "m.safetensors"))
		p := find(all, provenance.SourceA1111)
		if p == nil {
			t.Fatalf("not attributed to A1111: %+v", all)
		}
		if got := value(p, provenance.FieldRecommendedWeight); got != 0.8 {
			t.Errorf("recommended_weight = %v", got)
		}
		if got := value(p, provenance.FieldBaseModel); got != "SDXL" {
			t.Errorf("base_model = %v", got)
		}
	})

	// A JSON file matching neither shape is skipped rather than guessed at.
	t.Run("unrelated json", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "m.safetensors", "x")
		write(t, dir, "m.json", `{"some": "unrelated config", "port": 8080}`)

		for _, p := range Discover(filepath.Join(dir, "m.safetensors")) {
			if p.Source == provenance.SourceSwarmUI || p.Source == provenance.SourceA1111 {
				t.Fatalf("unrelated JSON was attributed to %s", p.Source)
			}
		}
	})
}

func TestSwarmEmbeddedPreviewIsDecoded(t *testing.T) {
	// A 1x1 PNG.
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	}
	encoded := base64.StdEncoding.EncodeToString(png)

	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.json", `{
        "model_name": "Swarm",
        "architecture": "stable-diffusion-xl-v1-base",
        "preview_image": "data:image/png;base64,`+encoded+`"
    }`)

	p := find(Discover(filepath.Join(dir, "m.safetensors")), provenance.SourceSwarmUI)
	if p == nil || len(p.PreviewData) != 1 {
		t.Fatalf("embedded preview was not decoded: %+v", p)
	}
	if len(p.PreviewData[0]) != len(png) {
		t.Fatalf("decoded %d bytes, want %d", len(p.PreviewData[0]), len(png))
	}
}

func TestAdjacentPreviewFileIsFound(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.preview.png", "fake png bytes")
	write(t, dir, "m.png", "another one")

	all := Discover(filepath.Join(dir, "m.safetensors"))
	var paths []string
	for _, p := range all {
		paths = append(paths, p.PreviewPaths...)
	}
	if len(paths) != 1 {
		t.Fatalf("found %d preview files, want 1 (the preferred suffix only): %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "m.preview.png" {
		t.Fatalf("chose %s, want the .preview.png", paths[0])
	}
}

func TestNoSidecarsIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "lonely.safetensors", "x")
	if got := Discover(filepath.Join(dir, "lonely.safetensors")); len(got) != 0 {
		t.Fatalf("discovered %d sidecars for a model with none", len(got))
	}
}

func TestMalformedSidecarIsSkipped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.cm-info.json", `{not valid json`)
	write(t, dir, "m.civitai.info", ``)

	if got := Discover(filepath.Join(dir, "m.safetensors")); len(got) != 0 {
		t.Fatalf("malformed sidecars produced %d results", len(got))
	}
}

// A JSON document that parses but carries none of the expected keys belongs to
// something else that happens to share the filename.
func TestWellFormedButUnrelatedSidecarIsSkipped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.safetensors", "x")
	write(t, dir, "m.cm-info.json", `{"unrelated": true}`)

	if p := find(Discover(filepath.Join(dir, "m.safetensors")), provenance.SourceStabilityMatrix); p != nil {
		t.Fatal("an unrelated JSON file was parsed as a Stability Matrix sidecar")
	}
}

func TestNormalizeBaseModel(t *testing.T) {
	cases := map[string]string{
		"SDXL 1.0": "SDXL", "SD XL": "SDXL", "sdxl": "SDXL",
		"Pony Diffusion V6 XL": "Pony", // pony is checked before sdxl
		"Illustrious XL":       "Illustrious",
		"Flux.1 D":             "Flux",
		"SD 1.5":               "SD 1.5",
		"SD 2.1":               "SD 2.x",
		"":                     "",
		// An unrecognized architecture is kept verbatim rather than dropped: the
		// set is open-ended and a new one appearing must not vanish.
		"Some New Arch 2027": "Some New Arch 2027",
	}
	for in, want := range cases {
		if got := normalizeBaseModel(in); got != want {
			t.Errorf("normalizeBaseModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unrecognized type is dropped so the type facet stays a closed set a filter
// can rely on.
func TestNormalizeType(t *testing.T) {
	cases := map[string]string{
		"LORA": "lora", "Lora": "lora", "LoCon": "lycoris", "DoRA": "lycoris",
		"Checkpoint": "checkpoint", "TextualInversion": "embedding",
		"VAE": "vae", "Controlnet": "controlnet",
		"SomethingElse": "", "": "",
	}
	for in, want := range cases {
		if got := normalizeType(in); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	cases := map[string]string{
		"plain text":                    "plain text",
		"<p>Hello <b>world</b></p>":     "Hello world",
		"a &amp; b":                     "a & b",
		"<script>alert(1)</script>safe": "alert(1)safe",
		"line\n\nbreaks   collapse":     "line breaks collapse",
	}
	for in, want := range cases {
		if got := stripHTML(in); got != want {
			t.Errorf("stripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanStrings(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	got := cleanStrings([]string{" a ", "a", "", "  ", "b", string(long)})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("cleanStrings = %v, want [a b] with the paragraph dropped", got)
	}
}
