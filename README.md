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
- A tool scrambles its sidecar → rehash, look it up, regenerate
- Orphaning becomes recoverable for anything previously seen

Civitai indexes by SHA256 too, so the key that identifies a file locally is the
same key used to enrich it.

See [`model-manager-spec.md`](model-manager-spec.md) for the full design.

## Status

**Phase 0 is implemented.** A standalone scanner that walks model roots, hashes
everything, captures format headers verbatim, and writes raw uninterpreted facts
to a single SQLite file.

Phase 0 deliberately interprets nothing — no typed metadata fields, no enrichment,
no UI, no writes outside its own database. The facts it records stay valid no
matter what Phase 1 decides about the schema, so a schema change never costs a
re-hash of 7.5TB.

Later phases — read-only index with UI and API, enrichment and download,
presentation layer, SSD tiering, sidecar projection — are described in §15 of the
spec.

## Build

```sh
make build          # -> bin/mm
make test
make release        # cross-compiled matrix -> bin/mm-<os>-<arch>
```

Go 1.24+. No cgo, no external toolchain, no service to run — the SQLite driver is
pure Go, so `GOOS`/`GOARCH` builds every target from one machine.

## Quick start

```sh
mm bench --root /path/to/models --workers 1,2,4   # measure the array first
mm scan  --root /path/to/models --workers 4       # hash everything
mm report                                          # distinct models, duplication
mm verify --sample 25                              # prove the index against disk
```

The scan commits per file, so an interrupt resumes rather than restarting, and a
rescan of an unchanged tree costs a stat pass rather than a hash pass.

Full documentation: [`docs/phase0.md`](docs/phase0.md).

## Guarantees

- **Never modifies, moves, renames, or deletes a model file.** Model files are
  opened read-only. Phase 0 creates nothing at all inside the model tree.
- **Reports duplicates, never deletes them.** Surfacing them is the feature;
  pulling the trigger is not.
- **Fully offline.** Phase 0 makes no network requests of any kind.
- **The database is a single file** you can copy, which is what makes the
  declared sole authority backable.

## Layout

```
cmd/mm                  CLI: scan, report, verify, bench
internal/store          SQLite schema, migrations, the only code that writes it
internal/hashing        Dual-hash streaming pass and the sampled probe
internal/modelformat    safetensors / GGUF framing; locates the weights region
internal/scan           Root walking, cache tiers, per-device workers
internal/report         Distinct-model, duplication and size-distribution figures
internal/verify         Re-reads files and checks the index against the disk
internal/bench          Hashing throughput at different worker counts
```
