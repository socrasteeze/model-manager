package modeltype

import (
	"path/filepath"
	"strings"
	"testing"
)

// The whole reason this package exists: an unrecognised type must not become a
// directory name. The old civitaiTypeToLocal ended `return strings.ToLower(t)`,
// and the browse UI pluralized whatever it got into `${type}s` -- so a Civitai
// type nobody had mapped would have created a folder named after it.
func TestUnknownTypeNormalizesToEmpty(t *testing.T) {
	for _, in := range []string{
		"Workflows", "Poses", "MotionModule", "../../etc", "", "   ",
		"Some Type Invented Next Year",
	} {
		if got := Normalize(in); got != "" {
			t.Errorf("Normalize(%q) = %q, want \"\" — an unknown type must never "+
				"be passed through as if it meant something", in, got)
		}
	}
}

func TestNormalizeMapsToolSpellings(t *testing.T) {
	cases := map[string]string{
		"LORA": LoRA, "loras": LoRA,
		"LoCon": LyCORIS, "DoRA": LyCORIS, "lycoris": LyCORIS,
		"Checkpoint": Checkpoint, "checkpointmerge": Checkpoint,
		"Stable-Diffusion": Checkpoint, "diffusion_models": Checkpoint,
		"TextualInversion": Embedding, "Embeddings": Embedding,
		"VAE": VAE, "vae_approx": VAE,
		"ControlNet": ControlNet, "control-net": ControlNet,
		"upscale_models": Upscaler, "ESRGAN": Upscaler,
		"Hypernetwork": Hypernetwork,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEveryCanonicalTypeNormalizesToItself(t *testing.T) {
	for _, want := range All {
		if got := Normalize(want); got != want {
			t.Errorf("Normalize(%q) = %q; the canonical set must be closed", want, got)
		}
		if !IsValid(want) {
			t.Errorf("IsValid(%q) = false for a canonical type", want)
		}
	}
}

// One type, three folder names, decided by which tool owns the root. This is
// the finding the whole feature is built on, so it is asserted directly.
func TestThreeToolsNameTheSameTypeDifferently(t *testing.T) {
	cases := []struct {
		modelType        string
		sm, swarm, comfy string
	}{
		{LoRA, "Lora", "Lora", "loras"},
		{Checkpoint, "StableDiffusion", "Stable-Diffusion", "checkpoints"},
		{Embedding, "TextualInversion", "Embeddings", "embeddings"},
	}
	for _, c := range cases {
		if got := DefaultFolder(ToolStabilityMatrix, c.modelType); got != c.sm {
			t.Errorf("stability-matrix %s = %q, want %q", c.modelType, got, c.sm)
		}
		if got := DefaultFolder(ToolSwarmUI, c.modelType); got != c.swarm {
			t.Errorf("swarmui %s = %q, want %q", c.modelType, got, c.swarm)
		}
		if got := DefaultFolder(ToolComfyUI, c.modelType); got != c.comfy {
			t.Errorf("comfyui %s = %q, want %q", c.modelType, got, c.comfy)
		}
	}
}

// An unknown tool or an unknown type gets no folder, which the caller reads as
// "the root itself". Falling back to the root is safe; fabricating a directory
// from an unvalidated string is not.
func TestDefaultFolderRefusesToInventADirectory(t *testing.T) {
	if got := DefaultFolder("some-tool-we-never-heard-of", LoRA); got != "" {
		t.Errorf("unknown tool produced folder %q", got)
	}
	if got := DefaultFolder(ToolComfyUI, "Workflows"); got != "" {
		t.Errorf("unknown type produced folder %q", got)
	}
}

func TestInferToolReadsTheDirectoryItIsGiven(t *testing.T) {
	present := func(dirs ...string) func(string) bool {
		set := map[string]bool{}
		for _, d := range dirs {
			set[d] = true
		}
		return func(p string) bool { return set[filepath.Base(p)] }
	}

	if got := InferTool("/models", present("StableDiffusion", "Lora")); got != ToolStabilityMatrix {
		t.Errorf("got %q, want stability-matrix", got)
	}
	if got := InferTool("/models", present("Stable-Diffusion", "Lora")); got != ToolSwarmUI {
		t.Errorf("got %q, want swarmui", got)
	}
	if got := InferTool("/models", present("checkpoints", "loras")); got != ToolComfyUI {
		t.Errorf("got %q, want comfyui", got)
	}
	// A directory with nothing distinctive gets no guess, and no guess means
	// downloads land in the root where the user will find them.
	if got := InferTool("/models", present("stuff", "backup")); got != "" {
		t.Errorf("guessed %q from an unrecognisable directory", got)
	}
}

// A folder name that made it into a path must be a single safe segment: these
// strings are joined onto a model root.
func TestBuiltInFoldersAreSinglePlainSegments(t *testing.T) {
	for _, tool := range Tools() {
		for _, mt := range All {
			f := DefaultFolder(tool, mt)
			if f == "" {
				continue
			}
			if strings.ContainsAny(f, `/\`) || f == ".." || strings.HasPrefix(f, ".") {
				t.Errorf("%s/%s = %q is not a plain single segment", tool, mt, f)
			}
		}
	}
}
