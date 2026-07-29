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
}

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
            sha256, image_sha256, mime, bytes, width, height, source, position, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256, image_sha256) DO UPDATE SET
            mime     = excluded.mime,
            bytes    = excluded.bytes,
            width    = COALESCE(excluded.width, preview_image.width),
            height   = COALESCE(excluded.height, preview_image.height),
            position = MIN(preview_image.position, excluded.position)`,
		p.SHA256, p.ImageSHA256, p.MIME, p.Bytes, width, height,
		p.Source, p.Position, nowUTC())
	if err != nil {
		return fmt.Errorf("store: attaching preview to %s: %w", p.SHA256, err)
	}
	return nil
}

// PreviewImages lists a model's images in display order.
func (s *Store) PreviewImages(sha string) ([]PreviewImage, error) {
	rows, err := s.db.Query(`
        SELECT id, sha256, image_sha256, mime, bytes,
               COALESCE(width, 0), COALESCE(height, 0), source, position, created_at
          FROM preview_image WHERE sha256 = ?
         ORDER BY position ASC, id ASC`, sha)
	if err != nil {
		return nil, fmt.Errorf("store: listing previews for %s: %w", sha, err)
	}
	defer rows.Close()

	out := []PreviewImage{}
	for rows.Next() {
		var p PreviewImage
		if err := rows.Scan(&p.ID, &p.SHA256, &p.ImageSHA256, &p.MIME, &p.Bytes,
			&p.Width, &p.Height, &p.Source, &p.Position, &p.CreatedAt); err != nil {
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
