package store

// Phase 0 schema. Raw uninterpreted facts only -- hashes, sizes, times, dev/inode,
// paths, and format headers captured verbatim as opaque blobs (spec §15). Nothing
// here parses a header into typed fields; that is a cheap re-runnable pass in a
// later phase, which is the entire reason a schema change must never cost a
// re-hash of 7.5TB.
//
// Migrations are append-only. Never edit a migration that has shipped; add a new
// one. `version` is the index in this slice plus one.
var migrations = []string{
	// --- 1: initial Phase 0 tables -------------------------------------------
	`
CREATE TABLE model_file (
    -- Primary key is the SHA256 of the whole file, which is also the Civitai
    -- lookup key (spec §2).
    sha256            TEXT    PRIMARY KEY,

    -- Tensor-region hash: survives an in-place header rewrite, which is the one
    -- thing that breaks content-addressing (spec §2.1). NULL means "no rebinding
    -- key available for this file" -- true for .ckpt and .pt, whose weights
    -- region cannot be located without deserializing, which is forbidden (§10.4).
    -- Rebinding logic MUST treat NULL as absent, not as a populated column.
    weights_sha256    TEXT,

    -- Byte offset at which the weights region begins. NULL alongside a NULL
    -- weights_sha256.
    weights_offset    INTEGER,

    -- Sampled probe over (first 1MiB + last 1MiB), used as the second-tier cache
    -- fallback for cross-volume copies (spec §10.1). A probe match NEVER confers
    -- identity; it binds a path provisionally and nothing more.
    probe_sha256      TEXT    NOT NULL,

    size              INTEGER NOT NULL,
    format            TEXT    NOT NULL,  -- safetensors | gguf | ckpt | pt | unknown

    -- Format header captured verbatim and uninterpreted. For safetensors this is
    -- the JSON header; for GGUF the magic through the tensor infos.
    header_blob       BLOB,
    header_offset     INTEGER,           -- where header_blob starts in the file
    header_truncated  INTEGER NOT NULL DEFAULT 0,

    first_seen        TEXT    NOT NULL,
    last_verified     TEXT    NOT NULL
) STRICT;

-- Second-tier cache probe lookup: size first because it is the cheap discriminator.
CREATE INDEX idx_model_file_probe ON model_file (size, probe_sha256);

-- Rebinding after an in-place header rewrite (spec §2.1). Partial index: the NULL
-- rows are precisely the ones that can never be rebound this way.
CREATE INDEX idx_model_file_weights ON model_file (weights_sha256)
    WHERE weights_sha256 IS NOT NULL;

-- Needed so a scan taken mid-migration can be identified and re-run (spec §6.4).
CREATE TABLE scan_run (
    scan_run_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    root         TEXT    NOT NULL,
    started_at   TEXT    NOT NULL,
    finished_at  TEXT,
    status       TEXT    NOT NULL,  -- running | completed | failed | interrupted

    files_seen   INTEGER NOT NULL DEFAULT 0,
    files_hashed INTEGER NOT NULL DEFAULT 0,
    files_cached INTEGER NOT NULL DEFAULT 0,
    files_probed INTEGER NOT NULL DEFAULT 0,
    bytes_hashed INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0
) STRICT;

-- A location is not a property of content: one hash has many paths, and each path
-- carries its own dev/inode/mtime. Spec §6.1 lists dev/inode on the file record,
-- but they are per-instance facts and belong here -- this is also the only place
-- the incremental cache can key on them. See docs/phase0.md.
CREATE TABLE model_file_path (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256       TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,

    -- The root this path was discovered under. The present-sweep is scoped per
    -- root, so a scan of one root must never mark another root's paths absent.
    root         TEXT    NOT NULL,

    -- The incremental cache key (spec §10.1): (device, inode, size, mtime).
    -- Deliberately NOT (path, size, mtime) -- the premise of the whole design is
    -- that paths churn, and a path-keyed cache misses on every migrated file.
    device       INTEGER NOT NULL,
    inode        INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    mtime_ns     INTEGER NOT NULL,

    first_seen   TEXT    NOT NULL,
    last_seen    TEXT    NOT NULL,

    -- Paths not observed in the latest completed scan of their root get
    -- present = 0 rather than being deleted (spec §6.2), so the index can answer
    -- "is this model still on disk?" instead of accumulating stale paths forever.
    present      INTEGER NOT NULL DEFAULT 1,

    -- Bound by sampled probe, not yet confirmed by a full hash (spec §10.1).
    -- Usable for browsing; never the basis for projection, dedup reporting,
    -- tiering, or any write-side decision until confirmed.
    provisional  INTEGER NOT NULL DEFAULT 0,

    scan_run_id  INTEGER NOT NULL REFERENCES scan_run(scan_run_id)
) STRICT;

CREATE INDEX idx_path_cache    ON model_file_path (device, inode, size, mtime_ns);
CREATE INDEX idx_path_sha      ON model_file_path (sha256);
CREATE INDEX idx_path_root     ON model_file_path (root, present);
CREATE INDEX idx_path_prov     ON model_file_path (provisional) WHERE provisional = 1;

-- Errors are recorded rather than swallowed: a scan that silently skipped 400
-- unreadable files while reporting success is worse than one that fails loudly.
CREATE TABLE scan_error (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_run_id  INTEGER NOT NULL REFERENCES scan_run(scan_run_id),
    path         TEXT    NOT NULL,
    kind         TEXT    NOT NULL,  -- stat | open | read | race | header
    message      TEXT    NOT NULL,
    occurred_at  TEXT    NOT NULL
) STRICT;

CREATE INDEX idx_scan_error_run ON scan_error (scan_run_id);
`,

	// --- 2: Phase 1 interpretation layer -------------------------------------
	//
	// Everything above this line is a raw fact. Everything below is an
	// interpretation of one, and can be recomputed from the facts plus whatever
	// was ingested -- which is why a change down here never costs a re-hash.
	`
-- Every candidate value for every field, with where it came from and when
-- (spec §7). This table is the thing that prevents re-inventing the original
-- bug: no ingest can silently replace a value, because nothing is ever replaced
-- -- candidates accumulate and a resolver picks a winner.
--
-- One row per (model, field, source). Re-ingesting from the same source updates
-- that source's row in place; it never touches another source's opinion.
CREATE TABLE field_value (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256      TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    field       TEXT    NOT NULL,

    -- JSON-encoded, always. Scalars and arrays share this column, and encoding
    -- everything means a trigger-word list and a weight round-trip through the
    -- same code path without a type tag beside them.
    value       TEXT    NOT NULL,

    source      TEXT    NOT NULL,
    source_tier INTEGER NOT NULL,  -- 3 manual, 2 origin, 1 tool-derived
    observed_at TEXT    NOT NULL,

    UNIQUE (sha256, field, source)
) STRICT;

CREATE INDEX idx_field_value_lookup ON field_value (sha256, field);

-- The resolved winners, materialized into typed indexed columns (spec §7.1).
-- Search and the UI read this; resolution rewrites it on ingest. Pure EAV would
-- make search painful, and typed-only would lose the provenance that is the
-- entire point -- so both exist, with this side derived from the other.
CREATE TABLE model_record (
    sha256               TEXT PRIMARY KEY REFERENCES model_file(sha256) ON DELETE CASCADE,
    type                 TEXT,     -- checkpoint / lora / lycoris / vae / embedding / controlnet / upscaler
    base_model           TEXT,     -- SDXL / Flux / Krea 2 / Qwen / Wan / Anima 2B / ...
    name                 TEXT,
    version              TEXT,
    description          TEXT,
    trigger_words        TEXT,     -- JSON array
    recommended_weight   REAL,
    recommended_settings TEXT,     -- JSON object
    nsfw                 INTEGER,
    origin               TEXT,     -- civitai / huggingface / self-trained / unknown
    updated_at           TEXT NOT NULL
) STRICT;

CREATE INDEX idx_model_record_type ON model_record (type);
CREATE INDEX idx_model_record_base ON model_record (base_model);
CREATE INDEX idx_model_record_name ON model_record (name);

-- A manual value is never overwritten by ingest. But when Origin later appears
-- and disagrees, silently discarding it makes stale manual values invisible and
-- permanent (spec §7.1) -- so the disagreement is surfaced here for one-click
-- accept instead.
CREATE TABLE suggestion (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256          TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    field           TEXT    NOT NULL,
    manual_value    TEXT    NOT NULL,
    suggested_value TEXT    NOT NULL,
    source          TEXT    NOT NULL,
    created_at      TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'pending',  -- pending | accepted | dismissed
    UNIQUE (sha256, field, source)
) STRICT;

CREATE INDEX idx_suggestion_pending ON suggestion (status) WHERE status = 'pending';

CREATE TABLE tag (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    created_at TEXT    NOT NULL
) STRICT;

CREATE TABLE model_tag (
    sha256   TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    tag_id   INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
    source   TEXT    NOT NULL,
    added_at TEXT    NOT NULL,
    PRIMARY KEY (sha256, tag_id)
) STRICT;

CREATE INDEX idx_model_tag_tag ON model_tag (tag_id);

-- Groups. Pure metadata, so they land in Phase 2 rather than waiting for the
-- presentation layer, and a view (§9) can later be generated from one.
CREATE TABLE collection (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT,
    created_at  TEXT    NOT NULL
) STRICT;

CREATE TABLE collection_member (
    collection_id INTEGER NOT NULL REFERENCES collection(id) ON DELETE CASCADE,
    sha256        TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    position      INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT    NOT NULL,
    PRIMARY KEY (collection_id, sha256)
) STRICT;

-- Self-trained models (spec §8). The thing no existing tool does at all, and
-- arguably the highest-value part of the app -- right now this knowledge lives
-- in scattered configs and memory.
CREATE TABLE training_record (
    sha256       TEXT PRIMARY KEY REFERENCES model_file(sha256) ON DELETE CASCADE,
    dataset      TEXT,
    dataset_size INTEGER,
    base         TEXT,
    config       TEXT,     -- JSON: rank, alpha, optimizer, lr, steps, batch
    trainer      TEXT,     -- ai-toolkit / Anima TrainFlow / OneTrainer
    notes        TEXT,
    run_date     TEXT,
    source       TEXT NOT NULL,  -- manual | safetensors_header
    updated_at   TEXT NOT NULL
) STRICT;

-- App-managed and content-addressed (spec §18). In-place references break on
-- move, which is the precise failure this app exists to eliminate. Bytes live in
-- a blob directory beside the database, not inside it, so the database stays
-- small enough to copy.
CREATE TABLE preview_image (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256       TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    image_sha256 TEXT    NOT NULL,
    mime         TEXT    NOT NULL,
    bytes        INTEGER NOT NULL,
    width        INTEGER,
    height       INTEGER,
    source       TEXT    NOT NULL,
    position     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL,
    UNIQUE (sha256, image_sha256)
) STRICT;

CREATE INDEX idx_preview_model ON preview_image (sha256, position);

-- Models are removed from Civitai regularly, and once gone the metadata is
-- unrecoverable anywhere (spec §12.1). So the full raw response is stored and
-- never expired -- this cache is quietly an archive, and for taken-down models
-- it may be the only surviving copy.
CREATE TABLE origin_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    provider     TEXT    NOT NULL,  -- civitai | huggingface
    lookup_key   TEXT    NOT NULL,
    found        INTEGER NOT NULL,
    raw_response TEXT,
    http_status  INTEGER,
    fetched_at   TEXT    NOT NULL,

    -- Set for negative lookups only. Without a negative cache every run
    -- re-queries thousands of known misses; with an expiring one, a model that
    -- appears upstream later is still picked up.
    expires_at   TEXT,

    UNIQUE (provider, lookup_key)
) STRICT;

CREATE INDEX idx_origin_cache_neg ON origin_cache (provider, expires_at) WHERE found = 0;

-- All hash types an origin reports, not just SHA256 (spec §12.1): the others are
-- how other tools and other providers refer to the same file.
CREATE TABLE origin_hash (
    sha256     TEXT NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    hash_type  TEXT NOT NULL,   -- SHA256 / AutoV1 / AutoV2 / BLAKE3 / CRC32
    hash_value TEXT NOT NULL,
    provider   TEXT NOT NULL,
    PRIMARY KEY (sha256, hash_type, provider)
) STRICT;

CREATE INDEX idx_origin_hash_value ON origin_hash (hash_value);

-- FTS5 ships in SQLite, so full-text search costs no extra service (spec §5.1).
-- Kept in sync explicitly from Go rather than by trigger: the indexed text is
-- assembled from four tables plus a filename, which is beyond what a trigger can
-- express without becoming the least debuggable thing in the schema.
CREATE VIRTUAL TABLE model_search USING fts5(
    sha256 UNINDEXED,
    name,
    description,
    base_model,
    type,
    trigger_words,
    tags,
    filename,
    tokenize = 'unicode61 remove_diacritics 2'
);
`,
	// --- 3: Phase 3 presentation layer ---------------------------------------
	//
	// Organize by views, not by moving bytes (spec 9). The app owns a directory
	// tree that consuming tools point at; nothing points at real files directly,
	// so grouping and labelling are fully reversible and carry no risk to the
	// library.
	`
CREATE TABLE view (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    root         TEXT    NOT NULL,

    -- How entries are grouped into subdirectories: base_model, type, tag,
    -- collection, or flat.
    group_by     TEXT    NOT NULL DEFAULT 'flat',

    -- JSON filter using the same shape as a search query, so a view is
    -- literally a saved search, materialized.
    filter       TEXT,

    -- The link strategy last used. Recorded rather than assumed, because it
    -- decides whether entries are safe for a consumer to write through.
    strategy     TEXT,

    created_at   TEXT    NOT NULL,
    generated_at TEXT,
    status       TEXT    NOT NULL DEFAULT 'never-generated'
) STRICT;

-- Every file this app created inside a view, so a view can be removed exactly.
--
-- Deleting a view means deleting only the entries recorded here, never
-- everything under the root: a user who points a view at a directory that
-- already holds something must not lose it.
CREATE TABLE view_entry (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    view_id     INTEGER NOT NULL REFERENCES view(id) ON DELETE CASCADE,
    sha256      TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,
    path        TEXT    NOT NULL UNIQUE,
    source_path TEXT    NOT NULL,
    strategy    TEXT    NOT NULL,
    bytes       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL
) STRICT;

CREATE INDEX idx_view_entry_view ON view_entry (view_id);
CREATE INDEX idx_view_entry_sha  ON view_entry (sha256);

-- Shared-extent measurements, cached on (device, inode, mtime) exactly like the
-- hash cache. FIEMAP is per-file syscall work across a whole library, so
-- re-measuring an unchanged file on every report is not affordable.
CREATE TABLE extent_cache (
    device      INTEGER NOT NULL,
    inode       INTEGER NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    apparent    INTEGER NOT NULL,
    shared      INTEGER NOT NULL,
    exclusive   INTEGER NOT NULL,
    measured_at TEXT    NOT NULL,
    PRIMARY KEY (device, inode, mtime_ns)
) STRICT;
`,

	// --- 4: managed roots and settings ---------------------------------------
	//
	// Until now the set of model roots was inferred from the data --
	// SELECT DISTINCT root FROM model_file_path -- which cannot express a root
	// that holds no models yet, cannot carry a label or a tool association, and
	// disappears entirely if the last file under it goes away. Adding a
	// directory from the UI needs all three, so the roots become a table.
	//
	// `path` holds the canonical spelling and is the authority. This matters
	// beyond tidiness: SweepAbsentPaths matches `WHERE root = ?` exactly, so two
	// spellings of one directory fork it and strand rows at present=1 forever.
	`
CREATE TABLE model_root (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    path            TEXT    NOT NULL UNIQUE,
    label           TEXT    NOT NULL DEFAULT '',

    -- The tool whose folder vocabulary this root uses ('stability-matrix',
    -- 'swarmui', 'comfyui', or ''). Decides the default per-type subfolder for
    -- downloads: one type has three different folder names across the three
    -- tools, so the mapping can only be (root, type) -> subfolder.
    tool            TEXT    NOT NULL DEFAULT '',

    -- A disabled root is remembered but skipped by scans. Removing a root is
    -- destructive to metadata association; disabling is the reversible option.
    enabled         INTEGER NOT NULL DEFAULT 1,

    added_at        TEXT    NOT NULL,
    last_scanned_at TEXT
) STRICT;

-- Seed from what the library already knows, so an existing database keeps
-- working with no user action. Roots recorded before this migration are
-- already canonical: they were written by prepareRoots.
INSERT INTO model_root (path, added_at)
SELECT DISTINCT root, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM model_file_path
 WHERE root <> '';

-- Small JSON-valued key/value store for user preferences: saved library
-- filters, the default download root, the per-(root,type) folder map. One kv
-- table rather than a column per preference, so a new setting is a write and
-- not a migration -- and preferences live server-side rather than in
-- localStorage because the same daemon serves the phone over the tailnet, and
-- a view you configured on the desktop should follow you there.
CREATE TABLE setting (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
`,

	// --- 5: user-supplied previews ------------------------------------------
	//
	// Two columns, both content addresses into the blob store.
	//
	// thumb_sha256 is a small derived copy served to the grid. A library of
	// 19k models rendering full-size preview images is tens of gigabytes over
	// the wire for a page of forty cards, and the phone on the tailnet is the
	// client that pays for it.
	//
	// workflow_sha256 is the ComfyUI workflow JSON carried in a generated
	// PNG's tEXt/iTXt chunk. Stored separately from the image so the workflow
	// survives even if the image is later replaced, and so it can be handed
	// back as a file to drop into ComfyUI.
	`
ALTER TABLE preview_image ADD COLUMN thumb_sha256 TEXT;
ALTER TABLE preview_image ADD COLUMN workflow_sha256 TEXT;

-- Manual previews sort ahead of fetched ones, the same tiering the field
-- provenance already uses: a thumbnail you chose must not be displaced by the
-- next enrichment run.
CREATE INDEX idx_preview_rank
    ON preview_image (sha256, source, position);
`,

	// --- 6: persisted origin identity and upstream update status -------------
	//
	// Which remote model a local file was published as was, until now,
	// re-derived on every BuildLocalIndex by JSON-parsing every archived
	// origin_cache.raw_response in Go (origin/annotate.go loadOwnedVersions).
	// That is correct and works retroactively -- the archive is complete and
	// never expired -- but it is O(whole archive) per browse request, it cannot
	// be joined or filtered in SQL, and it cannot be indexed. So neither "which
	// of my models has a newer version" nor "show me every copy I hold of this
	// upstream model" could be expressed as a query at all.
	//
	// DDL only. The backfill is origin.BackfillModelOrigin, in Go, deliberately:
	// a SQL backfill would need json_extract, and nothing in this repo uses
	// SQLite's JSON1 extension today. If the driver were ever built without it
	// this migration would fail inside migrate(), and the daemon would refuse to
	// open every existing database -- an unrecoverable startup failure for all
	// users, traded for a convenience. It also could not decode CivArchive's
	// variant key spellings, which the Go decoder already handles.
	`
CREATE TABLE model_origin (
    sha256              TEXT NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,

    -- Part of the key, not a column: civitai and civarchive mirror each
    -- other's model ids, and annotate.go already resolves a model under both.
    -- A sha-keyed table could only record one of them.
    provider            TEXT NOT NULL,

    -- TEXT, not INTEGER, matching origin.Listing.ID/VersionID. Providers are
    -- not obliged to number their models forever, and a non-numeric id must
    -- round-trip rather than silently become 0.
    origin_model_id     TEXT NOT NULL,
    origin_version_id   TEXT NOT NULL DEFAULT '',
    origin_version_name TEXT NOT NULL DEFAULT '',

    -- 'archive' (decoded from a stored response) or 'download' (the version
    -- the user actually asked for). Recorded because the first is an inference
    -- from an archived body and the second is direct evidence; a later decoder
    -- fix should be able to redo one without touching the other.
    source              TEXT NOT NULL DEFAULT 'archive',
    updated_at          TEXT NOT NULL,

    PRIMARY KEY (sha256, provider)
) STRICT;

-- Every local copy of one upstream model in one seek. Serves the update badge
-- and version grouping equally; grouping is why this is an index on the pair
-- rather than on origin_model_id alone.
CREATE INDEX idx_model_origin_model ON model_origin (provider, origin_model_id);

CREATE TABLE origin_model_status (
    provider            TEXT NOT NULL,
    origin_model_id     TEXT NOT NULL,

    latest_version_id   TEXT NOT NULL DEFAULT '',
    latest_version_name TEXT NOT NULL DEFAULT '',
    latest_published_at TEXT NOT NULL DEFAULT '',
    latest_base_model   TEXT NOT NULL DEFAULT '',

    -- The newest version's primary file. Stored LOWER-CASE, because providers
    -- report uppercase hex (origin.RemoteFile.SHA256) and model_file.sha256 is
    -- lower -- a mixed-case comparison here would never match and the badge
    -- would never clear after the update was installed.
    --
    -- Kept mainly so ownership can be settled by CONTENT: once these bytes are
    -- indexed under any path the update has been applied, whether or not the
    -- new file has been enriched yet. That is what makes the badge
    -- self-clearing (see the view below).
    latest_file_sha256  TEXT NOT NULL DEFAULT '',
    latest_size_bytes   INTEGER NOT NULL DEFAULT 0,
    latest_download_url TEXT NOT NULL DEFAULT '',
    latest_page_url     TEXT NOT NULL DEFAULT '',

    checked_at          TEXT NOT NULL,

    -- The last check's outcome, so a sweep that could not reach the provider
    -- stays distinguishable from one that found nothing new. A failed check
    -- never clears a known latest_version_id: the same rule origin_cache
    -- applies, where a later 404 must not erase an archived body.
    http_status         INTEGER NOT NULL DEFAULT 0,
    last_error          TEXT NOT NULL DEFAULT '',

    PRIMARY KEY (provider, origin_model_id)
) STRICT;

-- "Has a newer version" as a query, not a stored flag.
--
-- A boolean column on model_origin was the obvious design and is rejected: the
-- moment the user downloads the update the flag is a lie, and every write path
-- that could make it one -- a scan indexing the new file, an enrichment
-- recording its version, a delete -- would need its own invalidation hook.
-- That is exactly the silently-stale-value pattern field_value exists to
-- prevent. Here nothing is flagged: two facts are stored (which versions are
-- owned, what the newest one is) and the answer is their comparison, so
-- applying an update clears the badge with no invalidation code anywhere.
--
-- The exclusions mirror origin.CheckUpdates one-for-one, so the badge, the CLI
-- and the browse annotator cannot drift into meaning different things:
--   1. some local file already records the latest version id;
--   2. the latest version's own file hash is already indexed, possibly under a
--      different model and not yet enriched;
--   3. this row is not the newest copy held -- keeping v3 beside v5 must not
--      badge v3 as needing v6, the same rule annotate.go's match() applies
--      when it compares only the newest owned version;
--   4. at most one row per local file, because a duplicate would multiply
--      Search's result rows and break its LIMIT. Providers mirror ids, so two
--      rows for one file are two names for one fact; lowest provider name
--      wins, arbitrarily but deterministically.
CREATE VIEW model_update AS
SELECT mo.sha256,
       mo.provider,
       mo.origin_model_id,
       mo.origin_version_id     AS have_version_id,
       mo.origin_version_name   AS have_version_name,
       s.latest_version_id,
       s.latest_version_name,
       s.latest_published_at,
       s.latest_base_model,
       s.latest_file_sha256,
       s.latest_size_bytes,
       s.latest_download_url,
       s.latest_page_url,
       s.checked_at
  FROM model_origin mo
  JOIN origin_model_status s
    ON s.provider = mo.provider
   AND s.origin_model_id = mo.origin_model_id
 WHERE s.latest_version_id <> ''
   AND NOT EXISTS (SELECT 1 FROM model_origin o
                    WHERE o.provider = mo.provider
                      AND o.origin_model_id = mo.origin_model_id
                      AND o.origin_version_id = s.latest_version_id)
   AND (s.latest_file_sha256 = ''
        OR NOT EXISTS (SELECT 1 FROM model_file mf
                        WHERE mf.sha256 = s.latest_file_sha256))
   -- CAST, because version ids are numeric strings. One divergence from
   -- origin.numericID is worth naming: SQLite CASTs a non-numeric id to 0
   -- where Go returns -1, so such an id sorts differently. Civitai version
   -- ids are always numeric; this can only bite a malformed CivArchive record.
   AND NOT EXISTS (SELECT 1 FROM model_origin o
                    WHERE o.provider = mo.provider
                      AND o.origin_model_id = mo.origin_model_id
                      AND CAST(o.origin_version_id AS INTEGER)
                        > CAST(mo.origin_version_id AS INTEGER))
   AND NOT EXISTS (SELECT 1 FROM model_origin o
                     JOIN origin_model_status s2
                       ON s2.provider = o.provider
                      AND s2.origin_model_id = o.origin_model_id
                    WHERE o.sha256 = mo.sha256
                      AND o.provider < mo.provider
                      AND s2.latest_version_id <> '');
`,

	// --- 7: copies pulled from an upstream model-manager ----------------------
	`
-- The precondition for eviction, and the only intentionally machine-local table
-- in this schema.
--
-- Deleting a local copy is defensible only when the copy is provably
-- re-derivable, and "provably" has to mean a row written by the code that
-- performed the fetch -- not an inference from a URL that happens to look like
-- an upstream one, and not the mere existence of metadata that came from
-- somewhere.
--
-- Deliberately NOT a model_origin row. That table records a fact about the
-- *content*: "this file was published as Civitai model 999 version 4567", which
-- is true of every copy of those bytes on any machine in the world. This records
-- a fact about *this instance*: "this daemon fetched this copy from that host,
-- into that path, at that time". Different key, different lifetime, and
-- model_origin has nowhere to put the path, the size or the eviction stamp.
CREATE TABLE pulled_file (
    sha256      TEXT    NOT NULL REFERENCES model_file(sha256) ON DELETE CASCADE,

    -- Base URL of the daemon this copy came from, as configured. Part of the key
    -- because one model may be pulled from two upstreams, and eviction has to
    -- know which one it can be fetched back from.
    upstream    TEXT    NOT NULL,

    -- The absolute path written, and the canonical root it sits under. Recorded
    -- rather than looked up at eviction time: "the first present path" is not
    -- the same claim, and a tier-staged copy is a second present path that must
    -- never be deletable through this route.
    --
    -- Part of the key, which is the whole point. Keyed on (sha256, upstream)
    -- alone, pulling one model into two roots made the second fetch overwrite
    -- the first's row -- and the first file could then never satisfy eviction's
    -- "is this the copy we pulled" guard, so it was undeletable for good, with
    -- a refusal claiming it was not a copy this daemon had fetched. One row per
    -- copy, and each evicts on its own.
    path        TEXT    NOT NULL,
    root        TEXT    NOT NULL,

    size_bytes  INTEGER NOT NULL,
    pulled_at   TEXT    NOT NULL,

    -- NULL while the copy is resident. Set, not deleted, on eviction: the point
    -- of the feature is that the library still knows the model is available
    -- from that upstream, and a deleted row would forget it.
    evicted_at  TEXT,

    PRIMARY KEY (sha256, upstream, path)
) STRICT;

-- "What could I free, and how much?" is the question the settings panel asks.
CREATE INDEX idx_pulled_file_resident ON pulled_file (evicted_at) WHERE evicted_at IS NULL;
`,

	// --- 8: deliberate archive intake -----------------------------------------
	`
-- Enrichment answers "what is this file I already have?", keyed by a hash we
-- computed locally. This answers a different question: acquire this model FROM
-- the provider such that the provider can vanish and nothing is lost.
--
-- The difference shows up in what it is keyed by -- a provider's own model and
-- version ids, for a file that may not exist here yet -- and in what "done"
-- means: the file, the metadata body, the preview bytes and the archived
-- response each succeed or fail on their own.
--
-- Four booleans rather than one status column, because a partial archive is the
-- normal case and "partial" is not actionable. Which part is missing is what
-- decides between retrying, waiting, and accepting it.
CREATE TABLE archive_item (
    provider     TEXT NOT NULL,

    -- TEXT for the reason model_origin gives: a provider is not obliged to
    -- number its models forever, and a non-numeric id must round-trip rather
    -- than silently become 0.
    model_id     TEXT NOT NULL,
    version_id   TEXT NOT NULL,

    -- The content hash, once the file has landed. Deliberately NOT a foreign key
    -- to model_file: the row is created before the download starts, and for a
    -- model taken down before it could be fetched the file may never arrive at
    -- all. A hard key would make the one case this table exists for -- a record
    -- of something that is gone -- the one case it cannot hold. Empty until
    -- known, matching model_origin's convention for unset ids.
    sha256       TEXT NOT NULL DEFAULT '',

    archived_at  TEXT NOT NULL,

    -- Set when the provider stopped serving THIS VERSION. Distinct from
    -- origin_model_status.upstream_gone_at, which says the whole MODEL is gone:
    -- providers remove individual versions while keeping the model, so neither
    -- fact implies the other and neither can be derived from the other.
    upstream_gone_at TEXT,

    file_ok          INTEGER NOT NULL DEFAULT 0,
    meta_ok          INTEGER NOT NULL DEFAULT 0,
    origin_cache_ok  INTEGER NOT NULL DEFAULT 0,
    previews_ok      INTEGER NOT NULL DEFAULT 0,

    -- previews_ok is a judgement; these two are the evidence behind it, kept
    -- because "3 of 12" and "0 of 0" are different situations that a single
    -- previews_ok = 0 cannot tell apart -- and the first is retryable while the
    -- second is already complete.
    previews_total   INTEGER NOT NULL DEFAULT 0,
    previews_got     INTEGER NOT NULL DEFAULT 0,

    -- Why it is partial, so a partial archive says something rather than only
    -- that it is partial.
    last_error       TEXT NOT NULL DEFAULT '',
    last_attempt_at  TEXT NOT NULL DEFAULT '',

    -- Version-keyed, because "archive this model" is not an operation: a model
    -- is a series of releases and archiving it means archiving one of them.
    PRIMARY KEY (provider, model_id, version_id)
) STRICT;

-- "What have I archived of this file?", and the reverse lookup a scan needs when
-- a file it just indexed turns out to be one an intake was waiting for.
CREATE INDEX idx_archive_item_sha ON archive_item (sha256) WHERE sha256 <> '';

-- "What is still incomplete?" -- the list the UI shows and the scheduler re-runs.
-- Partial, because the complete rows are the majority and are never the answer.
CREATE INDEX idx_archive_item_incomplete ON archive_item (provider, model_id)
    WHERE file_ok = 0 OR meta_ok = 0 OR previews_ok = 0 OR origin_cache_ok = 0;

-- Preview bytes staged before there is a file to hang them on.
--
-- preview_image has a foreign key to model_file, so a preview cannot be recorded
-- until the model has been downloaded, hashed and indexed. That is the wrong
-- constraint for an archive: a model taken down before you fetched it takes its
-- previews with it, a mirror of the metadata does not mirror the images, and a
-- preview is not reconstructible from anything else. So the bytes go into the
-- blob store at once and their identity is recorded here; the download
-- completion hook copies them into preview_image if and when the file lands.
CREATE TABLE archive_preview (
    provider     TEXT    NOT NULL,
    model_id     TEXT    NOT NULL,
    version_id   TEXT    NOT NULL,

    -- The blob store's key. No foreign key anywhere: the blob store is a
    -- directory of content-addressed files, not a table.
    image_sha256 TEXT    NOT NULL,

    -- Where it came from, so a re-run can tell "already fetched" from "never
    -- tried" without spending a request, and so the dedup between the version
    -- body's images and the gallery endpoint has something to key on.
    source_url   TEXT    NOT NULL,

    position     INTEGER NOT NULL DEFAULT 0,
    mime         TEXT    NOT NULL DEFAULT '',
    bytes        INTEGER NOT NULL DEFAULT 0,
    fetched_at   TEXT    NOT NULL,

    PRIMARY KEY (provider, model_id, version_id, image_sha256)
) STRICT;

CREATE INDEX idx_archive_preview_url ON archive_preview (source_url);

CREATE TABLE archive_watch (
    provider     TEXT    NOT NULL,
    model_id     TEXT    NOT NULL,
    added_at     TEXT    NOT NULL,

    -- Empty until the first check. Ordered on by the scheduler, so a tick cut
    -- short by a rate limit still makes forward progress across ticks -- the
    -- same resumability rule OwnedOriginModels applies, for the same reason.
    last_checked TEXT    NOT NULL DEFAULT '',

    -- Whether a newly discovered version is fetched automatically or only
    -- reported. Off by default: a watch is a subscription to information, and
    -- turning it into a subscription to unattended multi-gigabyte downloads is a
    -- separate decision that has to be made separately.
    auto_pull    INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (provider, model_id)
) STRICT;

CREATE INDEX idx_archive_watch_stale ON archive_watch (last_checked);

-- Model-level takedown, recorded where model-level facts already live.
--
-- Not a column on archive_item, because the problem it fixes is not confined to
-- archived models: OwnedOriginModels does not filter on http_status, so a model
-- a sweep has 404'd returns to the queue on every subsequent sweep -- and with
-- the API's default max age of zero, that is every model, every time, forever.
-- Nullable with no default, so every existing row reads as "not gone" without a
-- backfill.
ALTER TABLE origin_model_status ADD COLUMN upstream_gone_at TEXT;
`,
}
