# Model Manager

An independent metadata authority and organization layer for a local AI model library.

Metadata binds to **file content, not to path**. The primary key is the SHA256 of the model
file; path is a mutable attribute. Move a file, rename it, or let another tool scramble its
sidecar — the metadata stays attached, because nothing about the binding was positional.

See [`model-manager-spec.md`](model-manager-spec.md) for the full design.

## Status

**Phase 0 — in progress.** A standalone scanner that walks model roots, hashes everything,
captures format headers verbatim, and writes raw uninterpreted facts to a single SQLite file.

Phase 0 deliberately does *not* interpret anything. No typed metadata fields, no enrichment,
no UI, no writes outside its own database. The facts it records stay valid no matter what
later phases decide about the schema, so a schema change never costs a re-hash of 7.5TB.

Later phases (read-only index + UI + API, enrichment and download, presentation layer, SSD
tiering, sidecar projection) are described in §15 of the spec.

## Build

```sh
make build          # -> bin/mm
make test
make release        # cross-compiled matrix -> bin/mm-<os>-<arch>
```

Requires Go 1.24+. No cgo, no external toolchain — the SQLite driver is pure Go.

## Usage

See [`docs/phase0.md`](docs/phase0.md).
