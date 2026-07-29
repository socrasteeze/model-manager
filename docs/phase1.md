# Phase 1 — read-only index, API, UI

Phase 0 recorded raw facts. Phase 1 turns them into something you can search,
browse and correct, and puts an HTTP API and web UI over it.

Section references (§7, §11, …) point into
[`../model-manager-spec.md`](../model-manager-spec.md).

---

## The provenance engine

This is the part that prevents re-inventing the bug the whole project exists to
fix (§7).

**Nothing is ever overwritten.** Every source's opinion about every field is
stored as its own row. A resolver picks a winner; the losers stay.

### Precedence

1. **Manual** — what you typed. Sticky, never overwritten by any ingest.
2. **Origin** — Civitai or HuggingFace by hash.
3. **Tool-derived** — everything else.

Within a tier: a fixed per-source trust order, *then* recency. Recency is
deliberately last, or a tool that rescans hourly would out-argue a better source
by being noisier.

The tool tier's internal order is a judgement call worth stating: a
safetensors/GGUF header outranks every third-party scrape, because it is data the
trainer embedded in the file itself and travels with the bytes rather than
sitting in a sidecar something else may have rewritten. Path heuristics rank
last — a folder called `SDXL` is a statement about how someone once organized a
disk.

An unclassified source defaults to the lowest tier, so a source nobody has
triaged can never outrank a value you typed.

### Manual is sticky but not a trap

Manual values are never overwritten. That alone would make a mistyped value
permanent, so two things exist alongside it (§7.1):

- **Clearing** deletes the manual row, letting lower tiers resolve again. It
  deletes rather than storing an empty string, because an empty manual value is
  itself a legitimate thing to want and the two intentions must stay
  distinguishable.
- **Suggestions.** When an origin later disagrees with a manual value, the
  conflict surfaces as a pending suggestion with one-click accept. Suggestions
  withdraw themselves when the disagreement resolves, and a dismissed one only
  returns if the offer actually changed.

Only *origin* disagreements are surfaced. A tool scrape contradicting a typed
value is the normal state of the world, and surfacing those would bury the ones
that matter.

### Silence, and when it is a retraction

`RecordObservations` treats a source that stops mentioning a field as having no
new opinion — its previous claim stands. That is right for external sidecars: a
tool that crashes halfway through writing one must not be able to erase a value.

`ReplaceObservations` is used for **our own derived sources** — the header
interpreter and the path heuristics — which recompute from complete input every
run. There, silence *is* a retraction: a field the current rules no longer
produce is a stale artifact of an older rule, not a surviving opinion. Without
this, an interpretation bug could never be fully fixed.

---

## Header interpretation

`mm interpret` turns the headers Phase 0 captured into typed metadata. **It reads
no model files** — the blobs are already in the database, which is the whole
reason Phase 0 stored them uninterpreted, and what makes this safe to re-run
whenever the rules improve.

Three sources:

- **Tensor names.** The most reliable signal available offline: a sidecar can be
  wrong or absent, but the tensor namespace *is* the model. Adapter markers are
  tested before checkpoint markers, since a LoRA targeting a diffusion UNet has
  `unet` throughout its names and would otherwise be misfiled.
- **kohya/sd-scripts `ss_*` metadata**, which yields the §8 training record for
  free: rank, alpha, optimizer, LR, steps, batch, dataset size, run date. Plus
  `modelspec.*` for the cross-tool convention.
- **Filenames and directories**, at the lowest trust in the system. They earn
  their place by giving every model a searchable name from the first scan, before
  any enrichment exists — and "find a model" is use case 1.

Trigger words are mined from `ss_tag_frequency`: a tag present on essentially
every training image is what the model was taught to respond to. If more than a
handful qualify, the threshold found a captioning vocabulary rather than a
trigger set, and it emits nothing.

Two guards worth knowing: a hash-like filename yields no name at all, and a bare
trailing digit is not treated as a version. In both cases metadata that *looks*
real but is not is worse than an empty field — real libraries are full of
`ckpt_0`, `thing_1`.

---

## Ingest

`mm ingest` reads what other tools wrote. Strictly read-only; nothing is written
into the model tree.

| Tool | File |
|---|---|
| Stability Matrix | `<model>.cm-info.json` |
| Civitai Helper / A1111 | `<model>.civitai.info` |
| ComfyUI LoRA Manager | `<model>.metadata.json` |
| SwarmUI **or** A1111 | `<model>.json` |

That last collision is resolved by dispatching on the keys present, not the
filename. Guessing wrong would attribute one tool's data to another and corrupt
the same-tier trust ordering, so a file matching neither shape is skipped.

`.civitai.info` is origin data, but it arrives through a sidecar that may be
stale, hand-edited, or left behind by a rename — so it lands at the tool tier. A
real hash lookup in Phase 2 supersedes it.

