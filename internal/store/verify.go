package store

import (
	"fmt"
)

// Operations used by verification. They deliberately do not touch scan_run:
// verification is not a scan, and attributing its writes to a scan run would
// make the per-root sweep and the report's run summary lie about what happened.

// ListPathsForVerify returns present paths to re-hash, in random order so a
// sample is a sample rather than the first N of a directory walk.
//
// limit <= 0 means every matching path.
func (s *Store) ListPathsForVerify(provisionalOnly bool, limit int) ([]FilePath, error) {
	query := `
        SELECT id, sha256, path, root, device, inode, size, mtime_ns, provisional
          FROM model_file_path
         WHERE present = 1`
	if provisionalOnly {
		query += ` AND provisional = 1`
	}
	query += ` ORDER BY RANDOM()`

	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing paths for verification: %w", err)
	}
	defer rows.Close()

	var out []FilePath
	for rows.Next() {
		var p FilePath
		var device, inode int64
		var provisional int64
		if err := rows.Scan(&p.ID, &p.SHA256, &p.Path, &p.Root,
			&device, &inode, &p.Size, &p.MtimeNs, &provisional); err != nil {
			return nil, err
		}
		p.Device = uint64(device)
		p.Inode = uint64(inode)
		p.Provisional = provisional == 1
		p.Present = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertFile records content facts without touching any path.
func (s *Store) UpsertFile(f ModelFile) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	now := nowUTC()

	var weightsSHA, weightsOff, headerBlob, headerOff any
	if f.WeightsSHA256 != "" {
		weightsSHA = f.WeightsSHA256
		weightsOff = f.WeightsOffset
	}
	if f.HeaderBlob != nil {
		headerBlob = f.HeaderBlob
		headerOff = f.HeaderOffset
	}

	_, err := s.db.Exec(`
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
		headerBlob, headerOff, boolInt(f.HeaderTruncated), now, now)
	if err != nil {
		return fmt.Errorf("store: upsert model_file %s: %w", f.SHA256, err)
	}
	return nil
}

// RebindPath points a path row at the hash a full read just proved it holds and
// clears the provisional flag.
//
// This is the operation that resolves a sampled-probe guess. If the guess was
// wrong, the wrong binding is corrected here rather than persisting -- which is
// the entire reason provisional bindings are barred from write-side decisions
// until this runs (spec §10.1).
func (s *Store) RebindPath(id int64, sha string, size, mtimeNs int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        UPDATE model_file_path
           SET sha256 = ?, size = ?, mtime_ns = ?, provisional = 0,
               present = 1, last_seen = ?
         WHERE id = ?`,
		sha, size, mtimeNs, nowUTC(), id)
	if err != nil {
		return fmt.Errorf("store: rebinding path %d: %w", id, err)
	}
	return nil
}

