package interpret

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

// FromPath derives what it can from where a file sits and what it is called.
//
// This is the weakest source in the system and is registered at the lowest trust
// for that reason: a directory called `SDXL` is a statement about how someone
// once organized their disk, not about what the file is. It earns its place
// anyway, because it gives every model a name to search on from the very first
// scan, before any enrichment has run — and "find a model" is use case 1.
func FromPath(path string) Result {
	var res Result
	if path == "" {
		return res
	}

	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if name := prettifyFilename(stem); name != "" {
		res.add(provenance.FieldName, name)
	}

	dirs := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	if t := typeFromDirs(dirs); t != "" {
		res.add(provenance.FieldType, t)
	}
	if b := baseModelFromDirs(dirs, stem); b != "" {
		res.add(provenance.FieldBaseModel, b)
	}
	return res
}

// dirTypeHints maps the directory names the major tools actually use. Matching
// is on whole path segments, so a model inside `my-loras-backup` is not
// classified by accident.
var dirTypeHints = map[string]string{
	"loras": "lora", "lora": "lora", "lycoris": "lycoris", "locon": "lycoris",
	"checkpoints": "checkpoint", "checkpoint": "checkpoint",
	"stable-diffusion": "checkpoint", "unet": "checkpoint", "diffusion_models": "checkpoint",
	"vae": "vae", "vae_approx": "vae",
	"embeddings": "embedding", "embedding": "embedding", "textual_inversion": "embedding",
	"controlnet": "controlnet", "control-net": "controlnet", "controlnets": "controlnet",
	"upscale_models": "upscaler", "esrgan": "upscaler", "upscalers": "upscaler",
}

func typeFromDirs(dirs []string) string {
	// Walk from the deepest directory outward: the closest enclosing folder is
	// the one most likely to have been chosen deliberately for this file.
	for i := len(dirs) - 1; i >= 0; i-- {
		if t, ok := dirTypeHints[strings.ToLower(dirs[i])]; ok {
			return t
		}
	}
	return ""
}

// baseModelPatterns are matched against directory segments and the filename.
// Ordered longest-first so `sdxl` is tested before `sd`.
var baseModelPatterns = []struct {
	re   *regexp.Regexp
	base string
}{
	{regexp.MustCompile(`(?i)\b(sdxl|xl)\b`), "SDXL"},
	{regexp.MustCompile(`(?i)\bpony\b`), "Pony"},
	{regexp.MustCompile(`(?i)\billustrious\b`), "Illustrious"},
	{regexp.MustCompile(`(?i)\bnoobai\b`), "NoobAI"},
	{regexp.MustCompile(`(?i)\bflux(\.1|1)?\b`), "Flux"},
	{regexp.MustCompile(`(?i)\bsd\s*3(\.5)?\b`), "SD 3"},
	{regexp.MustCompile(`(?i)\bsd\s*1\.?5\b`), "SD 1.5"},
	{regexp.MustCompile(`(?i)\bsd\s*2\.?\d?\b`), "SD 2.x"},
	{regexp.MustCompile(`(?i)\bqwen\b`), "Qwen"},
	{regexp.MustCompile(`(?i)\bwan\s*2?\.?\d?\b`), "Wan"},
	{regexp.MustCompile(`(?i)\bhunyuan\b`), "Hunyuan"},
	{regexp.MustCompile(`(?i)\banima\s*2b\b`), "Anima 2B"},
	{regexp.MustCompile(`(?i)\bkrea\s*2?\b`), "Krea 2"},
}

func baseModelFromDirs(dirs []string, stem string) string {
	for i := len(dirs) - 1; i >= 0; i-- {
		if b := matchBaseModel(dirs[i]); b != "" {
			return b
		}
	}
	// The filename is a weaker signal than a directory someone created on
	// purpose, so it is only consulted once the directories have nothing to say.
	return matchBaseModel(stem)
}

func matchBaseModel(s string) string {
	// Separators become spaces so word-boundary anchors work on the
	// `my_lora-sdxl_v2` style names that dominate in the wild.
	normalized := strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(s)
	for _, p := range baseModelPatterns {
		if p.re.MatchString(normalized) {
			return p.base
		}
	}
	return ""
}

// versionSuffix matches a trailing version marker, but deliberately not a bare
// single digit.
//
// Real libraries are full of `thing_1.safetensors` and `ckpt_0.safetensors`
// where the trailing number is an index or a shard, not a version. Reporting "0"
// as a version is worse than reporting nothing, because it looks like real
// metadata. So a version must either carry a `v` prefix or have a dotted or
// multi-digit form.
var versionSuffix = regexp.MustCompile(`(?i)[-_. ](v\d+(\.\d+)*|\d+\.\d+(\.\d+)*|\d{2,})$`)

// prettifyFilename turns a filename into something readable enough to be worth
// showing before any real metadata exists.
func prettifyFilename(stem string) string {
	s := strings.NewReplacer("_", " ", "-", " ").Replace(stem)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	// A bare hash or a numeric id is not a name; showing it would be worse than
	// showing nothing, because it looks like real metadata.
	if isHexBlob(strings.ReplaceAll(s, " ", "")) {
		return ""
	}
	return s
}

func isHexBlob(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// VersionFromFilename extracts a trailing version marker, which is how most
// people actually version their local models.
func VersionFromFilename(path string) string {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if m := versionSuffix.FindString(stem); m != "" {
		return strings.TrimLeft(strings.TrimSpace(m), "-_. ")
	}
	return ""
}
