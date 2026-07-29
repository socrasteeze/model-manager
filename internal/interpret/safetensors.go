// Package interpret turns stored format headers into typed field observations.
//
// This is the deferred half of Phase 0's bargain. Headers were captured verbatim
// and uninterpreted so that a schema change would never cost a re-hash of 7.5TB
// (spec §15); interpreting them is a cheap, re-runnable pass over blobs already
// in the database.
//
// Nothing here reads a model file. Everything it needs was captured during the
// hash pass.
package interpret

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Result is what one header yielded.
type Result struct {
	Observations []store.FieldObservation
	Training     *store.TrainingRecord

	// Warnings record things the header said that could not be used, so a
	// surprising interpretation is diagnosable without re-reading the file.
	Warnings []string
}

func (r *Result) add(field string, value any) {
	if value == nil {
		return
	}
	if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
		return
	}
	r.Observations = append(r.Observations, store.FieldObservation{Field: field, Value: value})
}

// Safetensors interprets a stored safetensors JSON header.
func Safetensors(blob []byte, truncated bool) Result {
	var res Result
	if len(blob) == 0 {
		return res
	}

	// Tensor names carry the strongest signal about what a file *is*, so they
	// are read even when the metadata block is absent -- which is the normal
	// case for merged checkpoints and anything converted by a script.
	tensorNames, meta, err := splitSafetensorsHeader(blob)
	if err != nil {
		if truncated {
			// Expected: the blob was cut at the storage cap, so the JSON is
			// genuinely incomplete rather than malformed.
			res.Warnings = append(res.Warnings, "header blob was truncated at the storage cap; metadata not parsed")
		} else {
			res.Warnings = append(res.Warnings, "header is not valid JSON: "+err.Error())
		}
		return res
	}

	if t := inferTypeFromTensors(tensorNames); t != "" {
		res.add(provenance.FieldType, t)
	}
	if b := inferBaseFromTensors(tensorNames); b != "" {
		res.add(provenance.FieldBaseModel, b)
	}

	if len(meta) == 0 {
		return res
	}
	interpretKohya(meta, &res)
	interpretModelSpec(meta, &res)
	return res
}

// splitSafetensorsHeader pulls out tensor names and the __metadata__ block
// without decoding the tensor descriptors, which are large and of no interest.
func splitSafetensorsHeader(blob []byte) (tensorNames []string, meta map[string]string, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, nil, err
	}

	meta = map[string]string{}
	for key, val := range raw {
		if key == "__metadata__" {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(val, &m); err != nil {
				continue
			}
			for mk, mv := range m {
				// Metadata values are conventionally strings, but not
				// universally -- some writers emit bare numbers.
				var s string
				if err := json.Unmarshal(mv, &s); err == nil {
					meta[mk] = s
				} else {
					meta[mk] = strings.Trim(string(mv), `"`)
				}
			}
			continue
		}
		tensorNames = append(tensorNames, key)
	}
	return tensorNames, meta, nil
}

