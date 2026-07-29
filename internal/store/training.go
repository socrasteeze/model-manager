package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// TrainingRecord is what produced a self-trained model (spec §8).
//
// No existing tool records this at all, which is why it is arguably the
// highest-value part of the app: right now the knowledge lives in scattered
// configs and memory. Safetensors headers carry a good deal of it for LoRAs
// trained by the common toolchains, so much of this auto-populates.
type TrainingRecord struct {
	SHA256      string         `json:"sha256"`
	Dataset     string         `json:"dataset,omitempty"`
	DatasetSize int            `json:"dataset_size,omitempty"`
	Base        string         `json:"base,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	Trainer     string         `json:"trainer,omitempty"`
	Notes       string         `json:"notes,omitempty"`
	RunDate     string         `json:"run_date,omitempty"`
	Source      string         `json:"source"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
}

// UpsertTrainingRecord writes a training record.
//
// A manually entered record is never replaced by one derived from a header. The
// user has context the header does not -- what worked, what did not, which
// dataset revision -- and re-running the interpretation pass must not cost them
// those notes.
func (s *Store) UpsertTrainingRecord(tr TrainingRecord) error {
	if tr.Source != "manual" {
		var existing string
		err := s.db.QueryRow(
			`SELECT source FROM training_record WHERE sha256 = ?`, tr.SHA256).Scan(&existing)
		if err == nil && existing == "manual" {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: checking training record %s: %w", tr.SHA256, err)
		}
	}

	var config any
	if len(tr.Config) > 0 {
		b, err := json.Marshal(tr.Config)
		if err != nil {
			return fmt.Errorf("store: encoding training config: %w", err)
		}
		config = string(b)
	}
	var datasetSize any
	if tr.DatasetSize > 0 {
		datasetSize = tr.DatasetSize
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        INSERT INTO training_record (
            sha256, dataset, dataset_size, base, config, trainer, notes,
            run_date, source, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256) DO UPDATE SET
            dataset      = COALESCE(excluded.dataset, training_record.dataset),
            dataset_size = COALESCE(excluded.dataset_size, training_record.dataset_size),
            base         = COALESCE(excluded.base, training_record.base),
            config       = COALESCE(excluded.config, training_record.config),
            trainer      = COALESCE(excluded.trainer, training_record.trainer),
            notes        = COALESCE(excluded.notes, training_record.notes),
            run_date     = COALESCE(excluded.run_date, training_record.run_date),
            source       = excluded.source,
            updated_at   = excluded.updated_at`,
		tr.SHA256, nullable(tr.Dataset), datasetSize, nullable(tr.Base), config,
		nullable(tr.Trainer), nullable(tr.Notes), nullable(tr.RunDate),
		tr.Source, nowUTC())
	if err != nil {
		return fmt.Errorf("store: upsert training record %s: %w", tr.SHA256, err)
	}
	return nil
}

// GetTrainingRecord reads a training record, or nil if there is none.
func (s *Store) GetTrainingRecord(sha string) (*TrainingRecord, error) {
	var tr TrainingRecord
	var dataset, base, config, trainer, notes, runDate sql.NullString
	var datasetSize sql.NullInt64

	err := s.db.QueryRow(`
        SELECT sha256, dataset, dataset_size, base, config, trainer, notes,
               run_date, source, updated_at
          FROM training_record WHERE sha256 = ?`, sha,
	).Scan(&tr.SHA256, &dataset, &datasetSize, &base, &config, &trainer,
		&notes, &runDate, &tr.Source, &tr.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading training record %s: %w", sha, err)
	}

	tr.Dataset, tr.Base, tr.Trainer = dataset.String, base.String, trainer.String
	tr.Notes, tr.RunDate = notes.String, runDate.String
	tr.DatasetSize = int(datasetSize.Int64)
	if config.Valid {
		_ = json.Unmarshal([]byte(config.String), &tr.Config)
	}
	return &tr, nil
}

// HeaderBlob returns the stored header for a model, for a re-runnable
// interpretation pass that costs no disk reads.
func (s *Store) HeaderBlob(sha string) ([]byte, string, bool, error) {
	var blob []byte
	var format string
	var truncated int
	err := s.db.QueryRow(
		`SELECT header_blob, format, header_truncated FROM model_file WHERE sha256 = ?`, sha,
	).Scan(&blob, &format, &truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("store: reading header for %s: %w", sha, err)
	}
	return blob, format, truncated == 1, nil
}

// HeaderRow is one model's stored header plus the context an interpreter needs.
type HeaderRow struct {
	SHA256    string
	Format    string
	Blob      []byte
	Truncated bool

	// AnyPath is one path this content is known at, used for filename and
	// directory heuristics. Present paths are preferred.
	AnyPath string
}

// IterHeaders walks every model with a stored header, newest scan first.
//
// The whole point of storing headers verbatim is that interpreting them is a
// cheap re-runnable pass over the database rather than another walk of 7.5TB
// (spec §15).
func (s *Store) IterHeaders(fn func(HeaderRow) error) (int, error) {
	rows, err := s.db.Query(`
        SELECT f.sha256, f.format, f.header_blob, f.header_truncated,
               COALESCE((SELECT p.path FROM model_file_path p
                          WHERE p.sha256 = f.sha256
                          ORDER BY p.present DESC, p.id ASC LIMIT 1), '')
          FROM model_file f`)
	if err != nil {
		return 0, fmt.Errorf("store: iterating headers: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var r HeaderRow
		var truncated int
		if err := rows.Scan(&r.SHA256, &r.Format, &r.Blob, &truncated, &r.AnyPath); err != nil {
			return n, err
		}
		r.Truncated = truncated == 1
		if err := fn(r); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
