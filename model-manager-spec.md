# Model Manager — Spec v2

Working name: TBD. Independent metadata authority and organization layer for a local model
library.

Status: not started. Architecture settled; no code written.
Revised 2026-07-27 following design review.

> **What changed from v1.** Two things invalidated parts of the original spec. First, the app
> is now intended to be **distributable** — good enough to offer publicly as a replacement for
> Stability Matrix's model manager. Second, the app becomes the **source of truth for
> organization** (move, group, label, tag), reversing v1's "never moves files" non-goal into
> *views first, real moves later*.
>
> The core thesis — content-addressed metadata, read-only ingest, disposable sidecars,
> provenance-tracked fields — survives intact. The **stack and deployment model do not**: v1's
> Supabase-on-EAGLE-424 / Docker-fleet / tailnet-only design was built for one homelab and is
> disqualifying for a distributable tool.

---

## 1. Problem

~19,000 model files, ~7.5TB, currently migrating from the NAS home share to
`/Volume1/ai/models`, with ~2.76TB of orphaned files being relocated to `models-archive`.

Four tools each maintain their own metadata sidecars and each believes it owns them:

- ComfyUI + LoRA Manager extension
- SwarmUI
- Stability Matrix
- Civitai (remote origin)

**Observed failure:** running a metadata pull in Stability Matrix or SwarmUI sometimes
disconnects the master JSON, leaving safetensors with no matching metadata.

**Root cause:** every tool binds metadata to the model by *path and filename*.
`model.safetensors` → `model.json`. That link is positional, not intrinsic. Move, rename, or
have one tool rewrite a sidecar another tool owns, and the binding is gone — nothing in the
JSON knows which safetensors it belonged to. Migration generates path churn constantly, which
is why this is getting worse rather than better.

---

## 2. Core principle

**Metadata binds to file content, not to path.**

Primary key is the SHA256 of the model file. Path becomes a mutable attribute.

Consequences:

- File moves between roots → hash unchanged → metadata still attached
- Renamed → same
- Same model present in two locations → one record, two paths
- A tool scrambles its sidecar → rehash the file, look it up, regenerate
- Orphaning becomes **recoverable** for any file previously seen or present upstream — hash a
  loose safetensors and its identity comes back

> Note the wording. v1 claimed orphaning becomes *impossible*; that was an overclaim. Hashing
> a brand-new file with no upstream record and no useful header tells you it is new, not what
> it is. Self-trained LoRAs are exactly that case, which is why manual entry matters so much.

Bonus: Civitai indexes by SHA256, so the key that identifies the file locally is the same key
used to query them for enrichment.

### 2.1 The one thing that breaks content-addressing

Content-addressing assumes the file is immutable. It is not always: **some tools rewrite the
safetensors metadata header in place.** Identical weights, different SHA256, record silently
orphaned.

**Mitigation: compute a second weights-region hash in the same streaming pass.** The
safetensors layout is `8-byte LE header length → JSON header → tensor bytes`; hash from the
end of the header onward. Store both.

- `sha256` remains the primary key and the Civitai lookup key.
- `weights_sha256` is the stable rebinding key when a header is rewritten.

Cost is one extra hash context over bytes already in memory — effectively free.

---

## 3. Product goal

This is not only a personal tool. The target is something a stranger can install and use to
replace Stability Matrix's model manager, which many people run *only* for that purpose.

That goal imposes hard requirements v1 did not have:

- **Zero infrastructure.** No Docker, no Postgres, no account, no cloud service, no telemetry.
  Download, run, works.
- **Windows is a first-class target, not a port.** Most of the audience is there.
- **Zero-config first run.** Auto-detect existing ComfyUI / SwarmUI / Stability Matrix / A1111
  / Forge installs and their model roots, or adoption dies at the setup screen.
- **Fully offline except enrichment.** Everything but Civitai/HF lookup works with no network.

The personal requirement — browse from phone/iPad over the tailnet — is satisfied by the same
build, as a bind flag rather than a separate deployment.

---

## 4. Architecture

