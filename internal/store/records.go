package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModelFile is the content-addressed record: everything true of the bytes,
// independent of where they happen to live.
type ModelFile struct {
	SHA256 string

	// WeightsSHA256 is empty when the format has no determinable weights region.
	// Callers must treat empty as "no rebinding key available", never as a value
	// (spec §2.1).
	WeightsSHA256 string
	WeightsOffset int64

	ProbeSHA256 string
	Size        int64
	Format      string

	HeaderBlob      []byte
	HeaderOffset    int64
	HeaderTruncated bool
}

// FilePath is one observed location of a ModelFile.
type FilePath struct {
	ID     int64
	SHA256 string
	Path   string
	Root   string

	Device  uint64
	Inode   uint64
	Size    int64
	MtimeNs int64

	Present     bool
	Provisional bool
	ScanRunID   int64
}

// ScanRun records one walk of one root.
type ScanRun struct {
	ID         int64
	Root       string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
}

// Scan run statuses.
const (
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// BeginScanRun opens a scan run row for root and returns its id.
func (s *Store) BeginScanRun(root string) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	res, err := s.db.Exec(
		`INSERT INTO scan_run (root, started_at, status) VALUES (?, ?, ?)`,
		root, nowUTC(), StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("store: beginning scan run: %w", err)
	}
	return res.LastInsertId()
}

// ScanCounters are the per-run tallies.
type ScanCounters struct {
	FilesSeen   int64
	FilesHashed int64
	FilesCached int64
	FilesProbed int64
	BytesHashed int64
	Errors      int64
}

// FinishScanRun closes out a scan run with its final status and counters.
func (s *Store) FinishScanRun(id int64, status string, c ScanCounters) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        UPDATE scan_run
           SET finished_at = ?, status = ?,
               files_seen = ?, files_hashed = ?, files_cached = ?,
               files_probed = ?, bytes_hashed = ?, errors = ?
         WHERE scan_run_id = ?`,
		nowUTC(), status,
		c.FilesSeen, c.FilesHashed, c.FilesCached,
		c.FilesProbed, c.BytesHashed, c.Errors, id)
	if err != nil {
		return fmt.Errorf("store: finishing scan run %d: %w", id, err)
	}
	return nil
}

// MarkInterruptedRuns closes out any run left in `running` by a previous process.
// A scan that was killed mid-walk must not be mistaken later for one that
// completed, because the present-sweep is scoped to the latest *completed* scan
// of a root (spec §6.2).
func (s *Store) MarkInterruptedRuns() (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	res, err := s.db.Exec(
		`UPDATE scan_run SET status = ?, finished_at = ? WHERE status = ?`,
		StatusInterrupted, nowUTC(), StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("store: marking interrupted runs: %w", err)
	}
	return res.RowsAffected()
}

// RecordError appends a scan error.
func (s *Store) RecordError(runID int64, path, kind, message string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO scan_error (scan_run_id, path, kind, message, occurred_at)
         VALUES (?, ?, ?, ?, ?)`,
		runID, path, kind, message, nowUTC())
	return err
}

// LookupByCacheKey resolves the incremental cache key (device, inode, size,
// mtime) to a known hash. This is the fast path that makes a rescan a stat pass
// rather than a hash pass, and it is keyed on inode rather than path precisely
// because a move within a filesystem preserves the inode and must cost nothing
// (spec §10.1).
//
// Returns ("", false) on a miss. Provisional bindings are never a cache hit --
// they are exactly the rows a later pass must confirm by full hash.
func (s *Store) LookupByCacheKey(device, inode uint64, size, mtimeNs int64) (string, bool, error) {
	var sha string
	err := s.db.QueryRow(`
        SELECT sha256 FROM model_file_path
         WHERE device = ? AND inode = ? AND size = ? AND mtime_ns = ?
           AND provisional = 0
         LIMIT 1`,
		int64(device), int64(inode), size, mtimeNs).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: cache lookup: %w", err)
	}
	return sha, true, nil
}

