package interpret

import (
	"encoding/json"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

func header(t *testing.T, tensors []string, meta map[string]string) []byte {
	t.Helper()
	obj := map[string]any{}
	for _, name := range tensors {
		obj[name] = map[string]any{"dtype": "F16", "shape": []int{8, 8}, "data_offsets": []int{0, 128}}
	}
	if meta != nil {
		obj["__metadata__"] = meta
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fieldOf(res Result, field string) any {
	for _, o := range res.Observations {
		if o.Field == field {
			return o.Value
		}
	}
	return nil
}

// Tensor names are the model. A sidecar can be missing or wrong; the namespace
// cannot.
func TestTypeInferenceFromTensorNames(t *testing.T) {
	cases := []struct {
		name    string
		tensors []string
		want    string
	}{
		{"kohya lora", []string{
			"lora_unet_down_blocks_0_attn.lora_down.weight",
			"lora_unet_down_blocks_0_attn.lora_up.weight",
		}, "lora"},
		{"peft lora", []string{
			"transformer.blocks.0.attn.lora_A.weight",
			"transformer.blocks.0.attn.lora_B.weight",
		}, "lora"},
		{"lycoris loha", []string{
			"lora_unet_blk.hada_w1_a", "lora_unet_blk.hada_w2_b",
		}, "lycoris"},
		{"lycoris lokr", []string{"lora_unet_blk.lokr_w1", "lora_unet_blk.lokr_w2"}, "lycoris"},
		{"controlnet", []string{
			"control_model.input_blocks.0.weight", "input_hint_block.0.weight",
		}, "controlnet"},
		{"checkpoint", []string{
			"model.diffusion_model.input_blocks.0.0.weight",
			"model.diffusion_model.middle_block.1.weight",
		}, "checkpoint"},
		{"flux", []string{
			"double_blocks.0.img_attn.qkv.weight", "single_blocks.0.linear1.weight",
		}, "checkpoint"},
		{"vae", []string{
			"encoder.conv_in.weight", "decoder.conv_out.weight", "quant_conv.weight",
		}, "vae"},
		{"embedding", []string{"emb_params"}, "embedding"},
		{"unrecognizable", []string{"some.random.tensor"}, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Safetensors(header(t, c.tensors, nil), false)
			got, _ := fieldOf(res, provenance.FieldType).(string)
			if got != c.want {
				t.Fatalf("type = %q, want %q", got, c.want)
			}
		})
	}
}

// A LoRA targeting a diffusion UNet has "unet" all over its tensor names. The
// adapter signal has to be checked before the checkpoint one or every LoRA is
// misfiled as a checkpoint.
func TestLoRATargetingUNetIsNotACheckpoint(t *testing.T) {
	res := Safetensors(header(t, []string{
		"lora_unet_down_blocks_0.lora_down.weight",
		"lora_unet_down_blocks_0.lora_up.weight",
		"model.diffusion_model.something",
	}, nil), false)
	if got, _ := fieldOf(res, provenance.FieldType).(string); got != "lora" {
		t.Fatalf("type = %q, want lora", got)
	}
}

func TestBaseModelInferenceFromTensorNames(t *testing.T) {
	cases := []struct {
		name    string
		tensors []string
		want    string
	}{
		{"sdxl via second text encoder", []string{"lora_te2_text_model.lora_down.weight"}, "SDXL"},
		{"flux", []string{"double_blocks.0.weight"}, "Flux"},
		{"sd3", []string{"joint_blocks.0.weight"}, "SD 3"},
		{"sd15 has nothing distinctive", []string{"lora_unet_mid.lora_down.weight"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Safetensors(header(t, c.tensors, nil), false)
			got, _ := fieldOf(res, provenance.FieldBaseModel).(string)
			if got != c.want {
				t.Fatalf("base_model = %q, want %q", got, c.want)
			}
		})
	}
}

// Wan is identified by two tensor families coexisting. Requiring both markers in
// a single name would never match anything.
func TestWanNeedsBothMarkersAcrossTheNamespace(t *testing.T) {
	res := Safetensors(header(t, []string{
		"patch_embedding.weight",
		"blocks.0.self_attn.q.weight",
	}, nil), false)
	if got, _ := fieldOf(res, provenance.FieldBaseModel).(string); got != "Wan" {
		t.Fatalf("base_model = %q, want Wan", got)
	}

	// One marker alone is not enough.
	res = Safetensors(header(t, []string{"patch_embedding.weight"}, nil), false)
	if got, _ := fieldOf(res, provenance.FieldBaseModel).(string); got == "Wan" {
		t.Fatal("a single marker was enough to claim Wan")
	}
}

// The training record for a self-trained LoRA comes out of the header for free,
// which is the whole reason §8 is tractable at all.
func TestKohyaMetadataYieldsATrainingRecord(t *testing.T) {
	meta := map[string]string{
		"ss_network_module":        "networks.lora",
		"ss_network_dim":           "32",
		"ss_network_alpha":         "16",
		"ss_learning_rate":         "0.0001",
		"ss_optimizer":             "AdamW8bit",
		"ss_lr_scheduler":          "cosine",
		"ss_max_train_steps":       "2400",
		"ss_num_epochs":            "10",
		"ss_batch_size_per_device": "2",
		"ss_num_train_images":      "120",
		"ss_base_model_version":    "sdxl_base_v1-0",
		"ss_output_name":           "my_style_lora",
		"ss_training_finished_at":  "1721000000",
	}
	res := Safetensors(header(t, []string{"lora_unet_x.lora_down.weight"}, meta), false)

	if res.Training == nil {
		t.Fatal("no training record extracted from a kohya header")
	}
	tr := res.Training
	if tr.Trainer != "kohya-ss / sd-scripts" {
		t.Errorf("Trainer = %q", tr.Trainer)
	}
	if tr.Base != "SDXL" {
		t.Errorf("Base = %q, want SDXL", tr.Base)
	}
	if tr.DatasetSize != 120 {
		t.Errorf("DatasetSize = %d, want 120", tr.DatasetSize)
	}
	if tr.RunDate == "" {
		t.Error("RunDate was not derived from the training timestamp")
	}
	// Numeric config values must arrive as numbers, not as a wall of strings.
	if rank, ok := tr.Config["rank"].(int64); !ok || rank != 32 {
		t.Errorf("config rank = %v (%T), want int64 32", tr.Config["rank"], tr.Config["rank"])
	}
	if lr, ok := tr.Config["learning_rate"].(float64); !ok || lr != 0.0001 {
		t.Errorf("config learning_rate = %v (%T)", tr.Config["learning_rate"], tr.Config["learning_rate"])
	}
	if opt, ok := tr.Config["optimizer"].(string); !ok || opt != "AdamW8bit" {
		t.Errorf("config optimizer = %v", tr.Config["optimizer"])
	}

	if got, _ := fieldOf(res, provenance.FieldName).(string); got != "my_style_lora" {
		t.Errorf("name = %q", got)
	}
	if got, _ := fieldOf(res, provenance.FieldBaseModel).(string); got != "SDXL" {
		t.Errorf("base_model = %q", got)
	}
	// A LoRA from this toolchain is self-trained until something better says so.
	if got, _ := fieldOf(res, provenance.FieldOrigin).(string); got != "self-trained" {
		t.Errorf("origin = %q, want self-trained", got)
	}
}

// A tag on essentially every training image is what the model was taught to
// respond to. A tag on half of them is a description.
func TestTriggerWordsFromTagFrequency(t *testing.T) {
	freq := map[string]map[string]int{
		"dataset1": {
			"mystyle":   100, // on everything: a trigger
			"1girl":     95,  // near-universal: also a trigger by this rule
			"blue hair": 40,  // a description
			"outdoors":  12,
		},
	}
	raw, _ := json.Marshal(freq)
	meta := map[string]string{
		"ss_network_module": "networks.lora",
		"ss_tag_frequency":  string(raw),
	}
	res := Safetensors(header(t, []string{"lora_unet_x.lora_down.weight"}, meta), false)

	triggers, _ := fieldOf(res, provenance.FieldTriggerWords).([]string)
	if len(triggers) != 2 {
		t.Fatalf("triggers = %v, want the two near-universal tags", triggers)
	}
	for _, tr := range triggers {
		if tr == "blue hair" || tr == "outdoors" {
			t.Fatalf("a merely-common tag was promoted to a trigger: %v", triggers)
		}
	}
}

// If the threshold catches a whole generic vocabulary, it found a captioning
// style rather than a trigger, and emitting all of it would be noise.
func TestTagFrequencyBailsOnTooManyCandidates(t *testing.T) {
	tags := map[string]int{}
	for _, tag := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		tags[tag] = 100
	}
	raw, _ := json.Marshal(map[string]map[string]int{"d": tags})
	res := Safetensors(header(t, []string{"lora_unet_x.lora_down.weight"},
		map[string]string{"ss_network_module": "networks.lora", "ss_tag_frequency": string(raw)}), false)

	if triggers := fieldOf(res, provenance.FieldTriggerWords); triggers != nil {
		t.Fatalf("emitted %v as triggers; a ten-tag vocabulary is not a trigger set", triggers)
	}
}

func TestModelSpecKeys(t *testing.T) {
	meta := map[string]string{
		"modelspec.title":          "Cinematic Lighting",
		"modelspec.description":    "Adds dramatic lighting",
		"modelspec.architecture":   "stable-diffusion-xl-v1-base/lora",
		"modelspec.trigger_phrase": "cinelight, dramatic",
	}
	res := Safetensors(header(t, []string{"some.tensor"}, meta), false)

	if got, _ := fieldOf(res, provenance.FieldName).(string); got != "Cinematic Lighting" {
		t.Errorf("name = %q", got)
	}
	if got, _ := fieldOf(res, provenance.FieldBaseModel).(string); got != "SDXL" {
		t.Errorf("base_model = %q", got)
	}
	if got, _ := fieldOf(res, provenance.FieldType).(string); got != "lora" {
		t.Errorf("type = %q, want lora from the /lora architecture suffix", got)
	}
	triggers, _ := fieldOf(res, provenance.FieldTriggerWords).([]string)
	if len(triggers) != 2 || triggers[0] != "cinelight" {
		t.Errorf("triggers = %v", triggers)
	}
}

// A merged checkpoint with no metadata block at all is the common case, and must
// still be classified from its tensors.
func TestHeaderWithoutMetadataBlock(t *testing.T) {
	res := Safetensors(header(t, []string{"model.diffusion_model.input_blocks.0.weight"}, nil), false)
	if got, _ := fieldOf(res, provenance.FieldType).(string); got != "checkpoint" {
		t.Fatalf("type = %q, want checkpoint", got)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

// A truncated blob is expected, not a defect, and must be reported as such so a
// missing interpretation is diagnosable.
func TestTruncatedHeaderWarnsRatherThanFailing(t *testing.T) {
	full := header(t, []string{"model.diffusion_model.x"}, map[string]string{"a": "b"})
	res := Safetensors(full[:len(full)/2], true)
	if len(res.Warnings) == 0 {
		t.Fatal("a truncated blob produced no warning")
	}
	if len(res.Observations) != 0 {
		t.Fatal("observations were produced from an unparseable blob")
	}
}

func TestEmptyAndGarbageHeaders(t *testing.T) {
	if res := Safetensors(nil, false); len(res.Observations) != 0 || len(res.Warnings) != 0 {
		t.Error("an empty blob produced output")
	}
	res := Safetensors([]byte("not json at all"), false)
	if len(res.Warnings) == 0 {
		t.Error("garbage produced no warning")
	}
}

func TestPathHeuristics(t *testing.T) {
	cases := []struct {
		path     string
		wantType string
		wantBase string
		wantName string
	}{
		{"/models/loras/SDXL/cinematic_style_v2.safetensors", "lora", "SDXL", "cinematic style v2"},
		{"/models/checkpoints/juggernaut-xl.safetensors", "checkpoint", "SDXL", "juggernaut xl"},
		{"/models/vae/sdxl_vae.safetensors", "vae", "SDXL", "sdxl vae"},
		{"/models/embeddings/badhands.pt", "embedding", "", "badhands"},
		{"/models/controlnet/canny.safetensors", "controlnet", "", "canny"},
		{"/models/loras/Flux/mystyle.safetensors", "lora", "Flux", "mystyle"},
		{"/random/place/thing.safetensors", "", "", "thing"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			res := FromPath(c.path)
			gotType, _ := fieldOf(res, provenance.FieldType).(string)
			gotBase, _ := fieldOf(res, provenance.FieldBaseModel).(string)
			gotName, _ := fieldOf(res, provenance.FieldName).(string)
			if gotType != c.wantType {
				t.Errorf("type = %q, want %q", gotType, c.wantType)
			}
			if gotBase != c.wantBase {
				t.Errorf("base_model = %q, want %q", gotBase, c.wantBase)
			}
			if gotName != c.wantName {
				t.Errorf("name = %q, want %q", gotName, c.wantName)
			}
		})
	}
}

// A directory called `my-loras-backup` is not the word `loras`. Matching on
// substrings rather than whole segments would misfile a lot of libraries.
func TestPathTypeMatchesWholeSegmentsOnly(t *testing.T) {
	res := FromPath("/models/my-loras-backup/thing.safetensors")
	if got := fieldOf(res, provenance.FieldType); got != nil {
		t.Fatalf("type = %v from a partial segment match", got)
	}
}

// A bare hash looks like real metadata when shown as a name, which is worse than
// showing nothing.
func TestHashLikeFilenameYieldsNoName(t *testing.T) {
	res := FromPath("/models/loras/a3f5c9d2e1b7480f9c2a1d5e8b3f7a91.safetensors")
	if got := fieldOf(res, provenance.FieldName); got != nil {
		t.Fatalf("name = %v, want none for a hash-like filename", got)
	}
}

func TestVersionFromFilename(t *testing.T) {
	cases := map[string]string{
		"/m/style_v2.safetensors":        "v2",
		"/m/style-v1.5.safetensors":      "v1.5",
		"/m/style 3.safetensors":         "3",
		"/m/style.safetensors":           "",
		"/m/no-version-here.safetensors": "",
	}
	for path, want := range cases {
		if got := VersionFromFilename(path); got != want {
			t.Errorf("VersionFromFilename(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestResultAddSkipsEmptyValues(t *testing.T) {
	var res Result
	res.add("f", "")
	res.add("f", "   ")
	res.add("f", nil)
	if len(res.Observations) != 0 {
		t.Fatalf("empty values were recorded: %+v", res.Observations)
	}
	res.add("f", "real")
	if len(res.Observations) != 1 {
		t.Fatal("a real value was skipped")
	}
}
