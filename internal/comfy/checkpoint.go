package comfy

// Resolving the configured base checkpoint for a base-model family.
//
// Shared between the render endpoint and `mm comfy plan`, which both need to
// agree on this or the CLI's promise of showing "exactly what would be
// queued" is false for anyone relying on a derivative family's inherited
// checkpoint.

import (
	"encoding/json"
	"strings"

	"github.com/socrasteeze/model-manager/internal/basemodel"
)

// CheckpointForFamily decodes a stored checkpoint setting and returns the
// checkpoint to use for one base-model family.
//
// The setting is either a bare string, meaning "use this for everything", or
// a map keyed by family name with "" as the default. A family with no slot of
// its own inherits its parent architecture's checkpoint before falling back to
// the default -- an Illustrious lora picks up the SDXL checkpoint the user
// already configured, rather than nothing.
func CheckpointForFamily(raw json.RawMessage, family string) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var byFamily map[string]string
	if err := json.Unmarshal(raw, &byFamily); err != nil {
		return ""
	}
	if c, ok := byFamily[family]; ok && strings.TrimSpace(c) != "" {
		return strings.TrimSpace(c)
	}
	if parent := basemodel.Parent(family); parent != "" {
		if c, ok := byFamily[parent]; ok && strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	return strings.TrimSpace(byFamily[""])
}
