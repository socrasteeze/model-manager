// Package modeltype owns the one vocabulary of model types this app uses.
//
// Before this package the vocabulary existed in four places that disagreed:
// the spec's canonical seven, the schema, the detail-panel dropdown, and a
// six-entry list in the browse UI. That was survivable while a type was only
// ever displayed. It stopped being survivable when a type started deciding a
// *directory name*: the browse panel pluralized whatever string a provider
// returned into `${type}s`, so an unmapped Civitai type would have created a
// directory named after it, and the mapped-but-unpluralizable ones already
// produced `lycoriss/` and `vaes/`.
//
// So the rule here is: a type is either one of the known set or it is empty.
// There is no third state where an arbitrary provider string is passed along
// as if it meant something.
package modeltype

import "strings"

// The canonical set. Order is display order, not precedence.
const (
	Checkpoint   = "checkpoint"
	LoRA         = "lora"
	LyCORIS      = "lycoris"
	Embedding    = "embedding"
	VAE          = "vae"
	ControlNet   = "controlnet"
	Upscaler     = "upscaler"
	Hypernetwork = "hypernetwork"
)

// All is every type this app recognises.
//
// hypernetwork is here because civitaiTypeToLocal already emitted it and
// nothing else in the app knew the string existed -- a type that can arrive
// but cannot be named is how an unrecognised value ends up somewhere it should
// not be.
var All = []string{
	Checkpoint, LoRA, LyCORIS, Embedding, VAE, ControlNet, Upscaler, Hypernetwork,
}

var valid = func() map[string]bool {
	m := make(map[string]bool, len(All))
	for _, t := range All {
		m[t] = true
	}
	return m
}()

// IsValid reports whether t is exactly a canonical type.
func IsValid(t string) bool { return valid[t] }

// Normalize maps the many spellings the tools and providers use onto the
// canonical set. An unrecognised value returns "", never itself: the caller
// must fall back to a default rather than act on a string nothing understands.
func Normalize(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.Join(strings.Fields(s), " ")

	switch s {
	case "lora", "loras", "lora model":
		return LoRA
	case "locon", "lycoris", "dora", "loha", "lokr":
		return LyCORIS
	case "checkpoint", "checkpoints", "model", "checkpointmerge",
		"checkpoint merge", "checkpoint trained", "stable diffusion", "unet",
		"diffusion models", "diffusion model":
		return Checkpoint
	case "textualinversion", "textual inversion", "embedding", "embeddings":
		return Embedding
	case "vae", "vaes", "vae approx":
		return VAE
	case "controlnet", "controlnets", "control net":
		return ControlNet
	case "upscaler", "upscalers", "upscale models", "esrgan",
		"aestheticgradient", "aesthetic gradient":
		return Upscaler
	case "hypernetwork", "hypernetworks":
		return Hypernetwork
	}
	return ""
}
