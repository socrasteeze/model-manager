# Model Manager — Rough Plan Spec

Working name: TBD. Independent metadata authority for the local model library.

Status: not started. This is a scoping document, not a design freeze.

---

## Problem

~19,000 model files, ~7.5TB, currently migrating from the NAS home share to
`/Volume1/ai/models`, with ~2.76TB of orphaned files being relocated to
`models-archive`.

Four tools each maintain their own metadata sidecars and each believes it owns them:

- ComfyUI + LoRA Manager extension
- SwarmUI
- Stability Matrix
- Civitai (remote origin)

**Observed failure:** running a metadata pull in Stability Matrix or SwarmUI sometimes
disconnects the master JSON, leaving safetensors with no matching metadata.

**Root cause:** every tool binds metadata to the model by *path and filename*.
`model.safetensors` → `model.json`. That link is positional, not intrinsic. Move,
rename, or have one tool rewrite a sidecar another tool owns, and the binding is gone —
nothing in the JSON knows which safetensors it belonged to. Migration generates path
churn constantly, which is why this is getting worse rather than better.

---

## Core principle

**Metadata binds to file content, not to path.**

Primary key is the SHA256 of the safetensors file. Path becomes a mutable attribute.

Consequences:

- File moves between roots → hash unchanged → metadata still attached
- Renamed → same
- Same model present in two locations → one record, two paths
- A tool scrambles its sidecar → rehash the file, look it up, regenerate
- Orphaning becomes impossible — any loose safetensors can be re-attributed by hashing it

Bonus: Civitai indexes by SHA256, so the key that identifies the file locally is the same
key used to query them for enrichment.

---

## Architecture

```
  Civitai API ─┐
  HuggingFace ─┤
  ComfyUI/LM  ─┼──> INGEST (read-only) ──> MASTER RECORD ──> PROJECT (write) ──> tool sidecars
  StabMatrix  ─┤                            (this app owns)                       (disposable)
  SwarmUI     ─┤
  Manual      ─┘
```

**Ingest is strictly read-only.** Parse whatever each tool has, merge into master. No
external tool ever writes master.

**Consumer sidecars are derived artifacts.** Project master out into each tool's dialect.
If Stability Matrix mangles one, regenerate it. Their "pull metadata" button stops being
a threat and becomes a no-op overwritten on next projection.

---

## Data model (sketch)

**Model file**
- `sha256` (PK)
- `size`, `mtime`
- `paths[]` — every location this hash exists at
- `format` — safetensors / ckpt / gguf / pt
- `first_seen`, `last_verified`

**Model record** (1:1 with sha256, but conceptually separate — the file vs what it *is*)
- `type` — checkpoint / LoRA / LyCORIS / VAE / embedding / controlnet / upscaler
- `base_model` — SDXL / Flux / Krea 2 / Qwen / Wan / Anima 2B / etc.
- `name`, `version`, `description`
- `trigger_words[]`
- `recommended_weight`, `recommended_settings`
- `nsfw` flag
- `tags[]`
- `preview_images[]`
- `origin` — civitai / huggingface / self-trained / unknown

**Field provenance** — this is the part that prevents re-inventing the current bug.

Every field stores: `value`, `source`, `timestamp`.

Precedence, highest first:

1. **Manual** — entered by you. Sticky. Never overwritten by any ingest, ever.
2. **Origin** — Civitai or HF by hash. Authoritative for anything untouched.
3. **Tool-derived** — StabMat / Swarm / LoRA Manager scrapes. Hints only, lowest trust.

Manual precedence matters more here than in most tools: self-trained LoRAs (Anima 2B
runs) have no remote record at all. Manual is the *only* source for a real slice of the
library and has to be untouchable.

**Training record** (self-trained models only)
- `dataset` — which dataset, how many images
- `base` — Anima 2B, etc.
- `config` — rank, alpha, optimizer, LR, steps, batch
- `trainer` — ai-toolkit / Anima TrainFlow / OneTrainer
- `notes` — what worked, what did not
- `run_date`

This is the thing no existing tool does at all, and it is arguably the highest-value part
of the whole app. Right now that knowledge lives in scattered configs and memory.

---

## Ingest sources

