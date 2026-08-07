package store

// Deliberate archive intake.
//
// Everything else in this package records what is already here. These tables
// record an intention and how far it got: fetch this model from that provider,
// completely enough that the provider can vanish and nothing is lost.
//
// The shape follows from one fact -- a partial archive is normal. A provider
// rate-limits, a preview is a video the blob store will not take, a 12 GB file
// is still transferring. So completeness is four independent booleans rather
// than a status, and every step records as it finishes rather than at the end.

import (
	"database/sql"
	"fmt"
	"strings"
)

// ArchiveItem is one archived model version and how complete it is.
type ArchiveItem struct {
	Provider  string `json:"provider"`
	ModelID   string `json:"model_id"`
	VersionID string `json:"version_id"`

	// SHA256 is empty until the file has landed and been hashed.
	SHA256     string `json:"sha256,omitempty"`
	ArchivedAt string `json:"archived_at"`

	// UpstreamGoneAt is set when the provider stopped serving this version.
	// This is the state the whole feature exists for.
	UpstreamGoneAt string `json:"upstream_gone_at,omitempty"`

	FileOK        bool `json:"file_ok"`
	MetaOK        bool `json:"meta_ok"`
	OriginCacheOK bool `json:"origin_cache_ok"`
	PreviewsOK    bool `json:"previews_ok"`

	PreviewsTotal int `json:"previews_total"`
	PreviewsGot   int `json:"previews_got"`

	LastError     string `json:"last_error,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
}

// Complete reports whether every part of the archive succeeded.
func (a ArchiveItem) Complete() bool {
	return a.FileOK && a.MetaOK && a.OriginCacheOK && a.PreviewsOK
}

// Gone reports whether the provider has stopped serving this version.
func (a ArchiveItem) Gone() bool { return a.UpstreamGoneAt != "" }

// ArchivePreview is preview bytes held for a model that may not exist here yet.
type ArchivePreview struct {
	Provider    string `json:"provider"`
	ModelID     string `json:"model_id"`
	VersionID   string `json:"version_id"`
	ImageSHA256 string `json:"image_sha256"`
	SourceURL   string `json:"source_url"`
	Position    int    `json:"position"`
	MIME        string `json:"mime,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
	FetchedAt   string `json:"fetched_at"`
}

// ArchiveWatch is a model whose new versions should be noticed.
type ArchiveWatch struct {
	Provider    string `json:"provider"`
	ModelID     string `json:"model_id"`
	AddedAt     string `json:"added_at"`
	LastChecked string `json:"last_checked,omitempty"`
	AutoPull    bool   `json:"auto_pull"`
}

// PutArchiveItem creates or refreshes an intake record.
//
// Only the identity and the attempt stamp are written here; the completeness
// flags are owned by MarkArchive* and are deliberately left alone, so starting a
// re-run cannot erase what a previous run achieved.
func (s *Store) PutArchiveItem(a ArchiveItem) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        INSERT INTO archive_item (provider, model_id, version_id, sha256, archived_at, last_attempt_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(provider, model_id, version_id) DO UPDATE SET
            last_attempt_at = excluded.last_attempt_at`,
		a.Provider, a.ModelID, a.VersionID, a.SHA256, nowUTC(), nowUTC())
	if err != nil {
		return fmt.Errorf("store: recording archive item %s/%s: %w", a.Provider, a.ModelID, err)
	}
	return nil
}

// MarkArchiveStep records that one part of an intake succeeded.
//
// One method rather than four, with the column named by the caller from a fixed
// set, because the four are identical in everything but the name and four copies
// would drift.
func (s *Store) MarkArchiveStep(provider, modelID, versionID, step string) error {
	col, ok := map[string]string{
		"file":         "file_ok",
		"meta":         "meta_ok",
		"origin_cache": "origin_cache_ok",
		"previews":     "previews_ok",
	}[step]
	if !ok {
		return fmt.Errorf("store: unknown archive step %q", step)
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE archive_item SET `+col+` = 1
          WHERE provider = ? AND model_id = ? AND version_id = ?`,
		provider, modelID, versionID)
	if err != nil {
		return fmt.Errorf("store: marking archive %s: %w", step, err)
	}
	return nil
}

// SetArchiveFile records the hash of the file that landed.
func (s *Store) SetArchiveFile(provider, modelID, versionID, sha string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE archive_item SET sha256 = ?, file_ok = 1
          WHERE provider = ? AND model_id = ? AND version_id = ?`,
		strings.ToLower(sha), provider, modelID, versionID)
	return err
}

// SetArchivePreviewCounts records how many previews were found and stored.
//
// previews_ok is derived here rather than asserted by the caller: it is exactly
// "we got everything we saw", including the case where there was nothing to get.
// A version with no images is complete, not pending forever.
func (s *Store) SetArchivePreviewCounts(provider, modelID, versionID string, total, got int) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	ok := 0
	if got >= total {
		ok = 1
	}
	_, err := s.db.Exec(
		`UPDATE archive_item SET previews_total = ?, previews_got = ?, previews_ok = ?
          WHERE provider = ? AND model_id = ? AND version_id = ?`,
		total, got, ok, provider, modelID, versionID)
	return err
}

// SetArchiveError records why an intake is incomplete.
func (s *Store) SetArchiveError(provider, modelID, versionID, msg string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE archive_item SET last_error = ?, last_attempt_at = ?
          WHERE provider = ? AND model_id = ? AND version_id = ?`,
		msg, nowUTC(), provider, modelID, versionID)
	return err
}

