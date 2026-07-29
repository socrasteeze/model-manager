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
}