```
   ┌─ scanner / hasher (goroutines, local disk only)
   │
   ├─ ingest adapters (Civitai / HF / ComfyUI+LM / SwarmUI / StabMatrix / headers)
   │
DAEMON ─ SQLite (master, single file) ─ HTTP API (REST + OpenAPI)
   │                                          │
   ├─ link-strategy engine (presentation)     ├─ web UI (embedded, served by daemon)
   │                                          ├─ PWA over tailnet (same binary, one flag)
   └─ projection (tool sidecars)              └─ third-party SD frontends
```

**One process, one artifact.** The daemon runs on the machine with the disks locally attached
— non-negotiable, since hashing and link management both require local filesystem access. It
serves its own UI. `localhost:PORT` works out of the box; binding to another interface is a
config flag.

**Ingest is strictly read-only.** Parse whatever each tool has, merge into master. No external
tool ever writes master.

**Consumer sidecars are derived artifacts.** Project master out into each tool's dialect. If
Stability Matrix mangles one, regenerate it. Their "pull metadata" button stops being a threat
and becomes a no-op overwritten on next projection.

---

## 5. Stack

### 5.1 Datastore: SQLite, embedded, single file

This is the decision that gates everything else, and the one that cannot be retracted later.
Building on Supabase first and porting to embedded afterwards means rewriting the data layer,
every query, the auth model, and the entire deployment story. Building embedded-first and
adding optional Postgres sync later is additive and cheap.

- 19k rows is nothing. Postgres here is a datacenter brought to a knife fight.
- **FTS5 ships in SQLite** — full-text search with no extra service.
- **WAL mode** gives one writer (the daemon) and unlimited concurrent readers, which is
  exactly this app's access pattern.
- The database being **a single file** solves the backup problem v1 never addressed. The
  declared sole authority becomes something you can copy.
- Optional later: replicate to Postgres for multi-node. Not v1.

### 5.2 Daemon: Go + `modernc.org/sqlite`

Settled. The deciding factor is the **syscall surface, not SQLite**.

The link-strategy engine — the product's keystone — needs the `FICLONE` ioctl for reflinks,
`FIEMAP` for shared-extent detection, `CreateHardLinkW` and junction creation on Windows,
`DUPLICATE_EXTENTS_TO_FILE` for ReFS block cloning, and `inotify` / `ReadDirectoryChangesW`
for index freshness. Go covers all of it natively through `x/sys`. The alternative reaches
them through FFI shims or by shelling out to `cp` and `fsutil`, which would make the most
platform-sensitive subsystem the most fragile code in the app.

SQLite is a wash rather than a point for either side: `modernc.org/sqlite` is pure Go, no cgo,
FTS5 included, cross-compiles clean.

Distribution favors Go outright — a ~15MB static binary, and `GOOS`/`GOARCH` builds every
target from one machine. Windows is mature in Go and thinner in the alternatives precisely
where this app leans hardest: file watching, links, and path semantics.

Goroutines also give per-device sequential-read control for the hashing pass for free.

**Rejected and why:**

| Option | Reason |
|---|---|
| `pkg` | Archived and unmaintained. |
| `better-sqlite3` | Native module; breaks single-executable packaging. |
| Node 22+ `node:sqlite` + SEA | Both still experimental. |
| Bun + `bun:sqlite` | Clean SQLite story, but loses on the syscall surface above. |

**Known tradeoff:** `modernc.org/sqlite` is a C→Go translation and is slower than the cgo
`mattn/go-sqlite3`. At 19k rows this is irrelevant. If profiling ever proves otherwise, the
escape hatch is cgo — at the cost of clean cross-compilation.

**Cost accepted:** a slower first week standing up the daemon skeleton in a less familiar
language. The backend is mostly stdlib (`crypto/sha256`, `net/http`, `io/fs`), so the tax is
small and paid once.

### 5.3 Front-end: React 18 / TypeScript / Vite, embedded

Unchanged from v1, and independent of the daemon language — the boundary is HTTP either way.
Compiled to static assets and embedded in the binary via `embed.FS`. PWA by design, since
phone and iPad browsing is a primary use case rather than an afterthought.

---

## 6. Data model

### 6.1 Model file

- `sha256` (PK)
- `weights_sha256` — tensor-region hash; survives header rewrites (§2.1)
- `size`, `mtime`
- `device`, `inode` — cache key and same-file detection
- `format` — safetensors / ckpt / gguf / pt
- `header_blob` — safetensors/GGUF header captured **verbatim, uninterpreted**
- `first_seen`, `last_verified`

