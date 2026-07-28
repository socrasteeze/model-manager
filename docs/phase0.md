# Phase 0 — hash pass

Phase 0 is a standalone Go binary that walks model roots, hashes everything,
captures format headers verbatim, and writes raw uninterpreted facts to a single
SQLite file. Nothing else.

Its output answers one question: **how many distinct models exist across the
library, and how much of its size is duplication.** That number decides whether
this is a 3,000-row problem or a 19,000-row one, sizes the SSD tier, and surfaces
the duplicate situation for free.

See [`../model-manager-spec.md`](../model-manager-spec.md) for the design this
implements. Section references below (§2.1, §10.1, …) point into it.

---

## Install

```sh
make build              # -> bin/mm
make release            # cross-compiled matrix -> bin/mm-<os>-<arch>
```

Go 1.24+. No cgo, no external toolchain, no service to run.

---

## Use

### 1. Measure the array before choosing concurrency

```sh
mm bench --root /Volume1/ai/models --workers 1,2,4,8
```

Spec §17 asks for this explicitly, and asks for it *before* assuming parallelism
wins. On spinning rust, concurrent readers seek-thrash and frequently lose to
sequential large-buffer reads. `bench` hashes a real sample at each worker count
and writes to no database.

Read the page-cache caveat it prints. A sample that fits in RAM measures the page
cache on every run after the first, which makes higher worker counts look free.

### 2. Scan

```sh
mm scan --root /Volume1/ai/models --workers 4
```

