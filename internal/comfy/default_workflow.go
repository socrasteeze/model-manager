package comfy

// A workflow that works out of the box.
//
// The alternative was to ship nothing and require the user to paste an API-
// format graph before the first render, which turns "make me a thumbnail" into
// a ComfyUI configuration exercise. This is a plain SDXL-shaped lora preview:
// load a checkpoint, apply the model being previewed as a lora, sample, save.
//
// It is a starting point, not a claim to be right for every model. The
// checkpoint name is a placeholder the user has to point at something they
// actually have, and the settings UI shows the whole thing for editing. What
// matters is that it is the *API* format, so it can be queued as-is.

import "encoding/json"

// DefaultWorkflow is the template used when none has been configured.
var DefaultWorkflow = json.RawMessage(`{
  "1": {
    "class_type": "CheckpointLoaderSimple",
    "inputs": { "ckpt_name": "{{checkpoint}}" }
  },
  "2": {
    "class_type": "LoraLoader",
    "inputs": {
      "lora_name": "{{model}}",
      "strength_model": 1.0,
      "strength_clip": 1.0,
      "model": ["1", 0],
      "clip": ["1", 1]
    }
  },
  "3": {
    "class_type": "CLIPTextEncode",
    "inputs": { "text": "{{prompt}}", "clip": ["2", 1] }
  },
  "4": {
    "class_type": "CLIPTextEncode",
    "inputs": { "text": "{{negative}}", "clip": ["2", 1] }
  },
  "5": {
    "class_type": "EmptyLatentImage",
    "inputs": { "width": 1024, "height": 1024, "batch_size": 1 }
  },
  "6": {
    "class_type": "KSampler",
    "inputs": {
      "seed": {{seed}},
      "steps": 25,
      "cfg": 6.0,
      "sampler_name": "dpmpp_2m",
      "scheduler": "karras",
      "denoise": 1.0,
      "model": ["2", 0],
      "positive": ["3", 0],
      "negative": ["4", 0],
      "latent_image": ["5", 0]
    }
  },
  "7": {
    "class_type": "VAEDecode",
    "inputs": { "samples": ["6", 0], "vae": ["1", 2] }
  },
  "8": {
    "class_type": "SaveImage",
    "inputs": { "filename_prefix": "model-manager", "images": ["7", 0] }
  }
}`)

// DefaultNegative is used when the caller supplies no negative prompt.
const DefaultNegative = "blurry, low quality, watermark, text, jpeg artifacts"