### 6.2 Model file path

v1 modeled locations as a bare `paths[]` array. That cannot express a path that *stopped*
existing, so the index would silently accumulate stale paths forever and could never answer
"is this model still on disk?" — which use cases 1, 2 and 8 all depend on.

Promoted to its own table:

- `sha256` (FK)
- `path`
- `first_seen`, `last_seen`
- `present` (bool)
- `scan_run_id`

Paths not observed in the latest completed scan of their root get `present = false` rather
than being deleted.

### 6.3 Model record

1:1 with `sha256`, but conceptually separate — the file versus what it *is*.

- `type` — checkpoint / LoRA / LyCORIS / VAE / embedding / controlnet / upscaler
- `base_model` — SDXL / Flux / Krea 2 / Qwen / Wan / Anima 2B / etc.
- `name`, `version`, `description`
- `trigger_words[]`
- `recommended_weight`, `recommended_settings`
- `nsfw` flag
- `tags[]`
- `preview_images[]`
- `origin` — civitai / huggingface / self-trained / unknown

### 6.4 Scan run

- `scan_run_id` (PK)
- `root`, `started_at`, `finished_at`, `status`

Needed so a scan taken mid-migration can be identified and re-run later.

---

## 7. Field provenance

This is the part that prevents re-inventing the current bug. Every field stores `value`,
`source`, `timestamp`.

**Precedence, highest first:**

1. **Manual** — entered by the user. Sticky. Never overwritten by any ingest, ever.
2. **Origin** — Civitai or HF by hash. Authoritative for anything untouched.
3. **Tool-derived** — StabMat / Swarm / LoRA Manager scrapes. Hints only, lowest trust.

Manual precedence matters more here than in most tools: self-trained LoRAs (Anima 2B runs)
have no remote record at all. Manual is the *only* source for a real slice of the library and
has to be untouchable.

### 7.1 Details v1 left unspecified

- **Same-tier conflict.** When StabMatrix and SwarmUI disagree on `base_model`, resolve by a
  fixed per-source trust order, then by most-recent timestamp.
- **Manual entries rotting.** Manual is correctly never overwritten — but when Origin later
  appears and disagrees, that must be **surfaced as a pending suggestion with one-click
  accept**, not silently discarded. Otherwise stale manual values become invisible and
  permanent.
- **Clearing a manual value.** "Never overwritten by ingest" requires an explicit
  manual-clear operation, or a mistyped field is unfixable-by-ingest forever.
- **Storage shape.** A `field_value` table holds every candidate (`sha256`, `field`, `value`,
  `source`, `source_tier`, `timestamp`); the resolved winners are **materialized** into typed,
  indexed columns on `model_record`. Search and UI read the materialized row; resolution
  re-runs on ingest. Pure EAV would make search painful; typed-only would lose the provenance
  that is the entire point.

---

## 8. Training record

Self-trained models only.

- `dataset` — which dataset, how many images
- `base` — Anima 2B, etc.
- `config` — rank, alpha, optimizer, LR, steps, batch
- `trainer` — ai-toolkit / Anima TrainFlow / OneTrainer
- `notes` — what worked, what did not
- `run_date`

This is the thing no existing tool does at all, and it is arguably the highest-value part of
the whole app. Right now that knowledge lives in scattered configs and memory.

Safetensors headers frequently carry these parameters directly for self-trained LoRAs, so a
chunk of this can auto-populate from `header_blob` with no user effort.

---

## 9. Presentation layer — the portability keystone

The agreed model is **organize by views, not by moving bytes**. The app owns a directory tree
that consuming tools point at; nothing points at real files directly.

This delivers "move, group, label, tag" non-destructively: unlimited views (by base model, by
type, by tag, by project) generated from the same underlying files, fully reversible, with no
risk to the library.

How a view is materialized must be abstracted, because the correct mechanism differs per
platform and filesystem. Getting this wrong is what would make the tool Linux-only.

### 9.1 Strategies, probed at startup

