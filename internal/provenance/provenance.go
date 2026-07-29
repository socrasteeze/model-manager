// Package provenance decides which of several competing values for a field wins.
//
// This is the part that prevents re-inventing the bug the whole project exists
// to fix (spec §7). Every field stores value, source and timestamp; nothing is
// ever overwritten in place; a resolver picks a winner from the accumulated
// candidates. An ingest that disagrees with what is already there loses or
// raises a suggestion — it never silently wins.
package provenance

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Tier is the precedence class of a source. Higher wins.
type Tier int

const (
	// TierTool is a scrape from another tool's sidecar. Hints only, lowest trust.
	TierTool Tier = 1

	// TierOrigin is Civitai or HuggingFace by hash. Authoritative for anything
	// the user has not touched.
	TierOrigin Tier = 2

	// TierManual is entered by the user. Sticky. Never overwritten by any
	// ingest, ever.
	//
	// This matters more here than in most tools: self-trained LoRAs have no
	// remote record at all, so manual is the *only* source for a real slice of
	// the library and has to be untouchable.
	TierManual Tier = 3
)

// Source names, stored verbatim in field_value.source.
const (
	SourceManual            = "manual"
	SourceCivitai           = "civitai"
	SourceHuggingFace       = "huggingface"
	SourceSafetensorsHeader = "safetensors_header"
	SourceGGUFHeader        = "gguf_header"
	SourceComfyUILoRAMgr    = "comfyui_lora_manager"
	SourceStabilityMatrix   = "stability_matrix"
	SourceSwarmUI           = "swarmui"
	SourceA1111             = "a1111"
	SourcePathHeuristic     = "path_heuristic"
)

// tiers maps each source to its precedence class.
var tiers = map[string]Tier{
	SourceManual:            TierManual,
	SourceCivitai:           TierOrigin,
	SourceHuggingFace:       TierOrigin,
	SourceSafetensorsHeader: TierTool,
	SourceGGUFHeader:        TierTool,
	SourceComfyUILoRAMgr:    TierTool,
	SourceStabilityMatrix:   TierTool,
	SourceSwarmUI:           TierTool,
	SourceA1111:             TierTool,
	SourcePathHeuristic:     TierTool,
}

// trust breaks ties *within* a tier, before falling back to recency (spec §7.1).
// Higher is more trusted.
//
// The ordering inside TierTool is a judgement call worth stating plainly. A
// safetensors or GGUF header is not really a "tool scrape" — it is data the
// producing trainer embedded in the file itself, and it travels with the bytes
// rather than sitting beside them in a sidecar some other tool may have
// rewritten. It therefore outranks every third-party scrape here, while still
// sitting below Origin, which can see the whole published record.
//
// A path heuristic ("it was in a folder called SDXL") ranks last: it is a guess
// about a filesystem layout the user may not have intended as a statement.
var trust = map[string]int{
	SourceManual:            100,
	SourceCivitai:           90,
	SourceHuggingFace:       80,
	SourceSafetensorsHeader: 60,
	SourceGGUFHeader:        60,
	SourceComfyUILoRAMgr:    40,
	SourceStabilityMatrix:   30,
	SourceSwarmUI:           25,
	SourceA1111:             20,
	SourcePathHeuristic:     5,
}

// TierOf reports a source's precedence class. Unknown sources land in TierTool,
// which is the safe default: a source nobody has classified must not be able to
// outrank a value the user typed.
func TierOf(source string) Tier {
	if t, ok := tiers[source]; ok {
		return t
	}
	return TierTool
}

// TrustOf reports a source's within-tier ranking.
func TrustOf(source string) int {
	if v, ok := trust[source]; ok {
		return v
	}
	return 1
}

// Canonical field names.
const (
	FieldType                = "type"
	FieldBaseModel           = "base_model"
	FieldName                = "name"
	FieldVersion             = "version"
	FieldDescription         = "description"
	FieldTriggerWords        = "trigger_words"
	FieldRecommendedWeight   = "recommended_weight"
	FieldRecommendedSettings = "recommended_settings"
	FieldNSFW                = "nsfw"
	FieldOrigin              = "origin"
)

// Fields is every field the resolver materializes.
var Fields = []string{
	FieldType, FieldBaseModel, FieldName, FieldVersion, FieldDescription,
	FieldTriggerWords, FieldRecommendedWeight, FieldRecommendedSettings,
	FieldNSFW, FieldOrigin,
}

// Candidate is one source's opinion about one field.
type Candidate struct {
	Field      string
	Value      string // JSON-encoded
	Source     string
	Tier       Tier
	ObservedAt time.Time
}

// Resolution is the outcome for one field.
type Resolution struct {
	Field  string
	Value  string
	Source string
	Tier   Tier

	// Losers are the candidates that did not win, most-plausible first. Kept so
	// the UI can show what else was on offer instead of presenting the winner as
	// the only thing anyone said.
	Losers []Candidate
}