// inferTypeFromTensors identifies what a file is from the shape of its tensor
// namespace. This is the most reliable signal available offline: a sidecar can
// be wrong or missing, but the tensor names are the model.
func inferTypeFromTensors(names []string) string {
	var (
		hasLoRA, hasLyCORIS, hasDiffusion bool
		hasControlNet, hasEmbedding       bool
		hasFluxBlocks                     bool
		vaeish, total                     int
	)

	for _, n := range names {
		total++
		// Every comparison below is against the lowercased name, so every
		// literal here must be lowercase too.
		l := strings.ToLower(n)
		switch {
		case strings.Contains(l, "lora_down") || strings.Contains(l, "lora_up") ||
			strings.Contains(l, ".lora_a") || strings.Contains(l, ".lora_b") ||
			strings.Contains(l, "lora_magnitude"):
			hasLoRA = true
		case strings.Contains(l, "hada_w1") || strings.Contains(l, "hada_w2") ||
			strings.Contains(l, "lokr_w1") || strings.Contains(l, "lokr_w2") ||
			strings.Contains(l, "oft_blocks") || strings.Contains(l, "boft"):
			hasLyCORIS = true
		case strings.Contains(l, "control_model") || strings.Contains(l, "input_hint_block") ||
			strings.Contains(l, "zero_convs"):
			hasControlNet = true
		case l == "emb_params" || strings.HasPrefix(l, "string_to_param") ||
			strings.HasPrefix(l, "clip_g") && total <= 2:
			hasEmbedding = true
		case strings.Contains(l, "double_blocks.") || strings.Contains(l, "single_blocks."):
			hasFluxBlocks = true
		case strings.Contains(l, "model.diffusion_model") || strings.HasPrefix(l, "diffusion_model.") ||
			strings.Contains(l, "unet."):
			hasDiffusion = true
		}
		if strings.Contains(l, "first_stage_model") || strings.HasPrefix(l, "encoder.") ||
			strings.HasPrefix(l, "decoder.") || strings.Contains(l, "quant_conv") {
			vaeish++
		}
	}

	// Order matters: a LoRA whose target happens to be a diffusion UNet still
	// contains "unet" in its names, so the adapter signals are checked first.
	switch {
	case hasLyCORIS:
		return "lycoris"
	case hasLoRA:
		return "lora"
	case hasControlNet:
		return "controlnet"
	case hasEmbedding && total <= 4:
		return "embedding"
	case total > 0 && vaeish == total:
		// Every tensor belongs to the autoencoder and nothing else does.
		return "vae"
	case hasDiffusion || hasFluxBlocks:
		return "checkpoint"
	}
	return ""
}

// inferBaseFromTensors reads the architecture off the tensor namespace. It is
// coarse by design -- it distinguishes families, not versions, and anything
// finer is left to a source that actually knows.
func inferBaseFromTensors(names []string) string {
	// These are accumulated across the whole namespace, not within one name: an
	// architecture is identified by which families of tensors coexist, and no
	// single tensor name carries two of these markers at once.
	var hasTE2, hasFlux, hasSD3, hasSDXLBlocks bool
	var hasPatchEmbedding, hasSelfAttnBlocks bool

	for _, n := range names {
		l := strings.ToLower(n)
		switch {
		case strings.Contains(l, "lora_te2") || strings.Contains(l, "text_encoder_2") ||
			strings.Contains(l, "conditioner.embedders.1"):
			hasTE2 = true
		case strings.Contains(l, "double_blocks.") || strings.Contains(l, "single_blocks."):
			hasFlux = true
		case strings.Contains(l, "joint_blocks."):
			hasSD3 = true
		case strings.Contains(l, "middle_block.1.transformer_blocks.5"):
			// SDXL's UNet is deeper here than SD 1.x/2.x ever is.
			hasSDXLBlocks = true
		}
		if strings.Contains(l, "patch_embedding") {
			hasPatchEmbedding = true
		}
		if strings.Contains(l, "blocks.0.self_attn") {
			hasSelfAttnBlocks = true
		}
	}

	switch {
	case hasFlux:
		return "Flux"
	case hasSD3:
		return "SD 3"
	case hasPatchEmbedding && hasSelfAttnBlocks:
		return "Wan"
	case hasTE2 || hasSDXLBlocks:
		return "SDXL"
	}
	return ""
}