| Strategy | Where | Notes |
|---|---|---|
| **btrfs reflink** | same btrfs filesystem | **Preferred on this array.** A real file sharing blocks with the original — ~zero disk cost, and appears as an ordinary file to every consumer. Works over SMB with no Samba configuration at all. |
| POSIX symlink | same Linux host as the tools | Cheapest and instant, but see the SMB warning. |
| ReFS / Dev Drive block clone | Windows, ReFS volumes | True copy-on-write via `DUPLICATE_EXTENTS_TO_FILE`. Preferred on Windows where available — but most users are on NTFS, so it cannot be the baseline. |
| NTFS hardlink | Windows, within a volume | Works without admin privilege, **but is not copy-on-write**. Opt-in only — see the warning. |
| Junction / symlink | Windows | Junctions for directories; file symlinks need admin or Developer Mode. Not a default. |
| Real copy | cross-device, or NTFS default | The only option when crossing filesystems, and the safe Windows default. |

### 9.2 Hard warning — symlinks over SMB

If any SD tool reaches the models over a network share, a symlink farm will likely fail. Samba
resolves symlinks server-side only under specific settings, and links pointing outside the
share ("wide links") are disabled by default for security and interact badly with
`unix extensions`. Loosening that is a real security tradeoff, not a config tweak.

**Therefore, on this array specifically: prefer reflinks over symlinks.** They give every
benefit of the symlink farm while being indistinguishable from ordinary files to ComfyUI,
Swarm, Stability Matrix, and anything over SMB.

### 9.3 Hard warning — NTFS hardlinks undermine the weights-region hash

A hardlink is not a copy; it is a second name for the same inode. A tool that rewrites a
safetensors header in place — precisely the failure mode §2.1 exists to survive — writes
through the hardlink and mutates the **original**. Reflinks and ReFS block clones diverge on
write; NTFS hardlinks do not. NTFS has no copy-on-write equivalent.

**Windows position, in preference order:**

1. Detect ReFS / Dev Drive block cloning and use it where available.
2. Otherwise **default to real copies** for any view a consumer tool might write to. Disk cost
   is the tradeoff, and it is the user's to opt out of.
3. Hardlinks available as explicit opt-in, with the risk documented in-product.

Partial mitigation regardless of strategy: because `weights_sha256` is stored, an in-place
header rewrite is *detectable* on the next verification pass rather than silently orphaning
the record. Detection is not prevention, so it does not change the default.

### 9.4 Consequences to design for

- Reflinks only work **within one btrfs filesystem**. Cross-filesystem views fall back to real
  copies.
- Each reflink reports its **full size** to `du` and to a naive scan. The duplicate report
  must be reflink-aware, or it will loudly report every intentional view as wasted space.
  Detect shared extents rather than comparing apparent sizes.
- Shared-extent detection means **FIEMAP ioctls — Linux-only, and per-file syscall work across
  ~19k files.** Cache the results keyed on `inode + mtime`, exactly like the hash cache.
  Windows and macOS fall back to reporting apparent duplicates with a caveat rather than
  blocking the feature.

---

## 10. Scanning and index freshness

### 10.1 Cache key

**`(device, inode, size, mtime)`** — not `(path, size, mtime)`.

The spec's own premise is that paths churn constantly. Keying the incremental cache on path
means every migrated file misses cache and gets fully re-hashed — the exact workload the cache
exists to avoid. A move *within* the btrfs array preserves the inode, so it costs nothing.

Second-tier fallback for cross-volume copies (new inode, preserved mtime): probe
`(size, hash of first 1MB + last 1MB)` before committing to a full read.

### 10.2 Scan safety during migration

Phase 0 hashes 7.5TB while FreeFileSync is still moving files. A file copied into place
mid-walk can be hashed partially-written, and that wrong hash would be committed as a
permanent identity.

**Rule: re-stat `size` and `mtime` immediately after the hash completes; discard the result if
either changed since the pre-hash stat.** Prefer running Phase 0 during a migration quiet
window. Record a `scan_run` row so a scan taken mid-churn can be identified and re-run.

### 10.3 Freshness contract

Files will arrive outside the app — Stability Matrix downloads, manual drops, Syncthing.

**Scheduled incremental rescan is the contract; filesystem watchers are an optimization.**

