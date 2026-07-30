package origin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// HuggingFaceBaseURL is the API root. Overridable for testing.
var HuggingFaceBaseURL = "https://huggingface.co/api"

// HuggingFace has no hash index (spec §12), so a file can only be matched by
// repo and filename. That is a weaker binding than Civitai's -- it is a claim
// about a name, not about content -- which is why everything from here is
// recorded at the origin tier but can be contradicted by a Civitai hash match,
// and why nothing here ever writes a hash into origin_hash.

// HFModel is the subset of the repo response worth typing. As with Civitai, the
// full body is archived verbatim, so this being incomplete costs nothing.
type HFModel struct {
	ID          string   `json:"id"`
	Author      string   `json:"author"`
	PipelineTag string   `json:"pipeline_tag"`
	Tags        []string `json:"tags"`
	Downloads   int64    `json:"downloads"`
	Likes       int64    `json:"likes"`
	LibraryName string   `json:"library_name"`
	CardData    struct {
		License        string   `json:"license"`
		BaseModel      any      `json:"base_model"`
		Tags           []string `json:"tags"`
		InstancePrompt string   `json:"instance_prompt"`
	} `json:"cardData"`
	Siblings []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
}

// LookupHuggingFaceRepo fetches a repo's metadata.
func (c *Client) LookupHuggingFaceRepo(ctx context.Context, repo string) (json.RawMessage, int, error) {
	url := fmt.Sprintf("%s/models/%s", c.huggingFaceBase(), strings.Trim(repo, "/"))
	return c.getJSON(ctx, url)
}

// ObservationsFromHuggingFace extracts typed fields from an archived response.
func ObservationsFromHuggingFace(raw json.RawMessage) ([]store.FieldObservation, []string) {
	var m HFModel
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}

	var obs []store.FieldObservation
	add := func(field string, value any) {
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			return
		}
		if ss, ok := value.([]string); ok && len(ss) == 0 {
			return
		}
		obs = append(obs, store.FieldObservation{Field: field, Value: value})
	}

	// The repo id is `author/name`; the second half is the human-facing name.
	if name := repoName(m.ID); name != "" {
		add(provenance.FieldName, name)
	}
	add(provenance.FieldOrigin, "huggingface")

	if base := hfBaseModel(m); base != "" {
		add(provenance.FieldBaseModel, base)
	}
	if t := hfType(m); t != "" {
		add(provenance.FieldType, t)
	}
	if m.CardData.InstancePrompt != "" {
		add(provenance.FieldTriggerWords, []string{m.CardData.InstancePrompt})
	}

	// Repo tags are dominated by machine labels (`region:us`, `license:mit`,
	// `diffusers`) that would swamp a tag facet meant for human organization.
	return obs, cleanList(filterHFTags(append(append([]string{}, m.Tags...), m.CardData.Tags...)))
}

func repoName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// hfBaseModel reads base_model, which the card writes as either a string or a
// list depending on who authored it.
func hfBaseModel(m HFModel) string {
	switch v := m.CardData.BaseModel.(type) {
	case string:
		return normalizeCivitaiBase(repoName(v))
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return normalizeCivitaiBase(repoName(s))
			}
		}
	}
	// Fall back to the tag list, where `base_model:...` also appears.
	for _, tag := range m.Tags {
		if rest, ok := strings.CutPrefix(tag, "base_model:"); ok {
			return normalizeCivitaiBase(repoName(rest))
		}
	}
	return ""
}

func hfType(m HFModel) string {
	for _, tag := range append(append([]string{}, m.Tags...), m.CardData.Tags...) {
		switch strings.ToLower(tag) {
		case "lora":
			return "lora"
		case "controlnet":
			return "controlnet"
		case "textual_inversion", "textual-inversion":
			return "embedding"
		}
	}
	if strings.Contains(strings.ToLower(m.PipelineTag), "text-to-image") {
		return "checkpoint"
	}
	return ""
}

// hfMachineTagPrefixes are namespaces HuggingFace uses for indexing rather than
// description.
var hfMachineTagPrefixes = []string{
	"region:", "license:", "base_model:", "dataset:", "arxiv:", "doi:",
	"language:", "size_categories:", "modality:", "library:", "endpoints_compatible",
	"autotrain_compatible", "has_space", "diffusers:",
}

func filterHFTags(tags []string) []string {
	var out []string
	for _, tag := range tags {
		lower := strings.ToLower(tag)
		machine := false
		for _, prefix := range hfMachineTagPrefixes {
			if strings.HasPrefix(lower, prefix) {
				machine = true
				break
			}
		}
		if !machine {
			out = append(out, tag)
		}
	}
	return out
}
