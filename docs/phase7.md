# Phase 7 — Directories, download folders, sticky filters, thumbnails

Four asks, all of them about controlling the library rather than reading it:
point the app at more folders, decide where downloads land, keep a filtered view
between sessions, and choose your own thumbnails.

## Directories are now first-class

Before this phase the set of model roots was whatever
`SELECT DISTINCT root FROM model_file_path` returned. That works only for a root
that already contains indexed files, which makes "add a directory" impossible to
express: a folder you have not scanned yet holds nothing, so it does not exist.

Roots are now a table (`model_root`), and the UI has a **Settings** tab that
adds, disables, rescans and forgets them.

    GET    /api/roots
    POST   /api/roots            add (canonicalized, overlap refused)
    PATCH  /api/roots/{id}       enable/disable, label, folder layout
    DELETE /api/roots/{id}       forget

### Why canonicalization is correctness, not tidiness

`SweepAbsentPaths` matches `WHERE root = ?` **exactly**. Two spellings of one
directory — a trailing slash, a symlink not resolved, a relative path — fork the
root, and every file under the fork stays `present = 1` forever no matter what
the disk says.

So `AddRoot` runs `filepath.Abs` + `Clean` + `EvalSymlinks` before storing, and
that stored string is the one the indexer writes into `model_file_path.root`.

Symlink resolution matters beyond hygiene here. The stated direction of travel
for this library is a single master root symlinked across machines; if one
machine records the link and another records the target, the two databases
disagree about a directory that is in fact the same, and neither sweep is right.

### Overlap is refused, not merged

Adding a root that nests inside a managed one — or that contains one — is a
409, not a merge. The present-sweep is scoped per root, so a file reachable
under two roots gets swept by whichever root did not claim it, and `present`
flaps on every scan. Refusing up front beats an index that quietly disagrees
with the disk.

### "Forget" never touches the disk

Removing a root marks its paths absent and deletes the root row. The
`model_file` records and every field observation, tag, preview and training note
stay. Re-adding the folder later restores the library rather than re-deriving it
from scratch.

This is the standing guarantee applied to a new operation: *the app never
modifies, moves, renames or deletes an existing model file*.

## Scanning from the UI

`scan.Run` was always cancellable and always reported progress through a
callback — it was simply only ever called from the CLI. `internal/scanjob`
wraps it in the job shape the download manager already established: register
synchronously so the ID goes back in the 202, run detached, poll for progress,
cancel by ID.

    POST   /api/scans          {"roots": []} means every enabled root
    GET    /api/scans/active
    DELETE /api/scans/{id}

**One scan at a time.** Two concurrent scans of overlapping trees contend on the
single SQLite writer for no gain, and if the trees do overlap they fight over
`present` — the same reason nested roots are refused.

A cancelled scan is not a lost scan: everything committed stays committed, and
`last_scanned_at` is stamped only for roots the run actually completed, because
a half-walked root must not look freshly verified.

`mm scan --root DIR` now registers what it scanned as a managed root too, so a
folder first seen on the command line is the same first-class thing as one added
from the UI.

## One vocabulary of model types

`internal/modeltype` is the single canonical set: checkpoint, lora, lycoris,
embedding, vae, controlnet, upscaler, hypernetwork. `Normalize` maps every
spelling the tools and providers use onto it, and **an unrecognised value
returns `""`, never itself**.

That last rule is the bug this package exists to close.
`civitaiTypeToLocal` used to end `return strings.ToLower(t)`, which was harmless
while a type was only displayed — but the type reaches the download path, where
the browse UI pluralized it into `${type}s` and used it as a *directory name*.
That already produced `vaes/` and `lycoriss/`, and an unmapped Civitai type
would have created a folder named after whatever Civitai invented next.

Unknown type now means "no subfolder", and no subfolder means the root itself.
Falling back to the root is safe; fabricating a directory from an unvalidated
string is not.

## Download folders are per (root, type)

The finding that shaped this. On the machine this was built against, three tools
name the same kind of folder three different ways:

| type       | Stability Matrix  | SwarmUI            | ComfyUI       |
|------------|-------------------|--------------------|---------------|
| lora       | `Lora`            | `Lora`             | `loras`       |
| checkpoint | `StableDiffusion` | `Stable-Diffusion` | `checkpoints` |
| embedding  | `TextualInversion`| `Embeddings`       | `embeddings`  |

So there is no such thing as "the folder for loras". A destination can only be
decided by the pair **(root, type)**, because the root is what carries the
vocabulary. A single global map would put files where two of the three tools
would never look.

Resolution order, server-side:

1. the user's configured map (`downloads.folder_map`, editable per root in
   Settings),
2. the built-in table for whatever tool the root belongs to,
3. nothing — the root itself.

A root added without a tool gets one inferred from **its own contents**:
`StableDiffusion/` means Stability Matrix, `Stable-Diffusion/` SwarmUI,
`checkpoints/` ComfyUI. Decided from the directory rather than from install
detection, because what matters for placing a file is the convention *inside
this folder* — someone who points the app at a directory they populated by hand
should get their own layout continued.

    GET /api/downloads/destination?root=&type=
    GET /api/downloads/folder-defaults