Rescans are cheap because of the cache — an unchanged tree costs a stat pass, not a hash pass.
Watchers (`inotify`, `ReadDirectoryChangesW`) are added where they work well, but cannot be
the contract: inotify requires a watch per directory and hits `max_user_watches` limits on
deep trees, and watchers are unreliable-to-useless over SMB. The UI also gets rescan-on-focus
and an explicit **Rescan now** control.

### 10.4 Format safety

**Never unpickle `.ckpt`.** It is Python pickle; parsing it is arbitrary code execution on
files sourced from the internet. Hash and size only. Header parsing covers safetensors and
GGUF, both of which have safe binary/JSON headers.

### 10.5 Database location

**The SQLite file must live on a locally attached disk, never on an SMB path.** SQLite's
locking is broken over network filesystems and this is a well-known corruption vector. The
daemon refuses to start if its database path is on a network mount.

---

## 11. HTTP API and hardening

The API is the third interop tier (alongside the filesystem view and sidecar projection) and
the thing that makes the app readable from any SD front-end. REST, documented with OpenAPI.

The daemon exposes downloads, placement, and eventually file moves over HTTP. Two real
exposures for a distributable tool: users binding `0.0.0.0` on shared LANs, and
**DNS-rebinding attacks against localhost APIs from an ordinary browser tab**. Both are cheap
to close now and painful to retrofit once third-party front-ends depend on the API.

**Minimum baseline, non-negotiable for the public build:**

- **Bind `127.0.0.1` by default.** Any other interface is an explicit opt-in.
- **Strict `Host` header allowlist.** Reject requests whose Host is not loopback or a
  configured hostname. This is the standard DNS-rebinding defense.
- **No CORS wildcard.** Explicit origin allowlist only.
- **Generated bearer token required whenever bound to a non-loopback interface.** Written to a
  file with restrictive permissions; the bundled UI reads it locally.

Tailnet binding may exempt the token, since the tailnet is already authenticated. The public
build may not.

---

## 12. Ingest sources

| Source | Read | Notes |
|---|---|---|
| Filesystem scan | paths, size, mtime, dev/inode, both hashes | The foundation. Everything else keys off it. |
| Civitai API | full metadata by SHA256 | Rate-limited. Cache aggressively — see below. |
| HuggingFace | repo card, config, license | Match by repo/filename — no hash index, so weaker binding. |
| ComfyUI / LoRA Manager | its JSON sidecars | Treat as hints. |
| Stability Matrix | its metadata store | Treat as hints. |
| SwarmUI | its model metadata | Treat as hints. |
| Manual entry | anything | Highest precedence. Required for self-trained. |
| Safetensors / GGUF header | embedded metadata | Free, offline, often has training config. Parse during the hash pass. |

**Header parsing during the hash pass costs almost nothing** since the file is already open,
and it may auto-populate a chunk of the training record.

### 12.1 The Civitai cache is an archive

Models are removed from Civitai regularly. Once gone, the metadata is unrecoverable anywhere.

- **Store the full raw API response blob**, not just extracted fields. Never expire it.
- **Cache negative lookups too**, with a TTL — otherwise every run re-queries thousands of
  known misses.
- **Store all hash types Civitai returns** for a file, not just SHA256.

This makes the local cache quietly the only surviving copy of metadata for taken-down models —
a real value-add, not just an optimization.

---

## 13. Use cases

In rough priority:

1. **Find a model.** Search across 19k by name, base model, type, trigger word, tag. Currently
   impossible.
2. **Answer "what is this file?"** Point at an orphaned safetensors, get its identity back.
3. **Organize.** Group, label, tag, and arrange into views — without touching the files.
4. **Recover from a tool stomping metadata.** Reproject and move on.
5. **Browse from phone/iPad over the tailnet.** Look up trigger words and recommended weights
   while generating from the couch.
6. **Download from a Civitai or HF link.** Paste or share a link, pick a destination, fetch
   model + metadata + preview together.
7. **Keep hot models on SSD.** Browse the whole library; stage the active set to fast storage.
8. **Recall training configs.** Which dataset and settings produced this LoRA.
9. **Report duplicates.** Same hash in multiple locations. Report only — see non-goals.
10. **Spot gaps.** Models with no metadata, no preview, no known base model.

---

## 14. Non-goals

Hard fences. Violating any of these turns a finishable project into an unfinishable one.