// Resolve picks the winning candidate for a single field.
//
// Order: highest tier, then highest within-tier trust, then most recent. Recency
// is deliberately last — a tool that rescans hourly should not be able to
// out-argue a better source simply by being noisier.
func Resolve(candidates []Candidate) (Resolution, bool) {
	if len(candidates) == 0 {
		return Resolution{}, false
	}

	sorted := make([]Candidate, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Tier != b.Tier {
			return a.Tier > b.Tier
		}
		ta, tb := TrustOf(a.Source), TrustOf(b.Source)
		if ta != tb {
			return ta > tb
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.After(b.ObservedAt)
		}
		// Total order, so resolution is reproducible rather than dependent on
		// the order rows came back from SQLite.
		return a.Source < b.Source
	})

	winner := sorted[0]
	return Resolution{
		Field:  winner.Field,
		Value:  winner.Value,
		Source: winner.Source,
		Tier:   winner.Tier,
		Losers: sorted[1:],
	}, true
}

// Disagreement is an origin-tier value that contradicts a sticky manual one.
type Disagreement struct {
	Field          string
	ManualValue    string
	SuggestedValue string
	Source         string
}

// FindDisagreements reports where Origin contradicts Manual.
//
// Manual correctly wins, but a manual value that is simply wrong would otherwise
// become invisible and permanent. Surfacing the conflict as a pending suggestion
// is what keeps "never overwritten" from meaning "never correctable" (§7.1).
//
// Only Origin is worth raising. Tool scrapes disagreeing with a typed value is
// the normal state of the world, and surfacing those would bury the real ones.
func FindDisagreements(candidates []Candidate) []Disagreement {
	var manual *Candidate
	for i := range candidates {
		if candidates[i].Tier == TierManual {
			manual = &candidates[i]
			break
		}
	}
	if manual == nil {
		return nil
	}

	var out []Disagreement
	for _, c := range candidates {
		if c.Tier != TierOrigin || valuesEqual(c.Value, manual.Value) {
			continue
		}
		out = append(out, Disagreement{
			Field:          c.Field,
			ManualValue:    manual.Value,
			SuggestedValue: c.Value,
			Source:         c.Source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// valuesEqual compares two JSON-encoded values semantically, so that a
// re-serialized array with different key order or spacing does not read as a
// disagreement and generate a suggestion nobody needs to act on.
func valuesEqual(a, b string) bool {
	if a == b {
		return true
	}
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return false
	}
	ab, err1 := json.Marshal(normalize(av))
	bb, err2 := json.Marshal(normalize(bv))
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// normalize recursively rebuilds a decoded JSON value so marshalling it is
// order-stable. Go already sorts map keys on marshal; this exists to strip the
// difference between, say, 1 and 1.0 arriving from different encoders.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	default:
		return v
	}
}

// EncodeValue JSON-encodes a value for storage.
func EncodeValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("provenance: encoding value: %w", err)
	}
	return string(b), nil
}

// DecodeString decodes a stored value as a string, tolerating a value that was
// stored as a number or bool by a sloppy adapter.
func DecodeString(encoded string) (string, bool) {
	var s string
	if err := json.Unmarshal([]byte(encoded), &s); err == nil {
		return s, true
	}
	var any_ any
	if err := json.Unmarshal([]byte(encoded), &any_); err != nil {
		return "", false
	}
	switch t := any_.(type) {
	case float64:
		return trimFloat(t), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case nil:
		return "", false
	}
	return "", false
}

// DecodeFloat decodes a stored value as a number, tolerating a numeric string.
func DecodeFloat(encoded string) (float64, bool) {
	var f float64
	if err := json.Unmarshal([]byte(encoded), &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal([]byte(encoded), &s); err == nil {
		var parsed float64
		if _, err := fmt.Sscanf(s, "%g", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// DecodeBool decodes a stored value as a bool, tolerating the several spellings
// of truth that arrive from different tools' sidecars.
func DecodeBool(encoded string) (bool, bool) {
	var b bool
	if err := json.Unmarshal([]byte(encoded), &b); err == nil {
		return b, true
	}
	if s, ok := DecodeString(encoded); ok {
		switch s {
		case "true", "True", "TRUE", "1", "yes", "Yes":
			return true, true
		case "false", "False", "FALSE", "0", "no", "No":
			return false, true
		}
	}
	if f, ok := DecodeFloat(encoded); ok {
		return f != 0, true
	}
	return false, false
}

// DecodeStringSlice decodes a stored value as a list, tolerating a single string
// where a list was expected — several tools write one trigger word bare.
func DecodeStringSlice(encoded string) ([]string, bool) {
	var out []string
	if err := json.Unmarshal([]byte(encoded), &out); err == nil {
		return out, true
	}
	if s, ok := DecodeString(encoded); ok && s != "" {
		return []string{s}, true
	}
	return nil, false
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	return s
}
