// Package ingest reads what other tools have written beside model files.
//
// Ingest is strictly read-only (spec §4). Nothing here writes into the model
// tree, and nothing another tool has written is ever treated as authoritative --
// these are hints at the lowest trust tier, because every one of these files is
// bound to its model by path and filename, which is the positional binding this
// project exists to replace.
package ingest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Parsed is what one sidecar yielded.
type Parsed struct {
	// Source is the provenance source name for everything in this result.
	Source string

	Observations []store.FieldObservation
	Tags         []string

	// PreviewData is image bytes carried inline by the sidecar.
	PreviewData [][]byte

	// PreviewPaths are adjacent image files to copy into the blob store.
	PreviewPaths []string

	// Training is a training record where the sidecar carried one.
	Training *store.TrainingRecord
}

func (p *Parsed) add(field string, value any) {
	if value == nil {
		return
	}
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return
		}
	case []string:
		if len(v) == 0 {
			return
		}
	}
	p.Observations = append(p.Observations, store.FieldObservation{Field: field, Value: value})
}

// Discover finds every sidecar beside a model file and parses each one.
//
// A missing or unreadable sidecar is not an error: most models have none, and a
// tool half-way through writing one is a normal thing to walk past.
func Discover(modelPath string) []Parsed {
	dir := filepath.Dir(modelPath)
	base := filepath.Base(modelPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	var out []Parsed

	if p, ok := parseFile(filepath.Join(dir, stem+".cm-info.json"), parseStabilityMatrix); ok {
		out = append(out, p)
	}
	if p, ok := parseFile(filepath.Join(dir, stem+".civitai.info"), parseCivitaiInfo); ok {
		out = append(out, p)
	}
	if p, ok := parseFile(filepath.Join(dir, stem+".metadata.json"), parseLoRAManager); ok {
		out = append(out, p)
	}
	// SwarmUI and A1111 both claim `<stem>.json`, so the dialect is decided by
	// what is inside rather than by the name.
	if p, ok := parseFile(filepath.Join(dir, stem+".json"), parseAmbiguousJSON); ok {
		out = append(out, p)
	}

	if previews := findPreviewFiles(dir, stem); len(previews) > 0 {
		out = append(out, Parsed{
			Source:       provenance.SourcePathHeuristic,
			PreviewPaths: previews,
		})
	}
	return out
}

func parseFile(path string, parse func([]byte) (Parsed, bool)) (Parsed, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Parsed{}, false
	}
	// A sidecar is metadata about a model, not a model. Anything this large is
	// not what it claims to be, and parsing it would be the expensive way to
	// find that out.
	if len(data) > 8<<20 {
		return Parsed{}, false
	}
	return parse(data)
}

// previewExtensions are checked in preference order; the first hit wins per
// suffix so an ingest does not attach five copies of the same picture.
var previewExtensions = []string{".preview.png", ".preview.jpeg", ".preview.jpg",
	".preview.webp", ".png", ".jpeg", ".jpg", ".webp"}

func findPreviewFiles(dir, stem string) []string {
	var out []string
	for _, ext := range previewExtensions {
		candidate := filepath.Join(dir, stem+ext)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			out = append(out, candidate)
			// One preview per model from the filesystem. The tools that write
			// several use a sidecar to list them, and that path is handled
			// separately.
			break
		}
	}
	return out
}

// --- Stability Matrix ---------------------------------------------------------

// stabilityMatrixInfo is the `.cm-info.json` ConnectedModelInfo shape.
type stabilityMatrixInfo struct {
	ModelName          string   `json:"ModelName"`
	ModelDescription   string   `json:"ModelDescription"`
	VersionName        string   `json:"VersionName"`
	VersionDescription string   `json:"VersionDescription"`
	BaseModel          string   `json:"BaseModel"`
	ModelType          string   `json:"ModelType"`
	TrainedWords       []string `json:"TrainedWords"`
	Nsfw               *bool    `json:"Nsfw"`
	IsSfw              *bool    `json:"IsSfw"`
	Tags               []string `json:"Tags"`
}