// interpretKohya reads the ss_* metadata block written by kohya-ss / sd-scripts
// and the trainers derived from it. This is where the training record comes from
// for free (spec §8).
func interpretKohya(meta map[string]string, res *Result) {
	if len(meta) == 0 {
		return
	}
	// Presence of any ss_ key is what identifies the toolchain.
	found := false
	for k := range meta {
		if strings.HasPrefix(k, "ss_") {
			found = true
			break
		}
	}
	if !found {
		return
	}

	if v := meta["ss_output_name"]; v != "" {
		res.add(provenance.FieldName, v)
	}
	if base := mapKohyaBaseModel(meta["ss_base_model_version"]); base != "" {
		res.add(provenance.FieldBaseModel, base)
	}
	if v := meta["ss_network_module"]; v != "" {
		if t := mapNetworkModule(v); t != "" {
			res.add(provenance.FieldType, t)
		}
	}
	// A LoRA trained by this toolchain is, by construction, self-trained unless
	// something better later says otherwise.
	res.add(provenance.FieldOrigin, "self-trained")

	if triggers := triggersFromTagFrequency(meta["ss_tag_frequency"]); len(triggers) > 0 {
		res.add(provenance.FieldTriggerWords, triggers)
	}

	tr := &store.TrainingRecord{Source: provenance.SourceSafetensorsHeader}
	tr.Trainer = detectTrainer(meta)
	tr.Base = firstNonEmpty(mapKohyaBaseModel(meta["ss_base_model_version"]), meta["ss_sd_model_name"])
	tr.Dataset = datasetName(meta)
	tr.DatasetSize = atoiOr(meta["ss_num_train_images"], 0)
	tr.RunDate = kohyaRunDate(meta)

	config := map[string]any{}
	for key, metaKey := range map[string]string{
		"rank":            "ss_network_dim",
		"alpha":           "ss_network_alpha",
		"optimizer":       "ss_optimizer",
		"learning_rate":   "ss_learning_rate",
		"unet_lr":         "ss_unet_lr",
		"text_encoder_lr": "ss_text_encoder_lr",
		"lr_scheduler":    "ss_lr_scheduler",
		"steps":           "ss_max_train_steps",
		"epochs":          "ss_num_epochs",
		"batch_size":      "ss_batch_size_per_device",
		"resolution":      "ss_resolution",
		"mixed_precision": "ss_mixed_precision",
		"clip_skip":       "ss_clip_skip",
		"noise_offset":    "ss_noise_offset",
		"seed":            "ss_seed",
		"network_args":    "ss_network_args",
	} {
		if v := strings.TrimSpace(meta[metaKey]); v != "" && v != "None" {
			config[key] = coerceScalar(v)
		}
	}
	if len(config) > 0 {
		tr.Config = config
	}

	if tr.Trainer != "" || tr.Base != "" || len(tr.Config) > 0 {
		res.Training = tr
	}
}

// detectTrainer names the toolchain from the fingerprints each one leaves.
func detectTrainer(meta map[string]string) string {
	if v := meta["ss_training_comment"]; strings.Contains(strings.ToLower(v), "trainflow") {
		return "Anima TrainFlow"
	}
	if _, ok := meta["ot_config"]; ok {
		return "OneTrainer"
	}
	if v := meta["modelspec.implementation"]; strings.Contains(strings.ToLower(v), "ai-toolkit") {
		return "ai-toolkit"
	}
	if _, ok := meta["ss_network_module"]; ok {
		return "kohya-ss / sd-scripts"
	}
	return ""
}

func mapKohyaBaseModel(v string) string {
	switch {
	case v == "":
		return ""
	case strings.HasPrefix(v, "sdxl_"):
		return "SDXL"
	case strings.HasPrefix(v, "sd3"):
		return "SD 3"
	case strings.HasPrefix(v, "flux"):
		return "Flux"
	case strings.HasPrefix(v, "sd_v1"):
		return "SD 1.5"
	case strings.HasPrefix(v, "sd_v2"):
		return "SD 2.x"
	}
	return v
}

func mapNetworkModule(v string) string {
	l := strings.ToLower(v)
	switch {
	case strings.Contains(l, "lycoris"):
		return "lycoris"
	case strings.Contains(l, "networks.lora") || strings.Contains(l, "network.lora"):
		return "lora"
	}
	return ""
}

