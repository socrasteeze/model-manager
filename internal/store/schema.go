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
}