func parseStabilityMatrix(data []byte) (Parsed, bool) {
	var info stabilityMatrixInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Parsed{}, false
	}
	// A file that parses as JSON but carries none of the expected keys is some
	// other tool's file that happens to share the name.
	if info.ModelName == "" && info.BaseModel == "" && info.ModelType == "" {
		return Parsed{}, false
	}

	p := Parsed{Source: provenance.SourceStabilityMatrix}
	p.add(provenance.FieldName, info.ModelName)
	p.add(provenance.FieldVersion, info.VersionName)
	p.add(provenance.FieldDescription, firstNonEmpty(info.ModelDescription, info.VersionDescription))
	p.add(provenance.FieldBaseModel, normalizeBaseModel(info.BaseModel))
	p.add(provenance.FieldType, normalizeType(info.ModelType))
	p.add(provenance.FieldTriggerWords, cleanStrings(info.TrainedWords))
	p.add(provenance.FieldOrigin, "civitai")

	// The two flags mean opposite things, and only one is usually present.
	switch {
	case info.Nsfw != nil:
		p.add(provenance.FieldNSFW, *info.Nsfw)
	case info.IsSfw != nil:
		p.add(provenance.FieldNSFW, !*info.IsSfw)
	}
	p.Tags = cleanStrings(info.Tags)
	return p, true
}

// --- Civitai .info (A1111 / Civitai Helper) -----------------------------------

// civitaiInfo is the model-version response the Civitai Helper extension caches.
//
// This is origin data, but it arrives through a tool's sidecar: it may be stale,
// hand-edited, or belong to a different file after a rename. It is therefore
// ingested at the tool tier, and a real hash lookup in Phase 2 supersedes it.
type civitaiInfo struct {
	Name         string   `json:"name"`
	BaseModel    string   `json:"baseModel"`
	Description  string   `json:"description"`
	TrainedWords []string `json:"trainedWords"`
	Model        struct {
		Name string `json:"name"`
		Type string `json:"type"`
		NSFW *bool  `json:"nsfw"`
	} `json:"model"`
}

func parseCivitaiInfo(data []byte) (Parsed, bool) {
	var info civitaiInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Parsed{}, false
	}
	if info.Name == "" && info.Model.Name == "" && info.BaseModel == "" {
		return Parsed{}, false
	}

	p := Parsed{Source: provenance.SourceA1111}
	p.add(provenance.FieldName, firstNonEmpty(info.Model.Name, info.Name))
	p.add(provenance.FieldVersion, info.Name)
	p.add(provenance.FieldDescription, stripHTML(info.Description))
	p.add(provenance.FieldBaseModel, normalizeBaseModel(info.BaseModel))
	p.add(provenance.FieldType, normalizeType(info.Model.Type))
	p.add(provenance.FieldTriggerWords, cleanStrings(info.TrainedWords))
	p.add(provenance.FieldOrigin, "civitai")
	if info.Model.NSFW != nil {
		p.add(provenance.FieldNSFW, *info.Model.NSFW)
	}
	return p, true
}

// --- ComfyUI LoRA Manager -----------------------------------------------------

type loraManagerInfo struct {
	ModelName string   `json:"model_name"`
	FileName  string   `json:"file_name"`
	BaseModel string   `json:"base_model"`
	Notes     string   `json:"notes"`
	Tags      []string `json:"tags"`
	UsageTips string   `json:"usage_tips"`
	Civitai   *struct {
		Name         string   `json:"name"`
		TrainedWords []string `json:"trainedWords"`
		Description  string   `json:"description"`
		Model        struct {
			Type string `json:"type"`
			NSFW *bool  `json:"nsfw"`
		} `json:"model"`
	} `json:"civitai"`
}

func parseLoRAManager(data []byte) (Parsed, bool) {
	var info loraManagerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Parsed{}, false
	}
	if info.ModelName == "" && info.FileName == "" && info.BaseModel == "" {
		return Parsed{}, false
	}

	p := Parsed{Source: provenance.SourceComfyUILoRAMgr}
	p.add(provenance.FieldName, info.ModelName)
	p.add(provenance.FieldBaseModel, normalizeBaseModel(info.BaseModel))
	p.add(provenance.FieldDescription, info.Notes)
	p.Tags = cleanStrings(info.Tags)

	if info.Civitai != nil {
		p.add(provenance.FieldVersion, info.Civitai.Name)
		p.add(provenance.FieldTriggerWords, cleanStrings(info.Civitai.TrainedWords))
		p.add(provenance.FieldType, normalizeType(info.Civitai.Model.Type))
		p.add(provenance.FieldOrigin, "civitai")
		if info.Civitai.Model.NSFW != nil {
			p.add(provenance.FieldNSFW, *info.Civitai.Model.NSFW)
		}
		if p.Observations == nil || !hasField(p, provenance.FieldDescription) {
			p.add(provenance.FieldDescription, stripHTML(info.Civitai.Description))
		}
	}

	// usage_tips is a JSON document stored as a string.
	if info.UsageTips != "" {
		var tips map[string]any
		if err := json.Unmarshal([]byte(info.UsageTips), &tips); err == nil {
			if v, ok := numberFrom(tips["strength"]); ok {
				p.add(provenance.FieldRecommendedWeight, v)
			}
			if len(tips) > 0 {
				if b, err := json.Marshal(tips); err == nil {
					p.add(provenance.FieldRecommendedSettings, json.RawMessage(b))
				}
			}
		}
	}
	return p, true
}

