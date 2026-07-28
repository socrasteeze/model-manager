// Package report answers the question Phase 0 exists to answer: how many
// distinct models are in the library, how much of its size is duplication, and
// what the size distribution is (spec §15).
//
// That last number is the input to sizing an SSD tier, which spec §16.3 says not
// to guess at before it exists.
package report

import (
	"database/sql"
	"fmt"
	"time"
)

// Report is the full Phase 0 picture.
type Report struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Database    string         `json:"database"`
	Runs        []RunSummary   `json:"scan_runs"`
	Totals      Totals         `json:"totals"`
	Formats     []FormatRow    `json:"formats"`
	Sizes       []SizeBucket   `json:"size_distribution"`
	Duplicates  []DuplicateSet `json:"top_duplicates"`
	Health      Health         `json:"health"`
}

// RunSummary is the latest scan of one root.
type RunSummary struct {
	Root        string `json:"root"`
	ScanRunID   int64  `json:"scan_run_id"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	FilesSeen   int64  `json:"files_seen"`
	FilesHashed int64  `json:"files_hashed"`
	FilesCached int64  `json:"files_cached"`
	FilesProbed int64  `json:"files_probed"`
	BytesHashed int64  `json:"bytes_hashed"`
	Errors      int64  `json:"errors"`
}

// Totals is the headline result.
type Totals struct {
	PathsPresent int64 `json:"paths_present"`

	// FileInstances counts distinct (device, inode) pairs: the number of real
	// files on disk, so a hardlinked model is not counted twice.
	FileInstances int64 `json:"file_instances"`

	// DistinctModels is the number Phase 0 was run to produce.
	DistinctModels int64 `json:"distinct_models"`

	// BytesOnDisk is apparent usage summed over distinct inodes.
	BytesOnDisk int64 `json:"bytes_on_disk"`

	// BytesDistinct is the total if every model existed exactly once.
	BytesDistinct int64 `json:"bytes_distinct"`

	// BytesDuplicated is the difference: what a perfect dedup would reclaim.
	BytesDuplicated int64 `json:"bytes_duplicated"`
}

// FormatRow breaks the library down by container format.
type FormatRow struct {
	Format         string `json:"format"`
	DistinctModels int64  `json:"distinct_models"`
	Bytes          int64  `json:"bytes"`

	// NoWeightsHash counts models with no rebinding key -- always the whole
	// count for .ckpt/.pt, and for anything else a framing parse that failed.
	NoWeightsHash int64 `json:"no_weights_hash"`
}

// SizeBucket is one row of the size histogram.
type SizeBucket struct {
	Label          string `json:"label"`
	MinBytes       int64  `json:"min_bytes"`
	MaxBytes       int64  `json:"max_bytes"`
	DistinctModels int64  `json:"distinct_models"`
	Bytes          int64  `json:"bytes"`
}

// DuplicateSet is one hash present at more than one place on disk.
type DuplicateSet struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Instances   int64  `json:"instances"`
	Paths       int64  `json:"paths"`
	WastedBytes int64  `json:"wasted_bytes"`
	ExamplePath string `json:"example_path"`
}

// Health is what needs attention before the index can be trusted.
type Health struct {
	ProvisionalPaths int64 `json:"provisional_paths"`
	AbsentPaths      int64 `json:"absent_paths"`
	TruncatedHeaders int64 `json:"truncated_headers"`

	// FramingFailures counts safetensors/GGUF files whose framing did not parse,
	// so they have no rebinding key despite being a format that should.
	FramingFailures int64 `json:"framing_failures"`

	ErrorsByKind map[string]int64 `json:"errors_by_kind"`
}

// Size histogram bounds, chosen to separate LoRAs (tens to hundreds of MB) from
// checkpoints (multiple GB), because those are the two populations an SSD tier
// has to be sized against.
var sizeBuckets = []struct {
	label    string
	min, max int64
}{
	{"< 16 MiB", 0, 16 << 20},
	{"16 – 128 MiB", 16 << 20, 128 << 20},
	{"128 – 512 MiB", 128 << 20, 512 << 20},
	{"512 MiB – 2 GiB", 512 << 20, 2 << 30},
	{"2 – 8 GiB", 2 << 30, 8 << 30},
	{"8 – 24 GiB", 8 << 30, 24 << 30},
	{"> 24 GiB", 24 << 30, 1 << 62},
}

// Generate builds the report. topDuplicates caps the duplicate listing.
func Generate(db *sql.DB, dbPath string, topDuplicates int) (*Report, error) {
	if topDuplicates <= 0 {
		topDuplicates = 20
	}
	r := &Report{GeneratedAt: time.Now().UTC(), Database: dbPath}

	var err error
	if r.Runs, err = latestRuns(db); err != nil {
		return nil, err
	}
	if r.Totals, err = totals(db); err != nil {
		return nil, err
	}
	if r.Formats, err = formats(db); err != nil {
		return nil, err
	}
	if r.Sizes, err = sizes(db); err != nil {
		return nil, err
	}
	if r.Duplicates, err = duplicates(db, topDuplicates); err != nil {
		return nil, err
	}
	if r.Health, err = health(db); err != nil {
		return nil, err
	}
	return r, nil
}

func latestRuns(db *sql.DB) ([]RunSummary, error) {
	// The most recent run per root, whatever its status -- an interrupted run is
	// exactly the thing the reader needs to be told about.
	rows, err := db.Query(`
        SELECT scan_run_id, root, status, started_at, COALESCE(finished_at, ''),
               files_seen, files_hashed, files_cached, files_probed, bytes_hashed, errors
          FROM scan_run
         WHERE scan_run_id IN (SELECT MAX(scan_run_id) FROM scan_run GROUP BY root)
         ORDER BY root`)
	if err != nil {
		return nil, fmt.Errorf("report: scan runs: %w", err)
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		var s RunSummary
		if err := rows.Scan(&s.ScanRunID, &s.Root, &s.Status, &s.StartedAt, &s.FinishedAt,
			&s.FilesSeen, &s.FilesHashed, &s.FilesCached, &s.FilesProbed,
			&s.BytesHashed, &s.Errors); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func totals(db *sql.DB) (Totals, error) {
	var t Totals

	if err := db.QueryRow(
		`SELECT COUNT(*) FROM model_file_path WHERE present = 1`).Scan(&t.PathsPresent); err != nil {
		return t, fmt.Errorf("report: present paths: %w", err)
	}

	// Grouping by (device, inode) is what keeps a hardlinked model from being
	// counted as two files occupying two files' worth of disk.
	if err := db.QueryRow(`
        SELECT COUNT(*), COALESCE(SUM(sz), 0) FROM (
            SELECT device, inode, MAX(size) AS sz
              FROM model_file_path
             WHERE present = 1
             GROUP BY device, inode
        )`).Scan(&t.FileInstances, &t.BytesOnDisk); err != nil {
		return t, fmt.Errorf("report: file instances: %w", err)
	}

	if err := db.QueryRow(`
        SELECT COUNT(*), COALESCE(SUM(size), 0)
          FROM model_file f
         WHERE EXISTS (SELECT 1 FROM model_file_path p
                        WHERE p.sha256 = f.sha256 AND p.present = 1)`,
	).Scan(&t.DistinctModels, &t.BytesDistinct); err != nil {
		return t, fmt.Errorf("report: distinct models: %w", err)
	}

	t.BytesDuplicated = t.BytesOnDisk - t.BytesDistinct
	return t, nil
}

func formats(db *sql.DB) ([]FormatRow, error) {
	rows, err := db.Query(`
        SELECT f.format, COUNT(*), COALESCE(SUM(f.size), 0),
               SUM(CASE WHEN f.weights_sha256 IS NULL THEN 1 ELSE 0 END)
          FROM model_file f
         WHERE EXISTS (SELECT 1 FROM model_file_path p
                        WHERE p.sha256 = f.sha256 AND p.present = 1)
         GROUP BY f.format
         ORDER BY 3 DESC`)
	if err != nil {
		return nil, fmt.Errorf("report: formats: %w", err)
	}
	defer rows.Close()

	var out []FormatRow
	for rows.Next() {
		var f FormatRow
		if err := rows.Scan(&f.Format, &f.DistinctModels, &f.Bytes, &f.NoWeightsHash); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func sizes(db *sql.DB) ([]SizeBucket, error) {
	out := make([]SizeBucket, 0, len(sizeBuckets))
	for _, b := range sizeBuckets {
		bucket := SizeBucket{Label: b.label, MinBytes: b.min, MaxBytes: b.max}
		if err := db.QueryRow(`
            SELECT COUNT(*), COALESCE(SUM(size), 0)
              FROM model_file f
             WHERE f.size >= ? AND f.size < ?
               AND EXISTS (SELECT 1 FROM model_file_path p
                            WHERE p.sha256 = f.sha256 AND p.present = 1)`,
			b.min, b.max,
		).Scan(&bucket.DistinctModels, &bucket.Bytes); err != nil {
			return nil, fmt.Errorf("report: size bucket %s: %w", b.label, err)
		}
		out = append(out, bucket)
	}
	return out, nil
}

func duplicates(db *sql.DB, limit int) ([]DuplicateSet, error) {
	// Instances counts distinct inodes, not paths. Two paths on one inode are a
	// hardlink -- one file, no wasted space -- and counting them as duplication
	// would invent savings that do not exist.
	rows, err := db.Query(`
        SELECT p.sha256,
               MAX(f.size)                                        AS size,
               COUNT(DISTINCT p.device || ':' || p.inode)         AS instances,
               COUNT(*)                                           AS paths,
               MIN(p.path)                                        AS example
          FROM model_file_path p
          JOIN model_file f ON f.sha256 = p.sha256
         WHERE p.present = 1
         GROUP BY p.sha256
        HAVING instances > 1
         ORDER BY (instances - 1) * MAX(f.size) DESC
         LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("report: duplicates: %w", err)
	}
	defer rows.Close()

	var out []DuplicateSet
	for rows.Next() {
		var d DuplicateSet
		if err := rows.Scan(&d.SHA256, &d.Size, &d.Instances, &d.Paths, &d.ExamplePath); err != nil {
			return nil, err
		}
		d.WastedBytes = (d.Instances - 1) * d.Size
		out = append(out, d)
	}
	return out, rows.Err()
}

