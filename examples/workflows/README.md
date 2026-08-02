# Example workflows

One file, and it is the built-in default lifted out of Go source so it can be
read and edited like any other workflow.

`sdxl-lora-preview.json` — SDXL family (also Illustrious, Pony, NoobAI). Point a
family slot at it, or copy it into your ComfyUI workflows folder and edit it
there. The `ckpt_name` and `lora_name` values in it are placeholders in the
ordinary sense: the app rewrites them per model at render time, so they only
matter if you open the file in ComfyUI to work on it.

There are deliberately no Flux, Krea or Anima examples. ComfyUI's own
*Browse Templates* has a correct, current graph for each of those, and a
best-effort one written here would be worse and would go stale. Export one with
*Save (API Format)* and point the family slot at it — see
[docs/comfyui-workflows.md](../../docs/comfyui-workflows.md).
