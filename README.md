# Model Manager

An independent metadata authority and organization layer for a local AI model
library.

Four tools — ComfyUI, SwarmUI, Stability Matrix, Civitai — each maintain their own
metadata sidecars and each believe they own them. Every one of them binds
metadata to the model by *path and filename*: `model.safetensors` → `model.json`.
That link is positional, not intrinsic. Move a file, rename it, or let one tool
rewrite a sidecar another tool owns, and the binding is gone — nothing in the JSON
knows which safetensors it belonged to.

**This project binds metadata to file content instead.** The primary key is the
SHA256 of the model file; path becomes a mutable attribute.

- File moves between roots → hash unchanged → metadata still attached
- Same model in two places → one record, two paths
- A tool scrambles its sidecar → regenerate it from master
- A tool rewrites a header in place → the weights-region hash rebinds the record

Civitai indexes by SHA256 too, so the key that identifies a file locally is the
same key used to enrich it.

See [`model-manager-spec.md`](model-manager-spec.md) for the full design.

## Status

All phases implemented. One static binary, no services, no configuration.

| Phase | What | Docs |
|---|---|---|
| 0 | Hash pass: dual hashes, headers captured verbatim, SQLite index | [phase0](docs/phase0.md) |
| 1 | Provenance engine, header interpretation, ingest, search, API, UI | [phase1](docs/phase1.md) |
| 2 | Civitai/HF enrichment and archive, resumable downloads | [phase2](docs/phase2.md) |
| 3 | Link-strategy engine and generated views | [phase3](docs/phase3.md) |
| 4 | SSD tiering | [phase4-5](docs/phase4-5.md) |
| 5 | Sidecar projection | [phase4-5](docs/phase4-5.md) |
| 6 | Remote browsing across Civitai/CivArchive/HuggingFace, update checking, downloads from the UI | [phase6](docs/phase6.md) |

## Build

```sh
make build          # -> bin/mm
make test
make ui             # rebuild the web UI (needs Node; the built output is committed)
make release        # cross-compiled matrix -> bin/mm-<os>-<arch>
```

Go 1.24+. No cgo, no external toolchain, no service to run — the SQLite driver is
pure Go, so `GOOS`/`GOARCH` builds every target from one machine.

## Quick start

```sh
mm detect                                    # find your existing tools
mm bench  --root /path/to/models             # measure the array before choosing concurrency
mm scan   --root /path/to/models --workers 4 # hash everything
mm interpret                                 # headers -> typed metadata (no disk reads)
mm ingest                                    # read other tools' sidecars
mm report                                    # distinct models, duplication, size spread
mm serve                                     # browse at http://127.0.0.1:8737
mm serve --writable                          # ...and download from the Browse tab
```

Then, once the index is proven:

```sh
mm enrich                                    # Civitai lookup by hash, archived forever
mm browse "neon ink" --type lora             # search three providers, marked have/update/new
mm updates                                   # which models have a newer version
mm view create --name by-base --root /views/by-base --group-by base_model
mm view generate by-base                     # organize without moving a byte
mm project --target stability-matrix         # write sidecars back out
```

## Commands

| Command | Does |
|---|---|
| `serve` | HTTP API and web UI |
| `scan` | Walk roots, hash, record raw facts |
| `interpret` | Turn stored headers into typed metadata (reads no model files) |
| `ingest` | Read other tools' sidecars (read-only) |
| `enrich` | Look models up on Civitai by hash and archive the response |
| `browse` | Search Civitai, CivArchive and HuggingFace; marks what you already have |
| `updates` | Report which models have a newer version published |
| `get` | Download a model, resumable and checksum-verified |
| `view` | Define and generate organized views, non-destructively |
| `tier` | Stage hot models onto fast storage |
| `project` | Write master metadata back out as tool sidecars |
| `link-probe` | Report which link mechanisms work between two directories |
| `detect` | Find installed SD tools and their model roots |
| `reindex` | Rebuild the search index and re-resolve every record |
| `report` | Distinct models, duplication, size distribution |
| `verify` | Re-read files and check the index against the disk |
| `bench` | Compare hashing throughput at different worker counts |

## Guarantees

- **Never modifies, moves, renames, or deletes an existing model file.** Model
  files are opened read-only. A download that lands on a name already taken is
  given a new name rather than replacing what is there. The files this tool
  creates are the ones you asked for — downloads at a destination you chose,
  view entries, tier copies, and sidecars — plus its own state: the SQLite
  database beside it, the preview-image store written by `enrich`, and transient
  partial-download and probe files that are cleaned up after use.
- **Reports duplicates, never deletes them.** Surfacing them is the feature.
- **Manual metadata is never overwritten by any ingest.** When an origin later
  disagrees, that surfaces as a suggestion with one-click accept, not a silent
  replacement.
- **Offline except where you ask it to reach out.** Four commands make network
  requests — `enrich`, `browse`, `updates`, and `get` — along with the daemon's
  `/api/browse`, `/api/updates` and `/api/remote-image` endpoints, which
  `mm serve --no-remote` disables outright. Everything else works with no
  network at all.
- **Binds `127.0.0.1` by default**, with a Host allowlist against DNS rebinding
  and no CORS wildcard. A token is required off-loopback.
- **The database is a single file** you can copy, which is what makes the
  declared sole authority backable.

## Layout

```
cmd/mm                  CLI
internal/store          SQLite schema, migrations, the only code that writes it
internal/hashing        Dual-hash streaming pass and the sampled probe
internal/modelformat    safetensors / GGUF framing; locates the weights region
internal/scan           Root walking, cache tiers, per-device workers
internal/provenance     Which of several competing values for a field wins
internal/interpret      Stored headers and paths -> typed observations
internal/ingest         Other tools' sidecars, read-only
internal/origin         Civitai / CivArchive / HuggingFace: hash lookup, permanent
                        archive, remote search, and update detection
internal/download       Resumable, verified, quarantined transfers
internal/link           Reflink / block-clone / symlink / hardlink / copy
internal/view           Generated directory trees over the library
internal/tier           Staging onto fast storage
internal/project        Master -> tool sidecar dialects
internal/api            HTTP API and the §11 security baseline
internal/webui          The embedded front-end (built assets committed)
web/                    React / TypeScript / Vite source
```
