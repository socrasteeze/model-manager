package modeltype

// Where a model of a given type belongs, per tool.
//
// This is the finding that shaped the whole feature. On the user's own machine
// three tools hold the same kinds of file under three different folder names:
//
//	type        Stability Matrix    SwarmUI            ComfyUI
//	lora        Lora                Lora               loras
//	checkpoint  StableDiffusion     Stable-Diffusion   checkpoints
//	embedding   TextualInversion    Embeddings         embeddings
//
// So there is no such thing as "the folder for loras". A download destination
// can only be decided by the pair (root, type), because the root is what
// carries the vocabulary. A single global type -> folder map would put files
// where two of the three tools would never look for them.
//
// These tables are defaults, not policy: the user's configured map wins, and
// this is only what gets offered when nothing has been configured.

import (
	"os"
	"path/filepath"
)

// Tool identifiers stored on a managed root.
const (
	ToolStabilityMatrix = "stability-matrix"
	ToolSwarmUI         = "swarmui"
	ToolComfyUI         = "comfyui"
)

var toolFolders = map[string]map[string]string{
	ToolStabilityMatrix: {
		Checkpoint:   "StableDiffusion",
		LoRA:         "Lora",
		LyCORIS:      "LyCORIS",
		Embedding:    "TextualInversion",
		VAE:          "VAE",
		ControlNet:   "ControlNet",
		Upscaler:     "ESRGAN",
		Hypernetwork: "Hypernetwork",
	},
	ToolSwarmUI: {
		Checkpoint:   "Stable-Diffusion",
		LoRA:         "Lora",
		LyCORIS:      "Lora",
		Embedding:    "Embeddings",
		VAE:          "VAE",
		ControlNet:   "controlnet",
		Upscaler:     "upscale_models",
		Hypernetwork: "hypernetworks",
	},
	ToolComfyUI: {
		Checkpoint:   "checkpoints",
		LoRA:         "loras",
		LyCORIS:      "loras",
		Embedding:    "embeddings",
		VAE:          "vae",
		ControlNet:   "controlnet",
		Upscaler:     "upscale_models",
		Hypernetwork: "hypernetworks",
	},
}

// DefaultFolder returns the conventional subfolder for a type under a root
// belonging to the named tool.
//
// Empty means "no opinion" — the caller falls back to the root itself, which
// is the right answer for an unknown tool and for an unrecognised type. It is
// deliberately not a guess: writing a file into a fabricated directory is how
// a model ends up somewhere no tool will ever load it from.
func DefaultFolder(tool, modelType string) string {
	t := Normalize(modelType)
	if t == "" {
		return ""
	}
	folders, ok := toolFolders[tool]
	if !ok {
		return ""
	}
	return folders[t]
}

// Tools is every tool with a built-in folder vocabulary.
func Tools() []string {
	return []string{ToolStabilityMatrix, ToolSwarmUI, ToolComfyUI}
}

// KnownTool reports whether a tool identifier has a built-in vocabulary.
func KnownTool(tool string) bool {
	_, ok := toolFolders[tool]
	return ok
}

// InferTool guesses which tool's vocabulary a root already uses, by looking for
// the folder name only that tool would have created.
//
// This is deliberately decided from the directory's own contents rather than
// from install detection. What matters for placing a download is the naming
// convention *inside this folder*: a user who points the app at a bare
// directory they populated by hand should get their own layout continued, not
// the layout of whichever tool happens to be installed elsewhere on the
// machine.
//
// The discriminators are the names no other tool uses: `StableDiffusion` is
// Stability Matrix's alone, `Stable-Diffusion` SwarmUI's alone, `checkpoints`
// ComfyUI's alone. Ambiguous or empty directories return "", and "" means the
// root itself is the destination -- never a fabricated folder.
func InferTool(root string, isDir func(string) bool) string {
	for _, probe := range []struct{ dir, tool string }{
		{"StableDiffusion", ToolStabilityMatrix},
		{"Stable-Diffusion", ToolSwarmUI},
		{"checkpoints", ToolComfyUI},
		{"TextualInversion", ToolStabilityMatrix},
		{"Embeddings", ToolSwarmUI},
		{"loras", ToolComfyUI},
	} {
		if isDir(filepath.Join(root, probe.dir)) {
			return probe.tool
		}
	}
	return ""
}

// DirExists is the ordinary isDir predicate for InferTool. It is a parameter
// rather than a hardcoded call so the inference can be tested without a
// filesystem, and so a case-insensitive filesystem's answer is the one that
// counts on Windows.
func DirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