- **Does not generate images.** Not a frontend. ComfyUI and Swarm already do this.
- **Never modifies, moves, renames, or deletes an existing file.** The app creates new files
  only at explicitly user-chosen destinations, and only ever sidecar/preview files, view
  entries, or freshly downloaded models. **It never opens a model file for writing.**
- **Writes nothing into the model tree at all before the index is verified.** Phases 0 and 1
  are strictly read-only outside the app's own database.
- **Does not dedupe by deleting.** Reports duplicate hashes, surfaces them, lets you act
  elsewhere. Does not pull the trigger.
- **Not a general-purpose file manager.**

> **On the v1 reversal.** v1 said "never moves files," full stop. That fence was written for a
> specific temporary condition: mid-migration, no index, no way to verify or recover. Once the
> index is verified, the app is the *only* thing in the system that can move files safely,
> because it is the only thing that can verify by hash and repair the binding afterward.
>
> The fence is therefore re-scoped rather than removed. Real byte-moving is deferred to the
> very end, opt-in, journaled, copy-verify-then-remove, with full undo — and is expected to be
> rarely needed, because views (§9) absorb most of the demand for it.

---

## 15. Phasing

**Phase 0 — hash pass, no app.**
A Go binary. Walk the model roots, hash everything (both hashes), capture headers, write to
SQLite. Nothing else. Runs on the machine with disks locally attached. Commits per file;
restartable at any point.

Output answers one question: how many *distinct* models exist across 19k files, and how much
of the 7.5TB is duplication. That number decides whether this is a 3,000-row problem or a
19,000-row one, and it surfaces the duplicate situation for free. It also sizes the SSD tier.

**Phase 0 stores raw uninterpreted facts only** — hashes, size, mtime, dev/inode, path, and
the header captured verbatim as an opaque blob. No parsing into typed fields. Those facts stay
valid no matter what Phase 1 decides about the schema, so a schema change never costs a
re-hash of 7.5TB. Header interpretation becomes a cheap re-runnable pass over stored blobs.

**Hash on the machine with the disks locally attached.** Not across SMB — 7.5TB over the wire
is a day-plus; local off the btrfs array is IO-bound and finishes.

**Phase 1 — read-only index + UI + API.**
Daemon, embedded DB, HTTP API, React UI served by the daemon. Ingest from all sources. Search,
browse, view. Install-detection for existing tools. Writes nothing outside its own database.
Prove the index is correct against the full library before it is allowed to emit a single byte
elsewhere.

**Phase 2 — enrichment + download.**
Civitai and HF lookup by hash. Manual editing. Training record entry. Tags and groups (pure
metadata, so they land here rather than waiting for the presentation layer).

*And* download-from-link, promoted from v1's Phase 4 because it is the single most common
reason people run Stability Matrix's model manager — the primary adoption driver for the
product goal.

**Download is its own workstream, not a one-liner**, or it will slip: API-key handling for
auth-gated Civitai models, rate limiting and backoff, resumable multi-GB transfers via HTTP
range requests, partial-file quarantine, and checksum verification against the expected hash
on completion before the file is admitted to the index.

**Phase 3 — presentation layer.**
The link-strategy engine and generated views. This is where organization is actually
delivered, non-destructively.

**Phase 4 — SSD tiering.**
Staging, atomic swap, pinning and eviction. Sized from Phase 0 output.

**Phase 5 — sidecar projection.**
Generate tool sidecars from master. Still gated on the index being proven trustworthy. Start
with one tool, verify, then add others.

**Later / optional — real file moves.**
Journaled, copy-verify-then-remove, full undo. Only after the index is trusted. Deliberately
last, because Phases 3–4 are expected to absorb most of the demand.

---

## 16. Infrastructure decisions

### 16.1 FreeFileSync

It is doing two unrelated jobs, and only one is a good fit.

- **Placement / organization — retire it from the models tree.** It is path-based mirroring,
  the same positional model that caused the original bug, and under this plan it becomes a
  second system that believes it owns layout. Two owners of placement is exactly the failure
  being escaped.
- **Backup — a legitimate job**, but at 7.5TB on btrfs, **`btrfs send`/`receive` over snapshots
  is materially better**: atomic, block-level incremental, and gives point-in-time rollback
  that file-level sync cannot.

### 16.2 Migration — check before it completes