// LookupByProbe resolves the second-tier fallback for cross-volume copies: a new
// inode with a preserved size, matched on a sampled hash of the first and last
// 1MiB (spec §10.1).
//
// A match here NEVER confers identity. The caller must bind the path as
// provisional and let a later verification pass confirm it with a full hash. A
// false positive in a content-addressed system assigns a wrong identity
// permanently, which is the exact class of failure this design exists to
// eliminate.
func (s *Store) LookupByProbe(size int64, probe string) (string, bool, error) {
	rows, err := s.db.Query(
		`SELECT sha256 FROM model_file WHERE size = ? AND probe_sha256 = ? LIMIT 2`,
		size, probe)
	if err != nil {
		return "", false, fmt.Errorf("store: probe lookup: %w", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return "", false, err
		}
		found = append(found, sha)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	// Two distinct hashes sharing a size and a probe means the probe is not
	// discriminating here. Ambiguity resolves to a full hash, not a coin flip.
	if len(found) != 1 {
		return "", false, nil
	}
	return found[0], true, nil
}

// UpsertFileAndPath records a model file and one of its paths in a single
// transaction. Committing per file is what makes the scan restartable at any
// point (spec §15) -- an interrupted 7.5TB pass must never start over.
func (s *Store) UpsertFileAndPath(f ModelFile, p FilePath) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	now := nowUTC()

	var weightsSHA any
	var weightsOff any
	if f.WeightsSHA256 != "" {
		weightsSHA = f.WeightsSHA256
		weightsOff = f.WeightsOffset
	}
	var headerBlob any
	var headerOff any
	if f.HeaderBlob != nil {
		headerBlob = f.HeaderBlob
		headerOff = f.HeaderOffset
	}

	// last_verified always advances; first_seen never moves. A re-observation is
	// evidence the file is still there, not evidence it is new.
	if _, err := tx.Exec(`
        INSERT INTO model_file (
            sha256, weights_sha256, weights_offset, probe_sha256, size, format,
            header_blob, header_offset, header_truncated, first_seen, last_verified
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256) DO UPDATE SET
            weights_sha256   = COALESCE(excluded.weights_sha256, model_file.weights_sha256),
            weights_offset   = COALESCE(excluded.weights_offset, model_file.weights_offset),
            probe_sha256     = excluded.probe_sha256,
            format           = excluded.format,
            header_blob      = COALESCE(excluded.header_blob, model_file.header_blob),
            header_offset    = COALESCE(excluded.header_offset, model_file.header_offset),
            header_truncated = excluded.header_truncated,
            last_verified    = excluded.last_verified`,
		f.SHA256, weightsSHA, weightsOff, f.ProbeSHA256, f.Size, f.Format,
		headerBlob, headerOff, boolInt(f.HeaderTruncated), now, now,
	); err != nil {
		return fmt.Errorf("store: upsert model_file %s: %w", f.SHA256, err)
	}

	// A path is unique; re-observing it updates the instance facts. If the path
	// now holds different content, sha256 is rewritten -- the path moved to a new
	// file, and the old hash keeps whatever other paths it has.
	if _, err := tx.Exec(`
        INSERT INTO model_file_path (
            sha256, path, root, device, inode, size, mtime_ns,
            first_seen, last_seen, present, provisional, scan_run_id
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
        ON CONFLICT(path) DO UPDATE SET
            sha256      = excluded.sha256,
            root        = excluded.root,
            device      = excluded.device,
            inode       = excluded.inode,
            size        = excluded.size,
            mtime_ns    = excluded.mtime_ns,
            last_seen   = excluded.last_seen,
            present     = 1,
            provisional = excluded.provisional,
            scan_run_id = excluded.scan_run_id`,
		p.SHA256, p.Path, p.Root, int64(p.Device), int64(p.Inode), p.Size, p.MtimeNs,
		now, now, boolInt(p.Provisional), p.ScanRunID,
	); err != nil {
		return fmt.Errorf("store: upsert path %s: %w", p.Path, err)
	}

	// An orphaned model_file row -- one whose every path was rewritten to point
	// at other content -- is not deleted. The record may still be the only
	// surviving identity for a file that reappears elsewhere later, which is the
	// whole point of content addressing.

	return tx.Commit()
}

// TouchPath records a cache-hit observation without rewriting content facts.
func (s *Store) TouchPath(p FilePath) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	now := nowUTC()
	_, err := s.db.Exec(`
        INSERT INTO model_file_path (
            sha256, path, root, device, inode, size, mtime_ns,
            first_seen, last_seen, present, provisional, scan_run_id
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
        ON CONFLICT(path) DO UPDATE SET
            sha256      = excluded.sha256,
            root        = excluded.root,
            device      = excluded.device,
            inode       = excluded.inode,
            size        = excluded.size,
            mtime_ns    = excluded.mtime_ns,
            last_seen   = excluded.last_seen,
            present     = 1,
            provisional = excluded.provisional,
            scan_run_id = excluded.scan_run_id`,
		p.SHA256, p.Path, p.Root, int64(p.Device), int64(p.Inode), p.Size, p.MtimeNs,
		now, now, boolInt(p.Provisional), p.ScanRunID)
	if err != nil {
		return fmt.Errorf("store: touch path %s: %w", p.Path, err)
	}
	// Re-observing a path is also evidence about the content behind it.
	_, err = s.db.Exec(
		`UPDATE model_file SET last_verified = ? WHERE sha256 = ?`, now, p.SHA256)
	return err
}

// SweepAbsentPaths marks every path under root that this run did not observe as
// no longer present. Called only after a run completes successfully: doing it
// after a failed or interrupted walk would mark half the library missing (spec
// §6.2).
func (s *Store) SweepAbsentPaths(root string, runID int64) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	res, err := s.db.Exec(`
        UPDATE model_file_path
           SET present = 0
         WHERE root = ? AND scan_run_id != ? AND present = 1`,
		root, runID)
	if err != nil {
		return 0, fmt.Errorf("store: sweeping absent paths under %s: %w", root, err)
	}
	return res.RowsAffected()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
