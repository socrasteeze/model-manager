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
}