The Browse tab shows the resolved path before you press Download. A destination
you cannot see is a destination you cannot object to.

Configured folders are still joined onto a model root, so they go through the
same traversal stripping and post-symlink containment check every other
subdirectory does.

## Filters persist, and the counts are honest

Saved filters live in the `setting` table, not `localStorage`. The same daemon
answers the desktop browser and the phone over the tailnet, and a view
configured on one should be the view on the other — consistent with the
database being the single authority.

A type-chip row sits under the top bar for one-click visibility toggling; the
sidebar keeps the full facet lists.

**A bug fixed on the way:** `FacetCounts` took no query, so the counts described
the whole library while the list beside them described a filtered subset — the
sidebar could say "lora 412" next to twelve results. It now takes the same
`SearchQuery` the search does, sharing one `filterSQL` builder so the two cannot
drift. Each dimension is counted with its own filter lifted, which is what lets
a second value be added to a facet already narrowing the search.

`Facets` gained `library_total` alongside `total`, because once `total` became
filter-aware there was nothing left to distinguish *"nothing matches"* from
*"there is nothing here yet"* — and those need different advice.

## Thumbnails

### Sticky was already true; the missing half was yours

Fetched previews have always been copied into the content-addressed blob store
at enrich time, so a Civitai deletion cannot reach back and blank a local
thumbnail (spec §18). What was missing was a picture *you* chose and a guarantee
that enrichment cannot displace it.

    POST   /api/models/{sha}/previews             raw image body
    POST   /api/models/{sha}/previews/generated   {"rel": "..."}
    PUT    /api/models/{sha}/previews/order
    DELETE /api/models/{sha}/previews/{image}
    GET    /api/models/{sha}/previews/{image}/workflow

Uploads are stored with source `manual`, which outranks every fetched source in
the display order — the same tiering the field provenance uses. The upsert also
refuses to demote: re-ingesting the same bytes from a provider leaves the source
as `manual`.

The body is raw image bytes and the **type is sniffed, never taken from a header
or a filename**. These bytes are served back from the UI's own origin, where an
HTML file that renders is XSS.

Detaching a preview leaves the blob in place. Blobs are content-addressed and
shared, so deleting the bytes would blank the same picture on every other model
using it.

### The workflow comes with the picture

A ComfyUI render carries the graph that produced it in a PNG `tEXt`/`iTXt`
chunk. `internal/thumb` walks those chunks with the stdlib only — PNG is a flat
sequence of length-prefixed records, and no decoder is needed to read one — and
stores the JSON as its own blob beside the image. It comes back as a download,
so it can be dropped straight into ComfyUI.

Stored separately from the image so it survives the image being replaced.
`zTXt`/compressed `iTXt` are inflated through a bounded reader: these bytes come
from an upload, and an unbounded decompress is a zip bomb.

### Pick from a render

Set a ComfyUI output folder in Settings and `GET /api/generated` lists recent
images from it, newest first. Only that one configured directory is readable,
the walk is depth-bounded, and a requested path has traversal segments stripped
and containment re-checked after symlinks — the same shape as the download
destination check, because this is the same kind of thing: a client-supplied
path used to touch the local filesystem.

### Fast, measured

The user called grid performance "a precaution for scale", so it was measured
rather than assumed. Every preview now gets a derived copy capped at 512px on
the long edge, stored alongside the full image; the grid query serves the
thumbnail when there is one and the full image otherwise.

On the fixture used to test this — a 1600×1200 render:

| | bytes |
|---|---|
| full preview | 692,662 |
| derived thumbnail | 60,075 |
| **ratio** | **11.5×** |

A 60-card page of previews that size is **41 MB against 3.6 MB**. Both carry
`Cache-Control: public, max-age=31536000, immutable`, which is free given the
URL is a content address.

Scaling is a box filter (each destination pixel averages the source pixels it
covers) rather than nearest-neighbour, which looks visibly wrong on the diagonal
detail these images are full of, and rather than a Lanczos resampler, which
would need a dependency in a project whose whole point is one static binary.

An image already under 512px gets no thumbnail: storing a near-identical second
copy would double the blob store for nothing.

    mm thumbs

backfills every preview ingested before this existed. Purely additive, safe to
interrupt, and it reports the bytes saved.

## What is not built

**Submitting a workflow to ComfyUI to render a preview.** That is the one item
that would make ComfyUI a *runtime dependency* rather than a file format we
read, and it needs the topology settled first — the ComfyUI here is portable and
also driven by SwarmUI, which owns its lifecycle — plus a queue/poll client and
sane behaviour when ComfyUI is not running. Scoped, not started.

## Schema

Migrations 4 and 5, both append-only as always.

- **4** — `model_root` (canonical path, label, tool, enabled, timestamps),
  seeded from `SELECT DISTINCT root FROM model_file_path` so an existing library
  keeps working with no user action; `setting` (JSON key/value), so a new
  preference is a write rather than a migration.
- **5** — `preview_image.thumb_sha256` and `preview_image.workflow_sha256`, both
  content addresses into the blob store.
