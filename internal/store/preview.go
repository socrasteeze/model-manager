package store

import (
	"fmt"
)

// PreviewImage is one image attached to a model, addressed by its own content
// hash (spec §18).
type PreviewImage struct {
	ID          int64  `json:"id"`
	SHA256      string `json:"sha256"`
	ImageSHA256 string `json:"image_sha256"`
	MIME        string `json:"mime"`
	Bytes       int64  `json:"bytes"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Source      string `json:"source"`
	Position    int    `json:"position"`
	CreatedAt   string `json:"created_at,omitempty"`

	// ThumbSHA256 is a small derived copy for grid rendering. Empty means the
	// full image is all there is.
	ThumbSHA256 string `json:"thumb_sha256,omitempty"`

	// WorkflowSHA256 addresses the ComfyUI workflow JSON extracted from the
	// image, when it carried one.
	WorkflowSHA256 string `json:"workflow_sha256,omitempty"`
}

// SourceManual marks a preview the user chose. It outranks every fetched
// source, so enrichment can never displace a chosen thumbnail.
const SourceManual = "manual"

// previewOrder ranks manual previews first, then by position, then by
// insertion. Written once here so the grid, the detail panel and the search
// projection cannot disagree about which image is "the" thumbnail.
const previewOrder = `ORDER BY (source = 'manual') DESC, position ASC, id ASC`

// AddPreviewImage attaches an image to a model.
//
// Keyed on (model, image content), so ingesting the same preview twice from two
// tools' sidecars records it once. Two tools shipping the same image is the
// normal case, not an edge case.
func (s *Store) AddPreviewImage(p PreviewImage) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var width, height any
	if p.Width > 0 {
		width = p.Width
	}
	if p.Height > 0 {
		height = p.Height
	}

	_, err := s.db.Exec(`
        INSERT INTO preview_image (
            sha256, image_sha256, mime, bytes, width, height, source, position,
            created_at, thumb_sha256, workflow_sha256
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256, image_sha256) DO UPDATE SET
            mime     = excluded.mime,
            bytes    = excluded.bytes,
            width    = COALESCE(excluded.width, preview_image.width),
            height   = COALESCE(excluded.height, preview_image.height),
            position = MIN(preview_image.position, excluded.position),
            thumb_sha256 = COALESCE(excluded.thumb_sha256, preview_image.thumb_sha256),
            workflow_sha256 = COALESCE(excluded.workflow_sha256, preview_image.workflow_sha256),
            -- Once manual, always manual: re-ingesting the same bytes from a
            -- provider must not demote an image the user chose.
            source = CASE WHEN preview_image.source = 'manual'
                          THEN preview_image.source ELSE excluded.source END`,
		p.SHA256, p.ImageSHA256, p.MIME, p.Bytes, width, height,
		p.Source, p.Position, nowUTC(), nullString(p.ThumbSHA256), nullString(p.WorkflowSHA256))
	if err != nil {
		return fmt.Errorf("store: attaching preview to %s: %w", p.SHA256, err)
	}
	return nil
}

// PreviewImages lists a model's images in display order.
func (s *Store) PreviewImages(sha string) ([]PreviewImage, error) {
	rows, err := s.db.Query(`
        SELECT id, sha256, image_sha256, mime, bytes,
               COALESCE(width, 0), COALESCE(height, 0), source, position, created_at,
               COALESCE(thumb_sha256, ''), COALESCE(workflow_sha256, '')
          FROM preview_image WHERE sha256 = ?
         `+previewOrder, sha)
	if err != nil {
		return nil, fmt.Errorf("store: listing previews for %s: %w", sha, err)
	}
	defer rows.Close()

	out := []PreviewImage{}
	for rows.Next() {
		var p PreviewImage
		if err := rows.Scan(&p.ID, &p.SHA256, &p.ImageSHA256, &p.MIME, &p.Bytes,
			&p.Width, &p.Height, &p.Source, &p.Position, &p.CreatedAt,
			&p.ThumbSHA256, &p.WorkflowSHA256); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PrimaryPreview returns a model's first image, or nil.
func (s *Store) PrimaryPreview(sha string) (*PreviewImage, error) {
	images, err := s.PreviewImages(sha)
	if err != nil || len(images) == 0 {
		return nil, err
	}
	return &images[0], nil
}

// SetTags replaces a model's tags from one source.
//
// Manual tags are held separate from ingested ones: re-running an ingest must
// not delete a tag the user added by hand, and a tool dropping a tag from its
// sidecar must not silently unpick the user's organization.
func (s *Store) SetTags(sha, source string, names []string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tags: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM model_tag WHERE sha256 = ? AND source = ?`, sha, source); err != nil {
		return fmt.Errorf("store: clearing tags from %s: %w", source, err)
	}

	now := nowUTC()
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO tag (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`,
			name, now); err != nil {
			return fmt.Errorf("store: creating tag %q: %w", name, err)
		}
		if _, err := tx.Exec(`
            INSERT INTO model_tag (sha256, tag_id, source, added_at)
            VALUES (?, (SELECT id FROM tag WHERE name = ?), ?, ?)
            ON CONFLICT(sha256, tag_id) DO UPDATE SET source = excluded.source`,
			sha, name, source, now); err != nil {
			return fmt.Errorf("store: tagging %s with %q: %w", sha, name, err)
		}
	}
	return tx.Commit()
}

// Tags lists a model's tags.
func (s *Store) Tags(sha string) ([]string, error) {
	rows, err := s.db.Query(`
        SELECT t.name FROM tag t
          JOIN model_tag mt ON mt.tag_id = t.id
         WHERE mt.sha256 = ? ORDER BY t.name`, sha)
	if err != nil {
		return nil, fmt.Errorf("store: listing tags for %s: %w", sha, err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// AllTags lists every tag with how many models carry it.
func (s *Store) AllTags() (map[string]int, error) {
	rows, err := s.db.Query(`
        SELECT t.name, COUNT(mt.sha256) FROM tag t
          LEFT JOIN model_tag mt ON mt.tag_id = t.id
         GROUP BY t.id ORDER BY t.name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing tags: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// nullString maps an empty string to SQL NULL, so COALESCE-based upserts treat
// "not supplied" and "explicitly blank" the same way: keep what is there.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RemovePreviewImage detaches one image from a model.
//
// The blob itself is left alone. Blobs are content-addressed and may be shared
// by several models, so deleting bytes on a detach would silently blank another
// model's thumbnail.
func (s *Store) RemovePreviewImage(sha, imageSHA string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(
		`DELETE FROM preview_image WHERE sha256 = ? AND image_sha256 = ?`, sha, imageSHA)
	if err != nil {
		return fmt.Errorf("store: removing preview from %s: %w", sha, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: %s has no preview %s", sha, imageSHA)
	}
	return nil
}

// ReorderPreviewImages sets display order from a list of image hashes.
//
// Images not named keep their relative order after the named ones. Manual
// previews still sort ahead of fetched ones -- ordering within a tier is the
// user's to choose, the tiering is not.
func (s *Store) ReorderPreviewImages(sha string, imageSHAs []string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Push everything past the end first, so a partial list cannot collide with
	// positions it has not reassigned yet.
	if _, err := tx.Exec(
		`UPDATE preview_image SET position = position + ? WHERE sha256 = ?`,
		len(imageSHAs)+1, sha); err != nil {
		return err
	}
	for i, img := range imageSHAs {
		if _, err := tx.Exec(
			`UPDATE preview_image SET position = ? WHERE sha256 = ? AND image_sha256 = ?`,
			i, sha, img); err != nil {
			return fmt.Errorf("store: reordering preview %s: %w", img, err)
		}
	}
	return tx.Commit()
}

// PreviewByImage looks up one attachment.
func (s *Store) PreviewByImage(sha, imageSHA string) (*PreviewImage, error) {
	images, err := s.PreviewImages(sha)
	if err != nil {
		return nil, err
	}
	for i := range images {
		if images[i].ImageSHA256 == imageSHA {
			return &images[i], nil
		}
	}
	return nil, fmt.Errorf("store: %s has no preview %s", sha, imageSHA)
}

// PreviewsWithoutThumbnails lists previews that have no derived grid copy.
//
// The backfill query for `mm thumbs`. Ordered by size descending, so an
// interrupted run has already dealt with the images that cost the most to send.
func (s *Store) PreviewsWithoutThumbnails(limit int) ([]PreviewImage, error) {
	query := `
        SELECT id, sha256, image_sha256, mime, bytes,
               COALESCE(width, 0), COALESCE(height, 0), source, position, created_at,
               COALESCE(thumb_sha256, ''), COALESCE(workflow_sha256, '')
          FROM preview_image
         WHERE thumb_sha256 IS NULL OR thumb_sha256 = ''
         ORDER BY bytes DESC, id ASC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing previews without thumbnails: %w", err)
	}
	defer rows.Close()

	out := []PreviewImage{}
	for rows.Next() {
		var p PreviewImage
		if err := rows.Scan(&p.ID, &p.SHA256, &p.ImageSHA256, &p.MIME, &p.Bytes,
			&p.Width, &p.Height, &p.Source, &p.Position, &p.CreatedAt,
			&p.ThumbSHA256, &p.WorkflowSHA256); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPreviewThumbnail records a derived grid copy against a preview.
func (s *Store) SetPreviewThumbnail(sha, imageSHA, thumbSHA string, width, height int) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var w, h any
	if width > 0 {
		w = width
	}
	if height > 0 {
		h = height
	}
	_, err := s.db.Exec(`
        UPDATE preview_image
           SET thumb_sha256 = ?,
               width  = COALESCE(?, width),
               height = COALESCE(?, height)
         WHERE sha256 = ? AND image_sha256 = ?`,
		thumbSHA, w, h, sha, imageSHA)
	if err != nil {
		return fmt.Errorf("store: recording thumbnail for %s: %w", imageSHA, err)
	}
	return nil
}