- Commits per file. An interrupted scan resumes; it never restarts.
- `Ctrl-C` once finishes in-flight files and closes out cleanly. Twice force-quits.
- A rescan of an unchanged tree costs a stat pass, not a hash pass.
- Multiple `--root` flags are allowed. Nested roots are rejected — see
  [Overlapping roots](#overlapping-roots).

Useful flags:

| Flag | Default | Notes |
|---|---|---|
| `--db` | OS config dir | Must be on a local disk (§10.5). |
| `--workers` | 1 | Per storage device, not global. |
| `--probe` | off | Sampled-probe fast path. See [The probe](#the-probe-and-provisional-paths). |
| `--buffer-mib` | 4 | Read chunk per worker. |
| `--max-header-mib` | 8 | Cap on stored header blobs. |

### 3. Report

```sh
mm report                 # human-readable
mm report --json          # machine-readable
```

### 4. Verify

```sh
mm verify --sample 25            # spot-check the index against disk
mm verify --sample 0             # check everything
mm verify --provisional          # confirm every probe-bound path by full hash
```

Exits non-zero if any file's contents disagree with the index, so it can gate
later phases. It corrects the index to match the disk as it goes — reporting a
known-wrong record without fixing it would be worse than not checking.

---

## What Phase 0 records

**Raw uninterpreted facts only.** No typed metadata fields, no enrichment, no
parsing of header contents.

That constraint is the point. The facts below stay valid no matter what Phase 1
decides about the schema, so a schema change never costs a re-hash of 7.5TB.
Interpreting the stored headers into typed fields later is a cheap, re-runnable
pass over blobs already in the database.

### `model_file` — one row per distinct content

| Column | Notes |
|---|---|
| `sha256` | Primary key. Also the Civitai lookup key (§2). |
| `weights_sha256` | Tensor-region hash. **Nullable** — see below. |
| `weights_offset` | Where the weights region begins. |
| `probe_sha256` | Sampled hash over the first and last 1MiB. |
| `size`, `format` | |
| `header_blob` | Captured **verbatim, uninterpreted**. |
| `header_offset`, `header_truncated` | |
| `first_seen`, `last_verified` | |

### `model_file_path` — one row per observed location

`sha256`, `path`, `root`, `device`, `inode`, `size`, `mtime_ns`, `first_seen`,
`last_seen`, `present`, `provisional`, `scan_run_id`.

### `scan_run`, `scan_error`

One row per walk of one root, with counters; errors recorded rather than
swallowed.

---

## The things that are easy to get wrong

### `weights_sha256` is nullable, and NULL means *absent*

Content-addressing assumes the file is immutable. It is not: some tools rewrite
the safetensors metadata header in place. Identical weights, different SHA256,
record silently orphaned (§2.1).

The mitigation is a second hash over the tensor region only, computed in the same
streaming pass. When a header is rewritten, `sha256` changes and
`weights_sha256` does not — so the orphaned record can be found and rebound.

Where it comes from, per format:

| Format | Weights region | `weights_sha256` |
|---|---|---|
| safetensors | 8-byte LE header length → JSON header → tensor bytes. Hashed from the end of the header onward. | Populated |
| GGUF (v2/v3) | Data offset is not stored in the file; every KV pair and tensor info is walked, then rounded up to `general.alignment`. | Populated |
| GGUF v1 | 32-bit layout. **Declined** — walking it with the v2 reader yields a plausible but wrong offset, and a wrong rebinding key is worse than none. | NULL |
| `.ckpt`, `.pt`, `.bin` | Python pickle. Parsing it is arbitrary code execution on files sourced from the internet (§10.4). Never parsed. | NULL |
| Any file whose framing fails to parse | | NULL |

**Any code that rebinds by `weights_sha256` must treat NULL as "no rebinding key
available for this file", never as a populated column.** The NULL rows are
precisely the formats least able to recover by any other means, so misreading
them misbehaves exactly where it hurts most.

An empty weights region also yields NULL rather than the digest of zero bytes,
which would otherwise be identical across every such file.

### The cache is keyed on inode, not path

`(device, inode, size, mtime)` — deliberately **not** `(path, size, mtime)`.

The premise of the whole design is that paths churn. A path-keyed cache misses on
every migrated file, which is the exact workload the cache exists to avoid. A
move within a filesystem preserves the inode, so it costs nothing (§10.1).

### The probe, and provisional paths

`--probe` enables a second-tier fallback for cross-volume copies: a new inode
with a familiar size, matched on a sampled hash of the first and last 1MiB.

**A probe match never confers identity.** A sampled hash over a multi-GB file is
a far weaker guarantee than a full one, and a false positive in a
content-addressed system assigns a wrong identity *permanently*. So a probe match
binds the path as `provisional = 1`, and provisional paths are:

- usable for browsing,
- **never** a cache hit,
- **never** the basis for projection, dedup reporting, tiering, or any write-side
  decision until `mm verify --provisional` confirms them by full hash.

The probe is **off by default**, because Phase 0's entire output is a
distinct-hash count that should be measured rather than inferred. Turn it on for
a rescan after a cross-volume migration, not for the first pass.

### Scanning during a migration is safe

A file copied into place mid-walk can be hashed partially written, and that wrong
hash would be committed as a permanent identity (§10.2).

Every hash re-stats through **the same descriptor the read used** — not the path,
which may by then answer to a different file — and discards the result if size or
mtime moved. Discarded files land in `scan_error` with kind `race` and are picked
up by the next pass. That is the correct trade: a re-read costs seconds, a wrong
identity is permanent.

Prefer running the first pass during a migration quiet window anyway. Every scan
records a `scan_run` row, so a scan taken mid-churn can be identified and re-run.

### Duplication is an upper bound, not reclaimable space

The duplication figure is computed from apparent file sizes over distinct
inodes. Hardlinks are already handled — one inode is one file.

Reflinks and ReFS block clones are not. They share extents on disk but report
full size to any scan without FIEMAP, so on a btrfs array holding intentional
reflinked views the raw figure counts every view as waste (§9.4). `mm report`
labels the number an upper bound for this reason. Shared-extent detection arrives
with the presentation layer.

### The database must be on a local disk

SQLite's locking is broken over network filesystems and this is a well-known
corruption vector (§10.5). `mm` refuses to open a database on one.

The models themselves can live on the share — only the database has to be local.
An unidentifiable filesystem warns rather than blocks; a known network filesystem
is fatal. `--allow-network-db` overrides it, and should not be used.

### Overlapping roots

Nested roots are rejected. The present-sweep is scoped per root, so a path
reachable under two roots would be swept by whichever root did not claim it,
flapping `present` between scans. Sibling roots sharing a prefix (`/models` and
`/models2`) are fine — the check compares whole path segments.

### Symlinks are not followed

Once §9 view generation exists, a symlink farm pointing back into the library is
an expected part of the tree, and following it would report every view entry as a
second copy of a model already counted.

Reflinks are ordinary files and *are* scanned — correctly, since they genuinely
are a second path on the same content.

### Absent paths are kept

Paths not seen in the latest **completed** scan of their root get `present = 0`
rather than being deleted (§6.2), so the index can answer "is this model still on
disk?" instead of accumulating stale paths forever.

An **interrupted** scan sweeps nothing. Marking every unobserved path absent on
partial evidence would mark most of the library missing.

---

## Verification procedure

This is spec §17's Phase 0 checklist, made runnable.

**1. The double-run.** Run the scan twice. The second should be near-instant and
report `0 hashed, N cached`, with an identical distinct-hash count.

```sh
mm scan --root /Volume1/ai/models --workers 4
mm report --json | jq .totals.distinct_models
mm scan --root /Volume1/ai/models --workers 4   # expect 0 hashed
mm report --json | jq .totals.distinct_models   # expect the same number
```

**2. Independent cross-check.** Confirm stored hashes against a tool that shares
no code with this one:

```sh
mm report --json | jq -r '.top_duplicates[0].example_path' | xargs sha256sum
```

and compare against the database. `mm verify --sample 25` automates the same
check across a random sample.

**3. Concurrency.** `mm bench --root ... --workers 1,2,4` on the real array, with
a sample larger than RAM or with caches dropped between runs.

**4. Non-destruction.** Phase 0 opens model files read-only and creates nothing
inside the model tree. To confirm, snapshot mtimes before and after:

```sh
find /Volume1/ai/models -type f -printf '%T@ %p\n' | sort > /tmp/before.txt
mm scan --root /Volume1/ai/models
find /Volume1/ai/models -type f -printf '%T@ %p\n' | sort > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt   # expect no output
```

---

## What Phase 0 deliberately does not do

- **No interpretation.** Headers are stored, never parsed into fields.
- **No enrichment.** No Civitai, no HuggingFace, no network access at all.
- **No UI, no API, no daemon.** Phase 1.
- **No writes outside its own database.** Phases 0 and 1 are strictly read-only
  outside it (§14).
- **No deletion, ever.** Duplicates are reported. This tool does not pull the
  trigger, in this phase or any later one.

---

## Deviations from the spec, and why

Two places where the implementation refines what §6 describes. Both are recorded
in comments in `internal/store/schema.go`.

**1. `device` and `inode` live on the path row, not the file row.** §6.1 lists
them on the model file. They are per-instance facts — one hash has many paths,
each with its own inode — and the path row is the only place the incremental
cache can key on them. Putting them on the file row would make the cache
unimplementable as specified.

**2. `probe_sha256` is a stored column.** §10.1 describes the probe as a
comparison but does not say where the value lives. It is stored on `model_file`
and indexed with `size`, so the fallback is an index lookup rather than a scan.
An ambiguous probe — two distinct hashes sharing a size and a sample — resolves
to a full hash rather than to whichever row the query returned first.

---

## Next

Phase 1: read-only index, HTTP API, embedded React UI, ingest adapters for
ComfyUI / SwarmUI / Stability Matrix, and install detection. Nothing there is
allowed to emit a byte outside the database until the index is proven correct
against the full library.