**Are source and destination on the same btrfs filesystem?** If so, most of a 7.5TB copy is
unnecessary — same-filesystem relocation is a metadata operation, and cross-subvolume can use
reflinks.

Note that **btrfs subvolumes report different `st_dev` values on the same filesystem**, so
comparing device IDs gives a false negative. The reliable empirical test is to attempt
`cp --reflink=always` on a single file: if it succeeds, they share a filesystem and reflinks
are available as the presentation mechanism.

### 16.3 SSD tiering

The data model already supports this with no new concepts: an SSD copy is **a second path on
the same hash** — verifiable, disposable, re-derivable at any time.

Mechanism: stage the copy to SSD in the background, then **atomically swap the presentation
entry** to point at the SSD copy. The swap is instant; the only cost is background copy time,
satisfying the limited-downtime requirement. Reflinks do not help here — crossing devices is a
genuine copy.

**Do not buy the SSD yet.** Phase 0 outputs the distinct-model count and size distribution,
which is precisely the input needed to size a working set. Sizing before that number exists is
guessing. Pinning plus LRU eviction covers the policy; which dominates depends on whether the
working set turns out to be ~150 models or ~600.

### 16.4 Backup of master

Master is declared the sole authority. A single-copy authority is a worse failure mode than
today's scattered sidecars. The single-file SQLite database makes this tractable: scheduled
file-level backup plus a hash-keyed JSON export that is restorable without the app.

---

## 17. Verification

- **Phase 0:** re-run the scan twice. The second run should be near-instant via cache and
  produce an identical distinct-hash count. Hash a handful of files independently with
  `sha256sum` and confirm they match the database.
- **Hashing concurrency:** during that same double-run, benchmark **1 worker vs. 4 on the
  actual array before assuming parallelism wins.** On spinning rust, concurrent readers
  seek-thrash and frequently lose to sequential large-buffer reads. If parallelism does help,
  scale it per-device rather than globally.
- **Link strategy:** confirm `cp --reflink=always` succeeds on the array, then verify a
  reflinked model in a generated view is readable by ComfyUI and over SMB, and that
  `btrfs filesystem du` reports shared rather than duplicated extents.
- **Standalone claim:** install the packaged build on a clean Windows machine with no Docker,
  no Postgres, and no config. It must detect an existing ComfyUI or Stability Matrix install,
  scan, and serve its UI. If that fails, the product goal is not met.
- **Tiering:** stage a model to SSD, swap, and confirm the consuming tool never sees a missing
  file and requires no restart.
- **Non-destruction:** after every phase through Phase 5, verify no model file's mtime or hash
  has changed anywhere in the tree.

---

## 18. Decisions closed

v1 left these open. All are now settled.

| Question | Decision |
|---|---|
| Postgres FTS vs. dedicated search index | Neither — SQLite FTS5. 19k rows needs no search stack. |
| Preview image storage: in place or app-managed? | **App-managed, content-addressed.** In-place references break on move, which is the precise failure this app exists to eliminate. Tens of GB against 7.5TB, and thumbnails are needed for the PWA regardless. |
| Push vs. pull projection | **Push.** Nothing supports pull today. |
| "Hash collision" between a model and its quantized variant | Misnamed — that is the opposite of a collision. Reframed as **variant grouping**. The weights-region hash resolves the re-saved-header case automatically, leaving only genuine fp16/fp8/pruned variants needing a manual "variant of" relation. |
| Civitai rate limits at 19k lookups | Throttled, resumable batch runs, aggressive caching including negative results. |

### Still open

- **Auth model.** Single-user is assumed. Confirm before anyone builds RBAC that is not needed.
- **Duplicate report semantics.** Needs a canonical-copy rule (e.g. the copy under a tool's
  configured root wins) or the report is not actionable.
- **Update checking.** Notify when a newer version of a model exists upstream — desirable for
  the product goal, unscheduled.

---

## 19. Immediate next actions

1. Run `cp --reflink=always` on the array to settle the same-filesystem question. It determines
   both migration cost and whether reflinks are the presentation mechanism.
2. Confirm where each SD tool runs relative to the array, since the answer selects the default
   link strategy.
3. Build Phase 0 — Go scanner writing SQLite, raw uninterpreted facts only.