func health(db *sql.DB) (Health, error) {
	var h Health
	h.ErrorsByKind = map[string]int64{}

	scalars := []struct {
		query string
		dest  *int64
	}{
		{`SELECT COUNT(*) FROM model_file_path WHERE provisional = 1 AND present = 1`, &h.ProvisionalPaths},
		{`SELECT COUNT(*) FROM model_file_path WHERE present = 0`, &h.AbsentPaths},
		{`SELECT COUNT(*) FROM model_file WHERE header_truncated = 1`, &h.TruncatedHeaders},
		{`SELECT COUNT(*) FROM model_file
           WHERE weights_sha256 IS NULL AND format IN ('safetensors', 'gguf')`, &h.FramingFailures},
	}
	for _, s := range scalars {
		if err := db.QueryRow(s.query).Scan(s.dest); err != nil {
			return h, fmt.Errorf("report: health: %w", err)
		}
	}

	// Only errors from the latest run per root: a stale error from a scan taken
	// mid-migration is not an outstanding problem.
	rows, err := db.Query(`
        SELECT kind, COUNT(*)
          FROM scan_error
         WHERE scan_run_id IN (SELECT MAX(scan_run_id) FROM scan_run GROUP BY root)
         GROUP BY kind`)
	if err != nil {
		return h, fmt.Errorf("report: error kinds: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return h, err
		}
		h.ErrorsByKind[kind] = n
	}
	return h, rows.Err()
}