// MarkArchiveVersionGone records that the provider stopped serving this version.
//
// Idempotent by only writing when currently NULL, so it records when the
// takedown was first seen rather than when it was last confirmed -- the first
// date is the one worth keeping, and re-confirming should not overwrite it.
func (s *Store) MarkArchiveVersionGone(provider, modelID, versionID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE archive_item SET upstream_gone_at = ?
          WHERE provider = ? AND model_id = ? AND version_id = ? AND upstream_gone_at IS NULL`,
		nowUTC(), provider, modelID, versionID)
	return err
}

// ArchiveItemFor returns one intake record, or nil.
func (s *Store) ArchiveItemFor(provider, modelID, versionID string) (*ArchiveItem, error) {
	rows, err := s.db.Query(archiveItemSelect+`
         WHERE provider = ? AND model_id = ? AND version_id = ?`,
		provider, modelID, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanArchiveItems(rows)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return &items[0], nil
}

// ArchiveItemsForFile returns the intake records covering one content hash.
//
// Usually none or one. More than one happens when the same bytes were published
// as two versions, which providers do more often than they admit.
func (s *Store) ArchiveItemsForFile(sha string) ([]ArchiveItem, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, nil
	}
	rows, err := s.db.Query(archiveItemSelect+
		` WHERE sha256 = ? ORDER BY provider, model_id, version_id`, strings.ToLower(sha))
	if err != nil {
		return nil, fmt.Errorf("store: listing archive records for %s: %w", sha, err)
	}
	defer rows.Close()
	return scanArchiveItems(rows)
}

// ArchiveItemsQuery filters the inventory.
type ArchiveItemsQuery struct {
	// Incomplete restricts to items with a step still outstanding.
	Incomplete bool

	// Gone restricts to items whose upstream has removed the version. This is
	// the "what did I save that no longer exists" view, which is the archive's
	// reason to exist and therefore a first-class filter rather than a query
	// somebody has to know how to write.
	Gone  bool
	Limit int
}

// ArchiveItems lists the inventory, most recently attempted first.
func (s *Store) ArchiveItems(q ArchiveItemsQuery) ([]ArchiveItem, error) {
	where := []string{"1 = 1"}
	if q.Incomplete {
		where = append(where,
			"(file_ok = 0 OR meta_ok = 0 OR previews_ok = 0 OR origin_cache_ok = 0)")
	}
	if q.Gone {
		where = append(where, "upstream_gone_at IS NOT NULL")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(archiveItemSelect+
		` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY archived_at DESC, model_id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing archive items: %w", err)
	}
	defer rows.Close()
	return scanArchiveItems(rows)
}

const archiveItemSelect = `
    SELECT provider, model_id, version_id, sha256, archived_at, upstream_gone_at,
           file_ok, meta_ok, origin_cache_ok, previews_ok,
           previews_total, previews_got, last_error, last_attempt_at
      FROM archive_item`

func scanArchiveItems(rows *sql.Rows) ([]ArchiveItem, error) {
	out := []ArchiveItem{}
	for rows.Next() {
		var a ArchiveItem
		var gone sql.NullString
		var fileOK, metaOK, cacheOK, previewsOK int
		if err := rows.Scan(&a.Provider, &a.ModelID, &a.VersionID, &a.SHA256,
			&a.ArchivedAt, &gone, &fileOK, &metaOK, &cacheOK, &previewsOK,
			&a.PreviewsTotal, &a.PreviewsGot, &a.LastError, &a.LastAttemptAt); err != nil {
			return nil, err
		}
		if gone.Valid {
			a.UpstreamGoneAt = gone.String
		}
		a.FileOK, a.MetaOK = fileOK == 1, metaOK == 1
		a.OriginCacheOK, a.PreviewsOK = cacheOK == 1, previewsOK == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// PutArchivePreview records staged preview bytes.
func (s *Store) PutArchivePreview(p ArchivePreview) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`
        INSERT INTO archive_preview
            (provider, model_id, version_id, image_sha256, source_url, position, mime, bytes, fetched_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(provider, model_id, version_id, image_sha256) DO UPDATE SET
            source_url = excluded.source_url,
            position   = excluded.position,
            mime       = excluded.mime,
            bytes      = excluded.bytes`,
		p.Provider, p.ModelID, p.VersionID, p.ImageSHA256, p.SourceURL,
		p.Position, p.MIME, p.Bytes, nowUTC())
	if err != nil {
		return fmt.Errorf("store: recording archived preview: %w", err)
	}
	return nil
}