// --- SwarmUI and A1111 both use <stem>.json -----------------------------------

type swarmInfo struct {
	ModelName      string   `json:"model_name"`
	Title          string   `json:"title"`
	Author         string   `json:"author"`
	Description    string   `json:"description"`
	TriggerPhrase  string   `json:"trigger_phrase"`
	UsageHint      string   `json:"usage_hint"`
	Architecture   string   `json:"architecture"`
	PreviewImage   string   `json:"preview_image"`
	Tags           []string `json:"tags"`
	StandardWidth  int      `json:"standard_width"`
	StandardHeight int      `json:"standard_height"`
	Date           string   `json:"date"`
}

type a1111Info struct {
	Description     string `json:"description"`
	SDVersion       string `json:"sd version"`
	ActivationText  string `json:"activation text"`
	PreferredWeight any    `json:"preferred weight"`
	Notes           string `json:"notes"`
}

// parseAmbiguousJSON decides which tool wrote `<stem>.json` from its keys.
//
// Guessing wrong would attribute one tool's data to another, which corrupts the
// same-tier trust ordering that §7.1 relies on -- so a file matching neither
// shape is skipped rather than assigned to whichever seems closer.
func parseAmbiguousJSON(data []byte) (Parsed, bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return Parsed{}, false
	}

	swarmKeys := 0
	for _, k := range []string{"trigger_phrase", "architecture", "preview_image", "usage_hint", "model_name"} {
		if _, ok := probe[k]; ok {
			swarmKeys++
		}
	}
	a1111Keys := 0
	for _, k := range []string{"activation text", "preferred weight", "sd version", "notes"} {
		if _, ok := probe[k]; ok {
			a1111Keys++
		}
	}

	switch {
	case swarmKeys > a1111Keys && swarmKeys > 0:
		return parseSwarm(data)
	case a1111Keys > 0:
		return parseA1111(data)
	}
	return Parsed{}, false
}

func parseSwarm(data []byte) (Parsed, bool) {
	var info swarmInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Parsed{}, false
	}

	p := Parsed{Source: provenance.SourceSwarmUI}
	p.add(provenance.FieldName, firstNonEmpty(info.Title, info.ModelName))
	p.add(provenance.FieldDescription, firstNonEmpty(info.Description, info.UsageHint))
	p.add(provenance.FieldTriggerWords, splitList(info.TriggerPhrase))
	p.add(provenance.FieldBaseModel, mapArchitecture(info.Architecture))
	p.Tags = cleanStrings(info.Tags)

	if info.StandardWidth > 0 && info.StandardHeight > 0 {
		settings := map[string]any{
			"width":  info.StandardWidth,
			"height": info.StandardHeight,
		}
		if b, err := json.Marshal(settings); err == nil {
			p.add(provenance.FieldRecommendedSettings, json.RawMessage(b))
		}
	}

	// SwarmUI embeds its preview as a data URI rather than as a separate file.
	if img := decodeDataURI(info.PreviewImage); len(img) > 0 {
		p.PreviewData = append(p.PreviewData, img)
	}
	return p, true
}

func parseA1111(data []byte) (Parsed, bool) {
	var info a1111Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Parsed{}, false
	}

	p := Parsed{Source: provenance.SourceA1111}
	p.add(provenance.FieldDescription, firstNonEmpty(info.Description, info.Notes))
	p.add(provenance.FieldBaseModel, normalizeBaseModel(info.SDVersion))
	p.add(provenance.FieldTriggerWords, splitList(info.ActivationText))
	if w, ok := numberFrom(info.PreferredWeight); ok {
		p.add(provenance.FieldRecommendedWeight, w)
	}
	return p, true
}

