# Changelog

This file lists changes that a user or operator would notice. It does not
list internal refactors with no visible effect.

Every entry links to the full release notes for that version.

## [v0.5.0](docs/release-notes/v0.5.0.md) — 2026-08-07

### Added

- Upstream libraries. Point one Model Manager at another with `MM_UPSTREAM_URL`
  and it appears in Browse beside Civitai, CivArchive and HuggingFace. This is
  how a laptop reaches a collection that lives on a NAS.
- Pulling a model from an upstream onto the local machine. It lands in the right
  per-type folder for whichever tool owns that directory, is verified against the
  hash the upstream indexed it under, and is indexed on arrival, exactly like a
  download from a provider.
- A pulled model brings the upstream's metadata with it: name, base model,
  trigger words, tags, previews, the training record, and the provenance behind
  them. A value corrected by hand on the upstream stays a manual value here, so a
  later metadata refresh cannot overwrite it. Only the upstream needs a provider
  API key.
- `--serve-files` on the machine holding the library, which lets other machines
  download indexed model files from it. Off by default. Transfers support resume.
- Evicting a pulled copy to reclaim space, behind `--allow-evict`. The file is
  removed; everything the library knows about it — name, tags, previews,
  provenance, your own edits — is kept, and the model stays listed as available
  from the upstream. Refused for any file that was not pulled, and for any model a
  generated view still links to, where the delete would have freed nothing.
- A Settings panel reporting whether the upstream is configured, reachable,
  accepting our credentials, and serving files, since those fail separately.

- Archiving a model from a provider, behind `--allow-archive`. It keeps the file,
  the provider's raw responses, every metadata candidate and every preview, so a
  model taken down later is still fully here. Intake is per model and deliberate;
  nothing is crawled.
- A watchlist. Watched models are checked for new versions on a timer
  (`--archive-interval`, six hours by default, `0` to check only on request).
  Fetching a new version automatically is off by default: a watch subscribes to
  information, not to unattended multi-gigabyte downloads.
- The archive records how complete each capture is, part by part, so a partial
  one says which part is missing and can be finished by re-running — that only
  fetches the gaps.
- A model the provider has removed is marked as gone and stops being re-checked,
  and its record is shown as the surviving copy. Before this, a model that 404'd
  once was re-asked on every update check for the life of the library.
- A downloaded file is now read once instead of twice. It used to be hashed to
  verify it and then read again in full to record it, so a 12 GB model cost 24 GB
  of reading — on the smallest disks, for the largest files. The published copy is
  also verified now, which the second read had been doing by accident.
- Downloads now check free space before starting. A transfer that cannot fit is
  refused in the first second with both numbers, instead of filling the disk and
  leaving a partial file behind. Both filesystems are checked: a download is
  staged beside the database and only then moved to the model root, so on the
  usual setup it needs room in two places at once.

### Fixed

- Preview images from a source that requires a bearer token failed to load. The
  image proxy sent no credential, which no public provider needed and an upstream
  library does.
- A model pulled into two model directories could never be evicted. The second
  pull replaced the record of the first, so the first copy no longer matched any
  record and was refused with a message saying it had not been pulled. Each copy
  is now recorded and evicted on its own.
- Evicting a model while it was being downloaded is now refused, naming the
  transfer, instead of deleting a file something was still writing.
- Resuming an interrupted download now tells the server which version of the file
  it already has. A file that changed mid-transfer restarts cleanly instead of
  producing a mixture of the old and new copies that only fails at the end.
- Starting an enrichment run while an update check was running is now refused, as
  the reverse already was. Both talk to the same provider on one shared rate
  limit, and running them together was how that limit got hit.
- Browse now says when a provider could not be reached for file details, instead
  of showing those results as "new". A model you already own could be listed as
  new during an outage, which invited downloading it again.
- An enrichment run now reports how many preview images failed, separately from
  models that failed. A run with no network reported zero images and no errors,
  which read as a library whose models simply have no previews.

## [v0.4.2](docs/release-notes/v0.4.2.md) — 2026-08-05

### Fixed

- The per-type download folder fields in Settings lost focus after every
  character typed, making them effectively unusable. They now save when you
  leave the field, like every other text setting on that panel.

## [v0.4.1](docs/release-notes/v0.4.1.md) — 2026-08-04

### Fixed

- A fresh install with no directories added showed a "Nothing matches" search
  result with CLI instructions. It's now a setup screen with a button onto
  Settings, where directories have always been addable.
- The owned-version chip in Browse's version picker rendered as garbage
  characters instead of a check mark, from an escaping bug in the v0.4.0
  build.

## [v0.4.0](docs/release-notes/v0.4.0.md) — 2026-08-04

### Added

