# ComfyUI workflows for thumbnails

You do not write a workflow for this app. You point it at one that already
works, and it adapts that workflow per model at render time.

## Setup

Once per base-model family, about fifteen seconds each:

1. In ComfyUI, open a workflow that renders one good picture of one model.
   *Browse Templates* has one per architecture; your own saved graphs work
   equally well.
2. Turn on *Settings ▸ Enable Dev mode Options*, then **Save (API Format)** into
   `<ComfyUI>/user/default/workflows`.
3. In Model Manager, **Settings ▸ Render thumbnails with ComfyUI**: set the
   ComfyUI address, set the workflow folder, and pick your file for that family.

That is the whole setup. Nothing in the file needs editing.

**Why the export step exists and cannot be skipped.** ComfyUI's canvas format
and its API format are different documents. Only the API format can be queued,
and converting between them requires the definitions of every custom node you
have installed — which this app does not have and will not fake. A file saved
the ordinary way is refused with a message saying exactly this, rather than
failing later in a way you would have to guess at.

## What the app rewrites, and what it leaves alone

Your workflow names one specific model. Rendering a *library* means that name
has to change per thumbnail, so these inputs are rewritten on a copy at render
time. **The file on disk is never modified.**

| Input | Found by | Rewritten to | When |
|---|---|---|---|
| lora | any `class_type` containing `LoraLoader` → `lora_name` | the model being previewed | **always** — this is the point |
| seed | `seed` / `noise_seed` | derived from the model's hash | always, so each model gets its own composition |
| positive prompt | traced from the sampler's `positive` link | the model's trigger words | when it has any, or you typed a prompt |
| negative prompt | traced from the sampler's `negative` link | your text | only when you supply one |
| checkpoint | `ckpt_name` / `unet_name` | the family's configured checkpoint | **only if you set one** |

Two of those deserve saying plainly.

**The checkpoint is left alone by default.** Your template names a base model
you evidently have, since the workflow worked. Overwriting it uninvited would
break a graph that was fine. Set a checkpoint for a family only when you want it
overridden.

**Prompts are traced, not guessed.** The app follows the sampler's `positive`
and `negative` inputs to the node ids they name — through a `FluxGuidance` or
similar wrapper if there is one — rather than assuming the first `CLIPTextEncode`
in the file is the positive one. It frequently is not, and a swapped pair
produces a thumbnail that is confidently wrong.

Everything else is untouched: samplers, schedulers, CFG, resolution, upscalers,
your CLIP and VAE filenames, custom nodes, and anything a future ComfyUI adds.

## One workflow per family

An SDXL/Illustrious lora and a FLUX.2 lora cannot share a graph. Different
loaders, different text encoders, a different VAE. Handed the wrong one, ComfyUI
does not render a worse picture — it errors on a model it cannot load.

So each family gets its own slot, and an unset family falls back to the
*Default* slot, and an unset default falls back to a built-in SDXL-shaped graph.

Where the checkpoint lives differs by family, which is why it is matched by
input name rather than node type:

| Family | Loader | Checkpoint input |
|---|---|---|
| SDXL, Illustrious, Pony, NoobAI | `CheckpointLoaderSimple` | `ckpt_name` |
| Flux.1, Flux.2, Krea 2 | `UNETLoader` (or a checkpoint loader for all-in-one files) | `unet_name` |
| Anima | export your own | — |

On the Flux family, CLIP and VAE are separate loaders. Their filenames are
per-family constants rather than per-model values, so they stay exactly as your
template has them.

**Anima gets no shape here.** It has its own pipeline, and this document is not
going to invent one. Export a working Anima graph and point the slot at it; the
rewriting above is architecture-independent and will handle it.

## Checking your work before spending GPU time

    mm comfy check flux2-preview.json          usable? what looks wrong?
    mm comfy plan  flux2-preview.json --sha …  exactly what would be sent

`plan` prints the substitution table — node, input, old value, new value —
without contacting ComfyUI or rendering anything. The Settings panel shows the
same thing per family.

## If you already have a rendered image

A ComfyUI PNG carries the API-format graph that made it. So the fastest route of
all, when you have an image you like:

    mm comfy adopt render.png > flux2-preview.json

The Settings panel has the same thing as a button. An image carrying only the
canvas-format graph is refused with the reason.

## Placeholders, if you want exact control

You do not need these. They exist for the case where the automatic rewriting
puts something in the wrong place, or where you want a value composed rather
than replaced — `"masterpiece, {{triggers}}"` keeps your wrapping, where
automatic rewriting would replace the whole field.

    {{model}}       the model's filename, as ComfyUI would load it
    {{checkpoint}}  the family's configured checkpoint
    {{name}}        the model's name from the library
    {{base_model}}  the family, e.g. Flux.2
    {{triggers}}    trigger words, comma-joined
    {{prompt}}      your prompt, else the trigger words, else the name
    {{negative}}    your negative prompt
    {{seed}}        a number derived from the model's hash

One rule: **`{{seed}}` emits a bare number** and belongs in a numeric field with
no quotes. Every other placeholder is a JSON-escaped string and belongs inside
quotes. A quoted seed makes ComfyUI reject the node.

Any input a placeholder controls is left alone by the automatic pass, so the two
never fight.

## When something is wrong

| What you see | What it means |
|---|---|
| *this is a ComfyUI editor workflow, not the API form* | Saved from the canvas. Re-save with *Save (API Format)*. |
| `no_lora_input` | No lora loader in the graph, so the model is never loaded and every thumbnail will look the same. Correct only if you are previewing checkpoints. |
| `no_save_node` | No `SaveImage` or `PreviewImage`, so the render produces nothing to attach. |
| *the workflow finished but produced no image* | ComfyUI ran the graph and saved nothing. Usually the missing save node above. |
| a node error quoted verbatim | ComfyUI's own complaint, passed through unchanged — most often a `ckpt_name` or `lora_name` it cannot find. Check the name matches what ComfyUI lists. |
| *ComfyUI is not reachable* | Nothing listening at the configured address. |

## The alternative: don't render at all

If you only need thumbnails for a handful of models, you do not need any of
this. Set the **ComfyUI output folder** in Settings and pick from images you have
already rendered — no workflow, no address, nothing running. It just does not
scale to a library of thousands, which is why rendering exists.