// --- shared helpers -----------------------------------------------------------

func hasField(p Parsed, field string) bool {
	for _, o := range p.Observations {
		if o.Field == field {
			return true
		}
	}
	return false
}

// normalizeType maps each tool's vocabulary onto this app's. An unrecognized
// value is dropped rather than passed through, so the type facet stays a closed
// set that a filter can rely on.
func normalizeType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "lora", "loras":
		return "lora"
	case "locon", "lycoris", "dora":
		return "lycoris"
	case "checkpoint", "model", "checkpointmerge", "checkpoint trained":
		return "checkpoint"
	case "textualinversion", "embedding", "textual inversion":
		return "embedding"
	case "vae":
		return "vae"
	case "controlnet":
		return "controlnet"
	case "upscaler", "aestheticgradient":
		return "upscaler"
	}
	return ""
}

// normalizeBaseModel collapses the many spellings the tools use for the same
// family, so filtering by base model returns one bucket rather than six.
func normalizeBaseModel(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return ""
	}
	switch {
	case strings.Contains(s, "pony"):
		return "Pony"
	case strings.Contains(s, "illustrious"):
		return "Illustrious"
	case strings.Contains(s, "noobai"):
		return "NoobAI"
	case strings.Contains(s, "sdxl") || strings.Contains(s, "sd xl") || s == "xl":
		return "SDXL"
	case strings.Contains(s, "flux"):
		return "Flux"
	case strings.Contains(s, "sd 3") || strings.Contains(s, "sd3"):
		return "SD 3"
	case strings.Contains(s, "sd 1") || strings.Contains(s, "sd1"):
		return "SD 1.5"
	case strings.Contains(s, "sd 2") || strings.Contains(s, "sd2"):
		return "SD 2.x"
	case strings.Contains(s, "qwen"):
		return "Qwen"
	case strings.Contains(s, "wan"):
		return "Wan"
	case strings.Contains(s, "hunyuan"):
		return "Hunyuan"
	}
	// An unrecognized base model is kept verbatim: the set is open-ended and a
	// new architecture appearing should not silently vanish from the index.
	return strings.TrimSpace(v)
}

func mapArchitecture(v string) string {
	l := strings.ToLower(v)
	switch {
	case strings.Contains(l, "stable-diffusion-xl"):
		return "SDXL"
	case strings.Contains(l, "flux"):
		return "Flux"
	case strings.Contains(l, "stable-diffusion-v3"):
		return "SD 3"
	case strings.Contains(l, "stable-diffusion-v1"):
		return "SD 1.5"
	case strings.Contains(l, "stable-diffusion-v2"):
		return "SD 2.x"
	}
	return ""
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' })
	return cleanStrings(parts)
}

func cleanStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" || seen[t] {
			continue
		}
		// A trigger word or tag this long is a paragraph that landed in the
		// wrong field.
		if len(t) > 200 {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func numberFrom(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// decodeDataURI extracts bytes from a `data:image/...;base64,...` value.
func decodeDataURI(v string) []byte {
	if !strings.HasPrefix(v, "data:") {
		return nil
	}
	comma := strings.IndexByte(v, ',')
	if comma < 0 || !strings.Contains(v[:comma], "base64") {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(v[comma+1:])
	if err != nil {
		return nil
	}
	return data
}

// stripHTML removes the markup Civitai descriptions carry, which would otherwise
// be rendered as literal tags or, worse, injected into the UI.
func stripHTML(s string) string {
	stripped := s
	// Only the tag-removal walk is conditional. Entity decoding and whitespace
	// collapsing apply to every description, tagged or not -- a sidecar written
	// as plain text still arrives with entities and hard line breaks in it.
	if strings.ContainsRune(s, '<') {
		var b strings.Builder
		depth := 0
		for _, r := range s {
			switch {
			case r == '<':
				depth++
			case r == '>':
				if depth > 0 {
					depth--
				}
			case depth == 0:
				b.WriteRune(r)
			}
		}
		stripped = b.String()
	}

	out := strings.ReplaceAll(stripped, "&nbsp;", " ")
	out = strings.ReplaceAll(out, "&amp;", "&")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&quot;", `"`)
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}
