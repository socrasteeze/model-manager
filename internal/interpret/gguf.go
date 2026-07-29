package interpret

import (
	"fmt"
	"strings"

	"github.com/socrasteeze/model-manager/internal/modelformat"
	"github.com/socrasteeze/model-manager/internal/provenance"
)

// GGUF interprets a stored GGUF header blob.
//
// GGUF metadata is conventionally namespaced (`general.*`, then per-architecture
// keys), which makes the useful part small and stable. Everything outside
// `general.*` is architecture-specific tuning that means nothing to a model
// browser, so it is not mined.
func GGUF(blob []byte, truncated bool) Result {
	var res Result
	if len(blob) == 0 {
		return res
	}

	meta, err := modelformat.GGUFMetadata(blob)
	if err != nil && len(meta) == 0 {
		if truncated {
			res.Warnings = append(res.Warnings, "header blob was truncated at the storage cap; metadata not parsed")
		} else {
			res.Warnings = append(res.Warnings, "GGUF metadata did not parse: "+err.Error())
		}
		return res
	}
	if err != nil {
		// Partial metadata is still worth having, but the reader should know the
		// picture is incomplete.
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("GGUF metadata parsed partially (%d keys) before: %v", len(meta), err))
	}

	if v, ok := meta["general.name"].(string); ok {
		res.add(provenance.FieldName, v)
	}
	if v, ok := meta["general.description"].(string); ok {
		res.add(provenance.FieldDescription, v)
	}
	if v, ok := meta["general.version"].(string); ok {
		res.add(provenance.FieldVersion, v)
	}
	if v, ok := meta["general.architecture"].(string); ok && v != "" {
		res.add(provenance.FieldBaseModel, describeGGUFArchitecture(v, meta))
	}

	// A GGUF file is a quantized weights container. Calling it a checkpoint is
	// the honest mapping onto this app's vocabulary -- it is a whole model, not
	// an adapter.
	res.add(provenance.FieldType, "checkpoint")

	return res
}

// describeGGUFArchitecture combines the architecture with the parameter count
// where one is published, because "llama" alone does not distinguish an 8B from
// a 70B and that difference is the whole reason someone is looking.
func describeGGUFArchitecture(arch string, meta map[string]any) string {
	name := strings.ToUpper(arch[:1]) + arch[1:]

	if raw, ok := meta["general.parameter_count"]; ok {
		if n := asUint(raw); n > 0 {
			return fmt.Sprintf("%s %s", name, humanParams(n))
		}
	}
	return name
}

func asUint(v any) uint64 {
	switch t := v.(type) {
	case uint64:
		return t
	case int64:
		if t > 0 {
			return uint64(t)
		}
	case float64:
		if t > 0 {
			return uint64(t)
		}
	}
	return 0
}

func humanParams(n uint64) string {
	switch {
	case n >= 1_000_000_000_000:
		return fmt.Sprintf("%.1fT", float64(n)/1e12)
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.0fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%d", n)
}
