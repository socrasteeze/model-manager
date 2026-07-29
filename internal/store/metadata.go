package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

// Metadata operations: field candidates, resolution into the materialized
// record, and the suggestions that keep a sticky manual value correctable.

// FieldObservation is one source's claim about one field, as offered by an
// ingest adapter.
type FieldObservation struct {
	Field string
	Value any // encoded to JSON on the way in
}

// RecordObservations stores what one source has to say about one model.
//
// Values are upserted per (model, field, source), so re-running an ingest
// refreshes that source's opinion and touches nothing else. A source that no
// longer mentions a field it once did leaves its old claim standing — silence is
// not a retraction, and treating it as one would let a tool erase a value by
// crashing halfway through writing its sidecar.
func (s *Store) RecordObservations(sha, source string, obs []FieldObservation) error {
	if len(obs) == 0 {
		return nil
	}
	tier := provenance.TierOf(source)
	now := nowUTC()

	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin observations: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
        INSERT INTO field_value (sha256, field, value, source, source_tier, observed_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256, field, source) DO UPDATE SET
            value       = excluded.value,
            source_tier = excluded.source_tier,
            observed_at = excluded.observed_at`)
	if err != nil {
		return fmt.Errorf("store: preparing observation insert: %w", err)
	}
	defer stmt.Close()

	for _, o := range obs {
		if o.Value == nil {
			continue
		}
		encoded, err := provenance.EncodeValue(o.Value)
		if err != nil {
			return err
		}
		// An empty string is not an observation. Adapters routinely emit blank
		// fields for keys their sidecar simply did not have, and storing those
		// would let an absence outrank a real value from a lower-trust source.
		if encoded == `""` || encoded == "null" || encoded == "[]" || encoded == "{}" {
			continue
		}
		if _, err := stmt.Exec(sha, o.Field, encoded, source, int(tier), now); err != nil {
			return fmt.Errorf("store: recording %s/%s from %s: %w", sha, o.Field, source, err)
		}
	}
	return tx.Commit()
}

// ClearManualField removes the user's value for a field so lower tiers resolve
// again.
//
// Without this, "never overwritten by ingest" would mean a mistyped value is
// unfixable by ingest forever (spec §7.1). Deleting the row rather than storing
// an empty one is deliberate: an empty manual value is itself a legitimate thing
// to want, and the two intentions must stay distinguishable.
func (s *Store) ClearManualField(sha, field string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	if _, err := s.db.Exec(
		`DELETE FROM field_value WHERE sha256 = ? AND field = ? AND source = ?`,
		sha, field, provenance.SourceManual); err != nil {
		return fmt.Errorf("store: clearing manual %s/%s: %w", sha, field, err)
	}
	// The suggestion existed only to reconcile against that manual value.
	_, err := s.db.Exec(
		`DELETE FROM suggestion WHERE sha256 = ? AND field = ? AND status = 'pending'`,
		sha, field)
	return err
}

// Candidates returns every stored opinion about one model.
func (s *Store) Candidates(sha string) ([]provenance.Candidate, error) {
	rows, err := s.db.Query(`
        SELECT field, value, source, source_tier, observed_at
          FROM field_value WHERE sha256 = ?`, sha)
	if err != nil {
		return nil, fmt.Errorf("store: reading candidates for %s: %w", sha, err)
	}
	defer rows.Close()

	var out []provenance.Candidate
	for rows.Next() {
		var c provenance.Candidate
		var tier int
		var observed string
		if err := rows.Scan(&c.Field, &c.Value, &c.Source, &tier, &observed); err != nil {
			return nil, err
		}
		c.Tier = provenance.Tier(tier)
		c.ObservedAt, _ = time.Parse(time.RFC3339Nano, observed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ModelRecord is the resolved, materialized view of one model.
type ModelRecord struct {
	SHA256              string   `json:"sha256"`
	Type                string   `json:"type,omitempty"`
	BaseModel           string   `json:"base_model,omitempty"`
	Name                string   `json:"name,omitempty"`
	Version             string   `json:"version,omitempty"`
	Description         string   `json:"description,omitempty"`
	TriggerWords        []string `json:"trigger_words,omitempty"`
	RecommendedWeight   *float64 `json:"recommended_weight,omitempty"`
	RecommendedSettings string   `json:"recommended_settings,omitempty"`
	NSFW                *bool    `json:"nsfw,omitempty"`
	Origin              string   `json:"origin,omitempty"`
	UpdatedAt           string   `json:"updated_at,omitempty"`
}

// ResolveModel re-runs resolution for one model and materializes the winners.
//
// Called after every ingest. It is idempotent and cheap, and rebuilding from
// candidates rather than mutating in place is what makes the resolution rules
// auditable: the materialized row is always exactly what the current rules say
// about the current evidence.
func (s *Store) ResolveModel(sha string) (*ModelRecord, error) {
	candidates, err := s.Candidates(sha)
	if err != nil {
		return nil, err
	}

	byField := map[string][]provenance.Candidate{}
	for _, c := range candidates {
		byField[c.Field] = append(byField[c.Field], c)
	}

	rec := &ModelRecord{SHA256: sha}
	for field, list := range byField {
		res, ok := provenance.Resolve(list)
		if !ok {
			continue
		}
		applyField(rec, field, res.Value)
	}

	if err := s.writeModelRecord(rec); err != nil {
		return nil, err
	}
	if err := s.refreshSuggestions(sha, byField); err != nil {
		return nil, err
	}
	// Search reads the materialized row, so the index has to move with it.
	// Refreshing here rather than on a schedule means a value edited in the UI
	// is findable immediately, which is the behaviour anyone expects.
	if err := s.reindexOne(sha); err != nil {
		return nil, err
	}
	return rec, nil
}

func applyField(rec *ModelRecord, field, encoded string) {
	switch field {
	case provenance.FieldType:
		rec.Type, _ = provenance.DecodeString(encoded)
	case provenance.FieldBaseModel:
		rec.BaseModel, _ = provenance.DecodeString(encoded)
	case provenance.FieldName:
		rec.Name, _ = provenance.DecodeString(encoded)
	case provenance.FieldVersion:
		rec.Version, _ = provenance.DecodeString(encoded)
	case provenance.FieldDescription:
		rec.Description, _ = provenance.DecodeString(encoded)
	case provenance.FieldTriggerWords:
		rec.TriggerWords, _ = provenance.DecodeStringSlice(encoded)
	case provenance.FieldRecommendedWeight:
		if f, ok := provenance.DecodeFloat(encoded); ok {
			rec.RecommendedWeight = &f
		}
	case provenance.FieldRecommendedSettings:
		rec.RecommendedSettings = encoded
	case provenance.FieldNSFW:
		if b, ok := provenance.DecodeBool(encoded); ok {
			rec.NSFW = &b
		}
	case provenance.FieldOrigin:
		rec.Origin, _ = provenance.DecodeString(encoded)
	}
}

func (s *Store) writeModelRecord(rec *ModelRecord) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var triggers any
	if len(rec.TriggerWords) > 0 {
		b, err := json.Marshal(rec.TriggerWords)
		if err != nil {
			return err
		}
		triggers = string(b)
	}
	var weight any
	if rec.RecommendedWeight != nil {
		weight = *rec.RecommendedWeight
	}
	var nsfw any
	if rec.NSFW != nil {
		nsfw = boolInt(*rec.NSFW)
	}

	rec.UpdatedAt = nowUTC()
	_, err := s.db.Exec(`
        INSERT INTO model_record (
            sha256, type, base_model, name, version, description,
            trigger_words, recommended_weight, recommended_settings,
            nsfw, origin, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256) DO UPDATE SET
            type = excluded.type, base_model = excluded.base_model,
            name = excluded.name, version = excluded.version,
            description = excluded.description, trigger_words = excluded.trigger_words,
            recommended_weight = excluded.recommended_weight,
            recommended_settings = excluded.recommended_settings,
            nsfw = excluded.nsfw, origin = excluded.origin,
            updated_at = excluded.updated_at`,
		rec.SHA256, nullable(rec.Type), nullable(rec.BaseModel), nullable(rec.Name),
		nullable(rec.Version), nullable(rec.Description), triggers, weight,
		nullable(rec.RecommendedSettings), nsfw, nullable(rec.Origin), rec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: materializing record %s: %w", rec.SHA256, err)
	}
	return nil
}

// refreshSuggestions keeps the pending set in step with the evidence: raise a
// suggestion where Origin now contradicts Manual, and withdraw ones whose
// disagreement has since gone away.
func (s *Store) refreshSuggestions(sha string, byField map[string][]provenance.Candidate) error {
	live := map[string]bool{}
	for _, list := range byField {
		for _, d := range provenance.FindDisagreements(list) {
			live[d.Field+"\x00"+d.Source] = true

			s.wmu.Lock()
			_, err := s.db.Exec(`
                INSERT INTO suggestion (sha256, field, manual_value, suggested_value, source, created_at, status)
                VALUES (?, ?, ?, ?, ?, ?, 'pending')
                ON CONFLICT(sha256, field, source) DO UPDATE SET
                    manual_value    = excluded.manual_value,
                    suggested_value = excluded.suggested_value,
                    -- A previously dismissed suggestion returns to pending only
                    -- if what is being suggested actually changed. Otherwise
                    -- dismissing it would achieve nothing.
                    status = CASE
                        WHEN suggestion.suggested_value != excluded.suggested_value THEN 'pending'
                        ELSE suggestion.status
                    END`,
				sha, d.Field, d.ManualValue, d.SuggestedValue, d.Source, nowUTC())
			s.wmu.Unlock()
			if err != nil {
				return fmt.Errorf("store: raising suggestion for %s/%s: %w", sha, d.Field, err)
			}
		}
	}

	// Withdraw pending suggestions whose conflict no longer exists — the manual
	// value may have been edited to agree, or cleared entirely.
	rows, err := s.db.Query(
		`SELECT field, source FROM suggestion WHERE sha256 = ? AND status = 'pending'`, sha)
	if err != nil {
		return err
	}
	var stale [][2]string
	for rows.Next() {
		var f, src string
		if err := rows.Scan(&f, &src); err != nil {
			rows.Close()
			return err
		}
		if !live[f+"\x00"+src] {
			stale = append(stale, [2]string{f, src})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, st := range stale {
		s.wmu.Lock()
		_, err := s.db.Exec(
			`DELETE FROM suggestion WHERE sha256 = ? AND field = ? AND source = ? AND status = 'pending'`,
			sha, st[0], st[1])
		s.wmu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// Suggestion is a surfaced disagreement between a manual value and an origin.
type Suggestion struct {
	ID             int64  `json:"id"`
	SHA256         string `json:"sha256"`
	Field          string `json:"field"`
	ManualValue    string `json:"manual_value"`
	SuggestedValue string `json:"suggested_value"`
	Source         string `json:"source"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}