// ArchivePreviews lists staged previews for one version, in order.
func (s *Store) ArchivePreviews(provider, modelID, versionID string) ([]ArchivePreview, error) {
	rows, err := s.db.Query(`
        SELECT provider, model_id, version_id, image_sha256, source_url,
               position, mime, bytes, fetched_at
          FROM archive_preview
         WHERE provider = ? AND model_id = ? AND version_id = ?
         ORDER BY position, image_sha256`, provider, modelID, versionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing archived previews: %w", err)
	}
	defer rows.Close()

	out := []ArchivePreview{}
	for rows.Next() {
		var p ArchivePreview
		if err := rows.Scan(&p.Provider, &p.ModelID, &p.VersionID, &p.ImageSHA256,
			&p.SourceURL, &p.Position, &p.MIME, &p.Bytes, &p.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ArchivedPreviewURLs returns the source URLs already fetched for a version, so
// a re-run can skip them without spending a request to find out.
func (s *Store) ArchivedPreviewURLs(provider, modelID, versionID string) (map[string]string, error) {
	previews, err := s.ArchivePreviews(provider, modelID, versionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(previews))
	for _, p := range previews {
		out[p.SourceURL] = p.ImageSHA256
	}
	return out, nil
}

// PutArchiveWatch adds or updates a watched model.
func (s *Store) PutArchiveWatch(w ArchiveWatch) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	auto := 0
	if w.AutoPull {
		auto = 1
	}
	_, err := s.db.Exec(`
        INSERT INTO archive_watch (provider, model_id, added_at, last_checked, auto_pull)
        VALUES (?, ?, ?, '', ?)
        ON CONFLICT(provider, model_id) DO UPDATE SET auto_pull = excluded.auto_pull`,
		w.Provider, w.ModelID, nowUTC(), auto)
	if err != nil {
		return fmt.Errorf("store: watching %s/%s: %w", w.Provider, w.ModelID, err)
	}
	return nil
}

// RemoveArchiveWatch stops watching a model.
func (s *Store) RemoveArchiveWatch(provider, modelID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`DELETE FROM archive_watch WHERE provider = ? AND model_id = ?`, provider, modelID)
	return err
}

// MarkArchiveWatchChecked records that a watched model was looked at.
func (s *Store) MarkArchiveWatchChecked(provider, modelID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE archive_watch SET last_checked = ? WHERE provider = ? AND model_id = ?`,
		nowUTC(), provider, modelID)
	return err
}

// ArchiveWatches lists watched models, least recently checked first.
//
// That order is what makes a sweep resumable: a run cut short by a rate limit
// leaves the models it reached at the back of the queue, so the next run
// continues rather than starting over at the same head. The same rule
// OwnedOriginModels applies, for the same reason.
func (s *Store) ArchiveWatches(limit int) ([]ArchiveWatch, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
        SELECT provider, model_id, added_at, last_checked, auto_pull
          FROM archive_watch
         ORDER BY last_checked ASC, model_id
         LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing archive watches: %w", err)
	}
	defer rows.Close()

	out := []ArchiveWatch{}
	for rows.Next() {
		var w ArchiveWatch
		var auto int
		if err := rows.Scan(&w.Provider, &w.ModelID, &w.AddedAt, &w.LastChecked, &auto); err != nil {
			return nil, err
		}
		w.AutoPull = auto == 1
		out = append(out, w)
	}
	return out, rows.Err()
}

// ArchiveCounts summarizes the inventory for the settings panel.
type ArchiveCounts struct {
	Items      int `json:"items"`
	Complete   int `json:"complete"`
	Incomplete int `json:"incomplete"`
	Gone       int `json:"gone"`
	Watched    int `json:"watched"`
}

// ArchiveSummary counts the inventory.
func (s *Store) ArchiveSummary() (*ArchiveCounts, error) {
	var c ArchiveCounts
	err := s.db.QueryRow(`
        SELECT COUNT(*),
               SUM(CASE WHEN file_ok = 1 AND meta_ok = 1 AND previews_ok = 1
                             AND origin_cache_ok = 1 THEN 1 ELSE 0 END),
               SUM(CASE WHEN upstream_gone_at IS NOT NULL THEN 1 ELSE 0 END)
          FROM archive_item`).Scan(&c.Items, &nullableInt{&c.Complete}, &nullableInt{&c.Gone})
	if err != nil {
		return nil, fmt.Errorf("store: summarizing the archive: %w", err)
	}
	c.Incomplete = c.Items - c.Complete
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM archive_watch`).Scan(&c.Watched); err != nil {
		return nil, err
	}
	return &c, nil
}

// nullableInt scans a SUM over zero rows, which SQLite returns as NULL.
type nullableInt struct{ dst *int }

func (n *nullableInt) Scan(v any) error {
	if v == nil {
		*n.dst = 0
		return nil
	}
	switch t := v.(type) {
	case int64:
		*n.dst = int(t)
	case float64:
		*n.dst = int(t)
	}
	return nil
}