// SetPathAbsent marks a single path as no longer present, for a file that
// disappeared between the scan and the verification.
func (s *Store) SetPathAbsent(id int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(
		`UPDATE model_file_path SET present = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: marking path %d absent: %w", id, err)
	}
	return nil
}

// PresentPaths lists every path currently on disk, for passes that need to look
// beside model files -- sidecar ingest especially.
//
// Absent paths are excluded deliberately: a sidecar beside a path that is gone
// describes a file the index has already established is not there, and ingesting
// it would resurrect claims about it.
func (s *Store) PresentPaths() ([]FilePath, error) {
	rows, err := s.db.Query(`
        SELECT id, sha256, path, root, device, inode, size, mtime_ns, provisional
          FROM model_file_path
         WHERE present = 1
         ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("store: listing present paths: %w", err)
	}
	defer rows.Close()

	var out []FilePath
	for rows.Next() {
		var p FilePath
		var device, inode, provisional int64
		if err := rows.Scan(&p.ID, &p.SHA256, &p.Path, &p.Root,
			&device, &inode, &p.Size, &p.MtimeNs, &provisional); err != nil {
			return nil, err
		}
		p.Device, p.Inode = uint64(device), uint64(inode)
		p.Provisional = provisional == 1
		p.Present = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// PathsFor lists every known location of one model, present or not.
func (s *Store) PathsFor(sha string) ([]FilePath, error) {
	rows, err := s.db.Query(`
        SELECT id, sha256, path, root, device, inode, size, mtime_ns, present, provisional
          FROM model_file_path WHERE sha256 = ?
         ORDER BY present DESC, path`, sha)
	if err != nil {
		return nil, fmt.Errorf("store: listing paths for %s: %w", sha, err)
	}
	defer rows.Close()

	out := []FilePath{}
	for rows.Next() {
		var p FilePath
		var device, inode, present, provisional int64
		if err := rows.Scan(&p.ID, &p.SHA256, &p.Path, &p.Root,
			&device, &inode, &p.Size, &p.MtimeNs, &present, &provisional); err != nil {
			return nil, err
		}
		p.Device, p.Inode = uint64(device), uint64(inode)
		p.Present, p.Provisional = present == 1, provisional == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// ConfirmedPresentPath returns one present, non-provisional path for a model,
// along with the size recorded against its content hash.
//
// Provisional paths are excluded per §10.1: a probe-bound path is not proven to
// hold these bytes. Staging cares because it copies a file and then presents the
// copy as that model; serving cares because the client verifies the stream
// against the hash it asked for, so a probe's guess would surface as a checksum
// failure at the far end of a multi-gigabyte transfer.
//
// The size comes from model_file rather than the path row because it is the size
// of the *content*, which is what a hash-addressed reader is entitled to expect;
// the path row's size is what some earlier stat saw.
//
// Shared by the tier manager and the file endpoint deliberately. Two spellings
// of "a path I am willing to act on" would eventually disagree about
// provisional rows, and the one that got it wrong would be the one nobody was
// looking at.
func (s *Store) ConfirmedPresentPath(sha string) (FilePath, int64, error) {
	var p FilePath
	var device, inode int64
	var contentSize int64
	err := s.db.QueryRow(`
        SELECT p.id, p.sha256, p.path, p.root, p.device, p.inode,
               p.size, p.mtime_ns, f.size
          FROM model_file_path p JOIN model_file f ON f.sha256 = p.sha256
         WHERE p.sha256 = ? AND p.present = 1 AND p.provisional = 0
         ORDER BY p.id LIMIT 1`, sha).Scan(
		&p.ID, &p.SHA256, &p.Path, &p.Root, &device, &inode,
		&p.Size, &p.MtimeNs, &contentSize)
	if err != nil {
		return FilePath{}, 0, fmt.Errorf("store: no confirmed on-disk copy of %s", sha)
	}
	p.Device, p.Inode = uint64(device), uint64(inode)
	p.Present = true
	return p, contentSize, nil
}

// ScanRunSummary is one recorded scan.
type ScanRunSummary struct {
	ID          int64  `json:"scan_run_id"`
	Root        string `json:"root"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	FilesSeen   int64  `json:"files_seen"`
	FilesHashed int64  `json:"files_hashed"`
	FilesCached int64  `json:"files_cached"`
	Errors      int64  `json:"errors"`
}

// RecentScanRuns lists scans newest first.
func (s *Store) RecentScanRuns(limit int) ([]ScanRunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
        SELECT scan_run_id, root, status, started_at, COALESCE(finished_at, ''),
               files_seen, files_hashed, files_cached, errors
          FROM scan_run ORDER BY scan_run_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing scan runs: %w", err)
	}
	defer rows.Close()

	out := []ScanRunSummary{}
	for rows.Next() {
		var r ScanRunSummary
		if err := rows.Scan(&r.ID, &r.Root, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.FilesSeen, &r.FilesHashed, &r.FilesCached, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