| Source | Read | Notes |
|---|---|---|
| Filesystem scan | paths, size, mtime, hash | The foundation. Everything else keys off it. |
| Civitai API | full metadata by SHA256 | Rate-limited. Cache aggressively. Not all models present. |
| HuggingFace | repo card, config, license | Match by repo/filename — no hash index, so weaker binding. |
| ComfyUI / LoRA Manager | its JSON sidecars | Treat as hints. |
| Stability Matrix | its metadata store | Treat as hints. |
| SwarmUI | its model metadata | Treat as hints. |
| Manual entry | anything | Highest precedence. Required for self-trained. |
| Safetensors header | embedded metadata | Free, offline, sometimes has training config. Worth parsing. |

Note on that last row: safetensors files carry a JSON header. For self-trained LoRAs it
often contains the training parameters directly. Parse it during the hash pass — it costs
almost nothing since the file is already open, and it may auto-populate a chunk of the
training record.

---

## Use cases

What this actually gets used for, in rough priority:

1. **Find a model.** Search across 19k by name, base model, type, trigger word, tag.
   Currently impossible.
2. **Answer "what is this file?"** Point at an orphaned safetensors, get its identity back.
3. **Recover from a tool stomping metadata.** Reproject and move on.
4. **Browse from phone/iPad over the tailnet.** Look up trigger words and recommended
   weights while generating from the couch.
5. **Download to a directory from a Civitai or HF link.** Paste or share a link, pick a
   destination, fetch model + metadata + preview together.
6. **Recall training configs.** Which dataset and settings produced this LoRA.
7. **Report duplicates.** Same hash in multiple locations. Report only — see non-goals.
8. **Spot gaps.** Models with no metadata, no preview, no known base model.

---

## Non-goals

Hard fences. Violating any of these turns a finishable project into an unfinishable one.

- **Does not generate images.** Not a frontend. ComfyUI and Swarm already do this.
- **Never moves, renames, or deletes files.** FreeFileSync owns file placement. A write
  path into a 7.5TB store mid-migration is how data is lost irrecoverably.
- **Does not dedupe by deleting.** Reports duplicate hashes, surfaces them, lets you act
  elsewhere. Does not pull the trigger.
- **Does not reorganize the directory tree.**
- **Not a general-purpose file manager.**

---

## Phasing

**Phase 0 — hash pass, no app.**
A script. Walk the model roots, hash everything, parse safetensors headers, write to a
database. Nothing else. Output answers one question: how many *distinct* models exist
across 19k files, and how much of the 7.5TB is duplication. That number decides whether
this is a 3,000-row problem or a 19,000-row one, and it surfaces the duplicate situation
for free.

Cache by `(path, size, mtime)` so re-runs are incremental.

**Hash on the machine with the disks locally attached.** Not across SMB — 7.5TB over the
wire is a day-plus; local off the btrfs array is IO-bound and finishes.

**Phase 1 — read-only index + UI.**
Ingest from all sources. Search, browse, view. Writes nothing outside its own database.
Prove the index is correct against the full library before it is allowed to emit a single
byte elsewhere.

**Phase 2 — enrichment.**
Civitai and HF lookup by hash. Manual editing. Training record entry.

**Phase 3 — projection.**
Generate tool sidecars from master. Separate decision, made only after phase 1 has proven
the index is trustworthy. Start with one tool, verify, then add others.

**Phase 4 — download-to-directory.**
Link in, model + metadata + preview out, into a chosen path. Reuse the Civitai URL
handling already written for the SwarmUI share target.

---

## Stack

Follow the LiftSet pattern — it shipped, it is understood, and the infrastructure exists.

- React 18 / TypeScript / Vite
- Self-hosted Supabase on EAGLE-424 (already running, 11 containers healthy — add a
  schema, not a stack)
- Deployed to the EAGLE-424 Docker fleet, tailnet-only
- Reuse the existing in-app pull-from-GitHub update button pattern
- PWA — this needs to work well on phone and iPad by design, not as an afterthought

Scanner/hasher is a separate worker process, not part of the web app. It needs local disk
access and runs long; the UI should never block on it.

---

## Open questions

- Postgres full-text search vs. a dedicated search index at 19k rows. Probably Postgres
  is fine — verify after phase 0 gives the real row count.
- Preview image storage: reference in place, or copy into app-managed storage? In place is
  simpler but breaks when files move. Content-addressed copies are safer but cost disk.
- How to handle a hash collision between a model and its own quantized/pruned variant —
  different hashes, same logical model. Needs a "variant of" relation, probably in phase 2.
- Whether projection should be push (app writes sidecars) or pull (each tool queries an
  API). Push works with tools as they exist today. Pull is cleaner but nothing supports it.
- Civitai API rate limits at 19k lookups. Almost certainly needs throttling and resumable
  batch runs.