Vocabularies are normalized on the way in. `type` collapses to a closed set a
filter can rely on, with unrecognized values dropped. `base_model` keeps
unrecognized values verbatim, because that set is open-ended and a new
architecture must not silently vanish. Pony and Illustrious are matched before
SDXL, since their official names contain it.

### Preview images

App-managed and content-addressed (§18), not referenced in place — an in-place
reference breaks the moment a file moves, which is the precise failure this
project exists to eliminate. Bytes live in a blob directory beside the database
so the database stays small enough to copy.

MIME is sniffed from magic bytes, never trusted from a filename, and anything
that does not sniff as a known image is stored as an opaque stream. A preview
served as `text/html` would execute in the UI's own origin.

---

## Search

FTS5, which ships in SQLite, so full-text search costs no extra service.

- **User text never reaches `MATCH` raw.** FTS5 has its own operator syntax; a
  stray quote or a bare `AND` would be a syntax error rather than a search. Every
  token is quoted, and the last gets a prefix wildcard so results appear as you
  type.
- **Filenames are indexed with separators as spaces**, so searching `cinematic`
  finds `cinematic_style_v2.safetensors`. Most of a real library has no metadata
  yet and the filename is all there is.
- **Multiple tags narrow, not widen.** Any-match would make tags useless at this
  scale.

Paging breaks ties on `sha256`, so a page boundary cannot repeat or skip a row
when two records share a sort key. The index refreshes on every resolve, so a
value edited in the UI is findable immediately.

`mm reindex` rebuilds both derived layers. Both are recomputable, so it is always
safe and never loses anything.

---

## Install detection

§3 makes zero-config first run a hard requirement — "adoption dies at the setup
screen" otherwise. `mm detect` finds ComfyUI, SwarmUI, Stability Matrix, A1111,
Forge, InvokeAI and Fooocus.

Every detector requires a **positive marker**. A directory named `ComfyUI` that
holds something else is not ComfyUI, and a bare `Models` directory is not
SwarmUI. Forge is tested before A1111 because it satisfies every A1111 marker as
well as its own.

ComfyUI's `extra_model_paths.yaml` is parsed, since it is frequently the only
place the real library is named.

The search is deliberately shallow. Walking a whole home directory would wander
into network mounts and turn first run into an unexplained multi-minute pause —
the same setup-screen failure by another route. `MM_SEARCH_PATHS` overrides it.

Detected roots are collapsed before being offered: two tools sharing a model
directory is normal, and the scanner rejects overlapping roots outright.

---

## The API and its hardening

`mm serve`. REST, documented at `/openapi.json`.

The §11 baseline, which the spec calls non-negotiable for a public build:

- **Binds `127.0.0.1`.** Any other interface is an explicit opt-in.
- **Strict Host allowlist.** This is the DNS-rebinding defense: a browser tab on
  any site can resolve a name it controls to `127.0.0.1` and reach a localhost
  server, so the connection being local proves nothing. Only the Host header
  distinguishes the attack.
- **No CORS wildcard, and no option for one.** A wildcard on an API that can read
  every model file lets any page on the internet enumerate the library.
  Same-origin requests are always allowed — browsers send `Origin` on plenty of
  same-origin requests, including anything marked `crossorigin`, which is what
  Vite emits for its own assets.
- **Bearer token off-loopback**, compared in constant time. On loopback it is
  pointless: anything that can reach the port can read the token file.
- **Trusted CIDRs** (`--tailnet`) exempt the token, per §11's allowance for an
  already-authenticated tailnet. Trust is decided from `RemoteAddr` only —
  `X-Forwarded-For` is attacker-controlled unless a trusted proxy set it, and
  this daemon is not behind one.

The token is **injected into the served page**, not only written to a file. §11
is explicit that the file copy exists for CLI and third-party clients: a browser
cannot read a file off disk, and any design assuming it can is unimplementable.
The page carrying a credential is served `no-store` under a same-origin CSP.

**Read-only by default.** Phase 1's contract is that the index is proven before
anything acts on it; `--writable` makes that a decision rather than an accident.

---

## The UI

React 18 / TypeScript / Vite, compiled to static assets and embedded via
`embed.FS`. The built output is committed so `go build` works on a machine with
no Node toolchain, which §3 requires of a distributable tool. `make ui` rebuilds
it.

- Dark-first, because this sits beside uniformly dark generation UIs.
- Facets sort by frequency, not alphabetically — in a 19k library the long tail
  of one-off values is noise, and A–Z buries the buckets that partition it.
- Trigger words are one-tap copy chips, with a clipboard fallback for plain HTTP,
  which is exactly how this is reached over a tailnet (use case 5).
- The provenance view shows losing candidates beside the winner, so a surprising
  value is explainable.
- Sidebar and detail become full-screen overlays below 900px, since phone and
  iPad browsing is a primary use case rather than an afterthought.
