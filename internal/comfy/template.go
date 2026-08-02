package comfy

// Filling a saved workflow in for a particular model.
//
// The workflow is stored once as a template and reused for every model, so the
// model's own filename, its trigger words, and a fresh seed have to get into
// the graph somehow. Substitution happens on the JSON *text* before it is
// parsed, which is what lets a placeholder sit anywhere -- inside a prompt
// string, as a whole field, in a node nobody anticipated -- without this
// package needing to know the graph's shape.
//
// Doing text substitution on JSON is exactly the kind of thing that produces
// injection bugs, so every value is JSON-escaped before it goes in. A trigger
// word containing a quote is then a trigger word containing a quote, not a
// broken graph or an attacker-chosen node parameter.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// Vars are the values a template can reference.
type Vars struct {
	// Model is the model's filename as ComfyUI would load it, e.g.
	// "my-style.safetensors". This is the one that matters: a thumbnail for a
	// lora is only meaningful if the graph loads that lora.
	Model string

	// Name, BaseModel and TriggerWords come from the library record.
	Name         string
	BaseModel    string
	TriggerWords []string

	// Checkpoint is the base model the preview is rendered on. A lora cannot
	// render anything by itself, so a workflow that previews one has to name a
	// checkpoint the user actually has -- there is no sane default this app can
	// guess, which is why it is configured rather than inferred.
	Checkpoint string

	// Prompt and Negative are the caller's text, if any.
	Prompt   string
	Negative string

	// Seed is a number the template can drop into a sampler.
	Seed int64
}

var placeholder = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

// Placeholders lists what a template may use, for the settings UI to show.
var Placeholders = []string{
	"model", "checkpoint", "name", "base_model", "triggers", "prompt", "negative", "seed",
}

// Fill substitutes placeholders in a workflow template.
//
// An unknown placeholder is left alone rather than blanked: a graph that
// happens to contain `{{something}}` in a text field is more likely to be
// someone's literal text than a typo this package should silently eat.
func Fill(template json.RawMessage, v Vars) (json.RawMessage, error) {
	text := string(template)

	replaced := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		key := strings.Trim(strings.Trim(match, "{}"), " ")
		switch key {
		case "model":
			return jsonInner(v.Model)
		case "checkpoint":
			return jsonInner(v.Checkpoint)
		case "name":
			return jsonInner(v.Name)
		case "base_model":
			return jsonInner(v.BaseModel)
		case "triggers":
			return jsonInner(strings.Join(v.TriggerWords, ", "))
		case "prompt":
			return jsonInner(firstNonEmpty(v.Prompt, strings.Join(v.TriggerWords, ", "), v.Name))
		case "negative":
			return jsonInner(v.Negative)
		case "seed":
			// A bare number, because a seed lands in a numeric field. Quoting
			// it would make ComfyUI reject the node.
			return fmt.Sprintf("%d", v.Seed)
		}
		return match
	})

	var out json.RawMessage
	if err := json.Unmarshal([]byte(replaced), &out); err != nil {
		return nil, fmt.Errorf(
			"comfy: the filled workflow is not valid JSON (%w). A placeholder that "+
				"sits outside a string, or a template that was already malformed, will "+
				"do this", err)
	}
	return out, nil
}

// jsonInner escapes a value for insertion *inside* an existing JSON string
// literal, which is where placeholders almost always sit -- `"a photo of
// {{name}}"`. json.Marshal's own quotes are stripped for that reason.
func jsonInner(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(encoded[1 : len(encoded)-1])
}

// SeedFor derives a stable seed from a model hash, so re-rendering the same
// model twice gives the same picture unless the caller asks otherwise.
//
// Deterministic on purpose: a thumbnail that changes every time it is
// regenerated makes "did my edit help?" unanswerable.
func SeedFor(sha string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sha))
	// Masked into the positive range ComfyUI's seed widgets accept.
	return int64(h.Sum64() & 0x7fffffffffff)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