// PendingSuggestions lists disagreements awaiting a decision.
func (s *Store) PendingSuggestions(sha string, limit int) ([]Suggestion, error) {
	query := `SELECT id, sha256, field, manual_value, suggested_value, source, status, created_at
                FROM suggestion WHERE status = 'pending'`
	args := []any{}
	if sha != "" {
		query += ` AND sha256 = ?`
		args = append(args, sha)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing suggestions: %w", err)
	}
	defer rows.Close()

	out := []Suggestion{}
	for rows.Next() {
		var sg Suggestion
		if err := rows.Scan(&sg.ID, &sg.SHA256, &sg.Field, &sg.ManualValue,
			&sg.SuggestedValue, &sg.Source, &sg.Status, &sg.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// AcceptSuggestion adopts an origin's value as the new manual value — the
// one-click accept §7.1 asks for. It stays manual afterwards, so a later ingest
// still cannot move it.
func (s *Store) AcceptSuggestion(id int64) error {
	var sha, field, value string
	err := s.db.QueryRow(
		`SELECT sha256, field, suggested_value FROM suggestion WHERE id = ?`, id,
	).Scan(&sha, &field, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: suggestion %d not found", id)
	}
	if err != nil {
		return err
	}

	s.wmu.Lock()
	_, err = s.db.Exec(`
        INSERT INTO field_value (sha256, field, value, source, source_tier, observed_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256, field, source) DO UPDATE SET
            value = excluded.value, observed_at = excluded.observed_at`,
		sha, field, value, provenance.SourceManual, int(provenance.TierManual), nowUTC())
	if err == nil {
		_, err = s.db.Exec(`UPDATE suggestion SET status = 'accepted' WHERE id = ?`, id)
	}
	s.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("store: accepting suggestion %d: %w", id, err)
	}

	_, err = s.ResolveModel(sha)
	return err
}

// DismissSuggestion keeps the manual value and stops asking, until the origin
// changes its mind to something new.
func (s *Store) DismissSuggestion(id int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE suggestion SET status = 'dismissed' WHERE id = ?`, id)
	return err
}

// GetModelRecord reads the materialized row.
func (s *Store) GetModelRecord(sha string) (*ModelRecord, error) {
	var rec ModelRecord
	var typ, base, name, version, desc, triggers, settings, origin sql.NullString
	var weight sql.NullFloat64
	var nsfw sql.NullInt64

	err := s.db.QueryRow(`
        SELECT sha256, type, base_model, name, version, description,
               trigger_words, recommended_weight, recommended_settings,
               nsfw, origin, updated_at
          FROM model_record WHERE sha256 = ?`, sha,
	).Scan(&rec.SHA256, &typ, &base, &name, &version, &desc,
		&triggers, &weight, &settings, &nsfw, &origin, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading record %s: %w", sha, err)
	}

	rec.Type, rec.BaseModel, rec.Name = typ.String, base.String, name.String
	rec.Version, rec.Description, rec.Origin = version.String, desc.String, origin.String
	rec.RecommendedSettings = settings.String
	if triggers.Valid {
		_ = json.Unmarshal([]byte(triggers.String), &rec.TriggerWords)
	}
	if weight.Valid {
		w := weight.Float64
		rec.RecommendedWeight = &w
	}
	if nsfw.Valid {
		b := nsfw.Int64 == 1
		rec.NSFW = &b
	}
	return &rec, nil
}

// ResolveAll re-materializes every model that has candidates. Used after a
// resolution-rule change or a bulk ingest.
func (s *Store) ResolveAll(progress func(done int)) (int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT sha256 FROM field_value`)
	if err != nil {
		return 0, err
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for i, sha := range shas {
		if _, err := s.ResolveModel(sha); err != nil {
			return i, err
		}
		if progress != nil && (i+1)%500 == 0 {
			progress(i + 1)
		}
	}
	return len(shas), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ReplaceObservations is RecordObservations for a source that recomputes
// everything it knows on every run.
//
// RecordObservations treats silence as non-retraction: a source that stops
// mentioning a field leaves its previous claim standing, so a tool cannot erase
// a value by crashing halfway through writing its sidecar. That is right for
// external sidecars and APIs, whose absence of a field is not evidence.
//
// It is wrong for our own derived sources -- the header interpreter and the path
// heuristics -- which see complete input every time. There, silence IS a
// retraction: if an improved rule stops producing a value, the old one is not a
// surviving opinion, it is a stale artifact of the previous rule, and leaving it
// in place means an interpretation bug can never be fully fixed.
func (s *Store) ReplaceObservations(sha, source string, obs []FieldObservation) error {
	keep := make(map[string]bool, len(obs))
	for _, o := range obs {
		keep[o.Field] = true
	}

	s.wmu.Lock()
	rows, err := s.db.Query(
		`SELECT field FROM field_value WHERE sha256 = ? AND source = ?`, sha, source)
	if err != nil {
		s.wmu.Unlock()
		return fmt.Errorf("store: reading prior observations: %w", err)
	}
	var stale []string
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			rows.Close()
			s.wmu.Unlock()
			return err
		}
		if !keep[field] {
			stale = append(stale, field)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.wmu.Unlock()
		return err
	}

	for _, field := range stale {
		if _, err := s.db.Exec(
			`DELETE FROM field_value WHERE sha256 = ? AND source = ? AND field = ?`,
			sha, source, field); err != nil {
			s.wmu.Unlock()
			return fmt.Errorf("store: retracting %s/%s: %w", sha, field, err)
		}
	}
	s.wmu.Unlock()

	return s.RecordObservations(sha, source, obs)
}