- Version grouping, in both Browse and the library. Browse groups versions of
  the same model into one card with a version picker; the library collapses
  several installed versions of one model into a single card that says how many
  it stands for. Searching for a popular model used to return eight cards with the
  same name, because each version is its own search result. By default,
  versions built for different base models stay apart, since one is not a
  drop-in replacement for the other; Settings can widen this to group them all,
  or turn grouping off. Bulk actions still cover every version, not just the
  one shown.
- The library now marks models that have a newer version upstream, with a
  "Needs update" filter beside the other filters. The mark says which version
  you have, which one is newer, and when the check ran.
- Update checks now run in the background and are saved. Before, the check ran
  while you waited and the answer was lost when you left the tab.
- A Settings control for the thumbnail shape. Previews are nearly always
  portrait, so grids are now 3:4 by default instead of square, which used to
  crop the top and bottom off most images. Tall (2:3) and square are also
  available.
- Adult results in Browse are now on by default and remembered. The control
  moved from a checkbox that reset on every reload to the Settings tab.

### Fixed

- Turning on a filter no longer pushes the rest of the sidebar down, and the
  type row above the library no longer changes height.
- The three buttons on a Browse result no longer wrap onto a second line.
- A Browse result with no preview image showed as a bare block of text. It now
  gets a placeholder tile, like the library already did.
- The "nsfw" tag on a Browse result was drawn as unreadable pink text on a
  purple block, and the "yours" tag on a preview image floated over the picture
  instead of sitting under it.
- The library, Browse and Settings tabs started their content at three
  different places, so switching tabs shifted what you were reading.

## [v0.3.3](docs/release-notes/v0.3.3.md) — 2026-08-04

### Fixed

- Every thumbnail in Browse was broken. Civitai's image host now redirects
  to a CDN host this daemon did not recognize, so the proxy refused it. The
  image and download allowlists now match by domain instead of by exact
  hostname, so a provider moving its CDN again will not need a code change
  to keep working.

## [v0.3.2](docs/release-notes/v0.3.2.md) — 2026-08-03

### Fixed

- The "Refresh from origin" button on a model showed even when the daemon
  was started with `--no-remote`, and pressing it always failed. It now
  hides itself in that case.
- A 409 for a model whose file just is not on disk said its hash was
  unconfirmed. It now says the file is not present, instead of sending you
  to a command that cannot help.
- Refreshing one model from its detail panel fetched the model's full
  record twice. It now fetches it once.
- The library toolbar and Settings each ran their own poll for sweep
  progress, and an error shown on one could outlive the run it was about.
  There is now one shared poll, and it clears itself once it has current
  truth.
- A finished sweep's summary always read like it had just happened. It now
  says how long ago it ran.

## [v0.3.1](docs/release-notes/v0.3.1.md) — 2026-08-03

### Fixed

- A stopped or rate-limited sweep used to report itself as 100% done. It now
  reports the true number of models it reached.
- A sweep the origin rate-limited looked the same as a finished sweep. The
  daemon now marks it "rate limited" so you know to run it again.
- Live counts (matched, missing, images, errors) stayed at zero until a sweep
  finished. They now update while the sweep runs.
- The single-model "Refresh from origin" button used to scan the whole
  library first. It now looks up that one model directly.

## [v0.3.0](docs/release-notes/v0.3.0.md) — 2026-08-03

### Added

- A "Refresh from origin" button. One is on each model's panel, one is on the
  library toolbar for the current filter, one is in Settings for the whole
  library.
- The refresh follows the same rules the library already used. A value you
  typed stays. A blank field gets filled in. A thumbnail you picked stays
  first.

## [v0.2.0](docs/release-notes/v0.2.0.md) — 2026-07-30

### Added

- A Settings tab to add, disable, rescan, and forget model directories.
- Scanning from the UI, with live progress.
- Per-directory, per-model-type download folders. Downloads used to guess one
  shared folder name. They now match the destination tool's own layout.
- Saved library filters. The view you set on the desktop now matches the view
  on the phone.
- Custom thumbnails. Upload a file, pick one from a ComfyUI output folder, or
  render one from a saved ComfyUI workflow.
- Split base-model families (Flux.1, Flux.2, Krea 2, Anima, SDXL) instead of
  one shared "Flux" bucket.

### Fixed

- An unknown model type used to get its own folder, wrongly pluralized (for
  example "vaes"). It now uses the destination directory instead.

## v0.1.0 — 2026-07-30

First release.

### Added

- One static binary. No external services, no configuration.
- Content-based identity. Every model is keyed by its SHA256 hash, not its
  path or filename.
- A provenance engine. It ranks each value by source, and a value you typed
  stays ahead of anything a tool or the origin suggests later.
- Header interpretation, ingest from other tools' sidecars, search, and a web
  UI.
- Civitai and HuggingFace enrichment. Every response is archived.
- Resumable downloads.