// triggersFromTagFrequency mines the dataset's tag histogram for likely trigger
// words: a tag present in essentially every training image is what the model was
// taught to respond to.
//
// This is a heuristic offered at the lowest trust available, and it is worth
// having precisely because self-trained LoRAs have no remote record to consult.
func triggersFromTagFrequency(raw string) []string {
	if raw == "" {
		return nil
	}
	var byDataset map[string]map[string]int
	if err := json.Unmarshal([]byte(raw), &byDataset); err != nil {
		return nil
	}

	var maxCount int
	counts := map[string]int{}
	for _, tags := range byDataset {
		for tag, n := range tags {
			counts[tag] += n
			if counts[tag] > maxCount {
				maxCount = counts[tag]
			}
		}
	}
	if maxCount == 0 {
		return nil
	}

	var out []string
	for tag, n := range counts {
		// Present in at least 90% of the images the most common tag appears in.
		// A tag on every image is an instruction; a tag on half of them is a
		// description.
		if float64(n) >= 0.9*float64(maxCount) {
			t := strings.TrimSpace(tag)
			if t != "" && len(t) < 100 {
				out = append(out, t)
			}
		}
	}
	// More than a handful means the threshold caught a generic vocabulary rather
	// than a trigger, and emitting all of it would be noise.
	if len(out) > 8 {
		return nil
	}
	sortStrings(out)
	return out
}

func datasetName(meta map[string]string) string {
	raw := meta["ss_datasets"]
	if raw == "" {
		return ""
	}
	var datasets []map[string]any
	if err := json.Unmarshal([]byte(raw), &datasets); err != nil {
		return ""
	}
	for _, d := range datasets {
		if subsets, ok := d["subsets"].([]any); ok {
			for _, s := range subsets {
				if sm, ok := s.(map[string]any); ok {
					if dir, ok := sm["image_dir"].(string); ok && dir != "" {
						return dir
					}
				}
			}
		}
	}
	return ""
}

func kohyaRunDate(meta map[string]string) string {
	for _, key := range []string{"ss_training_finished_at", "ss_training_started_at"} {
		if v := meta[key]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
			}
		}
	}
	if v := meta["modelspec.date"]; v != "" {
		return v
	}
	return ""
}

// interpretModelSpec reads the Stability AI model-spec keys, which are the
// closest thing safetensors has to a cross-tool convention.
func interpretModelSpec(meta map[string]string, res *Result) {
	if v := meta["modelspec.title"]; v != "" {
		res.add(provenance.FieldName, v)
	}
	if v := meta["modelspec.description"]; v != "" {
		res.add(provenance.FieldDescription, v)
	}
	if v := meta["modelspec.trigger_phrase"]; v != "" {
		res.add(provenance.FieldTriggerWords, splitTriggers(v))
	}
	if arch := meta["modelspec.architecture"]; arch != "" {
		if base := mapModelSpecArchitecture(arch); base != "" {
			res.add(provenance.FieldBaseModel, base)
		}
		if strings.Contains(strings.ToLower(arch), "/lora") {
			res.add(provenance.FieldType, "lora")
		}
	}
}

func mapModelSpecArchitecture(arch string) string {
	l := strings.ToLower(arch)
	switch {
	case strings.Contains(l, "stable-diffusion-xl"):
		return "SDXL"
	case strings.Contains(l, "stable-diffusion-v3") || strings.Contains(l, "sd3"):
		return "SD 3"
	case strings.Contains(l, "flux"):
		return "Flux"
	case strings.Contains(l, "stable-cascade"):
		return "Stable Cascade"
	case strings.Contains(l, "stable-diffusion-v1"):
		return "SD 1.5"
	case strings.Contains(l, "stable-diffusion-v2"):
		return "SD 2.x"
	}
	return ""
}

func splitTriggers(v string) []string {
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// coerceScalar turns a metadata string into a number or bool where it plainly is
// one, so a training config reads as data rather than as a wall of strings.
func coerceScalar(v string) any {
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	switch v {
	case "True", "true":
		return true
	case "False", "false":
		return false
	}
	return v
}

func atoiOr(v string, fallback int) int {
	if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return i
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
