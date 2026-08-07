package store

// Persisted origin identity and upstream update status.
//
// Two facts, deliberately kept apart because they have different keys and
// different lifetimes:
//
//   - Identity is per (local file, provider): "this file was published as
//     Civitai model 999 version 4567". One local file can be known under two
//     mirrored provider namespaces, which is why provider is part of the key.
//   - Upstream status is per (provider, model): "model 999's newest version is
//     5000". That is one fact about one remote model; a library holding v3 and
//     v5 of it would otherwise store the answer twice and be able to disagree
//     with itself.
//
// Nothing here stores "needs an update". That is the comparison of the two,
// evaluated by the model_update view on every read -- see the migration for
// why a stored flag was rejected.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Where a model_origin row came from.
const (
	// OriginSourceArchive means it was decoded from a stored provider response.
	OriginSourceArchive = "archive"

	// OriginSourceDownload means the user asked for this exact version, which
	// is direct evidence rather than an inference from an archived body.
	OriginSourceDownload = "download"
)

// ModelOrigin is which upstream model and version a local file was published
// as.
type ModelOrigin struct {
	SHA256      string `json:"sha256"`
	Provider    string `json:"provider"`
	ModelID     string `json:"origin_model_id"`
	VersionID   string `json:"origin_version_id,omitempty"`
	VersionName string `json:"origin_version_name,omitempty"`
	Source      string `json:"source,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// PutModelOrigin records what a local file was published as.
//
// Hashes are lower-cased on the way in. Providers report uppercase hex and
// model_file.sha256 is lower; a mixed-case row here would satisfy the foreign
// key against nothing and join to nothing.
func (s *Store) PutModelOrigin(o ModelOrigin) error {
	if o.SHA256 == "" || o.Provider == "" || o.ModelID == "" {
		return fmt.Errorf("store: model origin needs a sha, a provider and a model id")
	}
	if o.Source == "" {
		o.Source = OriginSourceArchive
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        INSERT INTO model_origin (
            sha256, provider, origin_model_id, origin_version_id,
            origin_version_name, source, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(sha256, provider) DO UPDATE SET
            origin_model_id     = excluded.origin_model_id,
            origin_version_id   = excluded.origin_version_id,
            -- Silence is not a retraction: a decoder that could not find a
            -- version name this time must not erase one recorded earlier.
            origin_version_name = CASE WHEN excluded.origin_version_name = ''
                                       THEN model_origin.origin_version_name
                                       ELSE excluded.origin_version_name END,
            -- Direct evidence outranks an inference from an archived body, and
            -- must not be downgraded by a later archive pass.
            source              = CASE WHEN model_origin.source = 'download'
                                       THEN model_origin.source
                                       ELSE excluded.source END,
            updated_at          = excluded.updated_at`,
		strings.ToLower(o.SHA256), o.Provider, o.ModelID, o.VersionID,
		o.VersionName, o.Source, nowUTC())
	if err != nil {
		return fmt.Errorf("store: recording origin for %s: %w", short(o.SHA256), err)
	}
	return nil
}

// ModelOrigins lists what one local file is known as, across providers.
func (s *Store) ModelOrigins(sha string) ([]ModelOrigin, error) {
	rows, err := s.db.Query(`
        SELECT sha256, provider, origin_model_id, origin_version_id,
               origin_version_name, source, updated_at
          FROM model_origin WHERE sha256 = ? ORDER BY provider`,
		strings.ToLower(sha))
	if err != nil {
		return nil, fmt.Errorf("store: reading origins for %s: %w", short(sha), err)
	}
	defer rows.Close()

	out := []ModelOrigin{}
	for rows.Next() {
		var o ModelOrigin
		if err := rows.Scan(&o.SHA256, &o.Provider, &o.ModelID, &o.VersionID,
			&o.VersionName, &o.Source, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OriginModelStatus is what a provider last said about a remote model.
type OriginModelStatus struct {
	Provider string
	ModelID  string

	LatestVersionID   string
	LatestVersionName string
	LatestPublishedAt string
	LatestBaseModel   string
	LatestFileSHA256  string
	LatestSizeBytes   int64
	LatestDownloadURL string
	LatestPageURL     string

	HTTPStatus int
	LastError  string
}

// PutOriginModelStatus records a successful check.
//
// Every latest_* column is written through a CASE that keeps the stored value
// when the incoming one is blank. A check that was rate-limited, timed out or
// 404'd tells you nothing new about what the newest version is, and
// overwriting a known answer with "" would silently retract the badge and read
// as "up to date" -- the same never-retract rule origin_cache applies to an
// archived body.
func (s *Store) PutOriginModelStatus(st OriginModelStatus) error {
	if st.Provider == "" || st.ModelID == "" {
		return fmt.Errorf("store: origin model status needs a provider and a model id")
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        INSERT INTO origin_model_status (
            provider, origin_model_id, latest_version_id, latest_version_name,
            latest_published_at, latest_base_model, latest_file_sha256,
            latest_size_bytes, latest_download_url, latest_page_url,
            checked_at, http_status, last_error
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(provider, origin_model_id) DO UPDATE SET
            latest_version_id   = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_version_id
                                       ELSE excluded.latest_version_id END,
            latest_version_name = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_version_name
                                       ELSE excluded.latest_version_name END,
            latest_published_at = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_published_at
                                       ELSE excluded.latest_published_at END,
            latest_base_model   = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_base_model
                                       ELSE excluded.latest_base_model END,
            latest_file_sha256  = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_file_sha256
                                       ELSE excluded.latest_file_sha256 END,
            latest_size_bytes   = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_size_bytes
                                       ELSE excluded.latest_size_bytes END,
            latest_download_url = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_download_url
                                       ELSE excluded.latest_download_url END,
            latest_page_url     = CASE WHEN excluded.latest_version_id = ''
                                       THEN origin_model_status.latest_page_url
                                       ELSE excluded.latest_page_url END,
            checked_at  = excluded.checked_at,
            http_status = excluded.http_status,
            last_error  = excluded.last_error`,
		st.Provider, st.ModelID, st.LatestVersionID, st.LatestVersionName,
		st.LatestPublishedAt, st.LatestBaseModel, strings.ToLower(st.LatestFileSHA256),
		st.LatestSizeBytes, st.LatestDownloadURL, st.LatestPageURL,
		nowUTC(), st.HTTPStatus, st.LastError)
	if err != nil {
		return fmt.Errorf("store: recording status for %s/%s: %w", st.Provider, st.ModelID, err)
	}
	return nil
}

// MarkOriginModelChecked advances the timestamp for a check that produced no
// new answer -- a network failure, a rate limit, or a 404 meaning the model was
// removed upstream.
//
// Deliberately not the same thing as recording an empty status: it must leave
// every latest_* column alone. A model taken down still has a newer version
// published somewhere, and blanking it here would clear a correct badge.
func (s *Store) MarkOriginModelChecked(provider, modelID string, httpStatus int, errMsg string) error {
	return s.PutOriginModelStatus(OriginModelStatus{
		Provider: provider, ModelID: modelID,
		HTTPStatus: httpStatus, LastError: errMsg,
	})
}

// MarkOriginModelGone records that a provider has stopped serving a model.
//
// Idempotent by only writing when currently NULL, so it records when the model
// was FIRST seen gone rather than when it was last confirmed. The same
// never-retract rule PutOriginModelStatus follows: every latest_* column is left
// alone, because a model removed upstream may still have had a newer version
// published before it went, and blanking a known answer would clear a correct
// badge.
func (s *Store) MarkOriginModelGone(provider, modelID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	// The status row may not exist yet -- a model 404'd on its very first check
	// has never been recorded. Insert with the stamp, or set it if unset.
	_, err := s.db.Exec(`
        INSERT INTO origin_model_status (provider, origin_model_id, checked_at, http_status, upstream_gone_at)
        VALUES (?, ?, ?, 404, ?)
        ON CONFLICT(provider, origin_model_id) DO UPDATE SET
            upstream_gone_at = COALESCE(origin_model_status.upstream_gone_at, excluded.upstream_gone_at)`,
		provider, modelID, nowUTC(), nowUTC())
	if err != nil {
		return fmt.Errorf("store: marking %s/%s gone: %w", provider, modelID, err)
	}
	return nil
}

// SHAsForOriginModel lists the local files published as one upstream model.
//
// The inverse of the identity table's usual direction. Needed when something
// happens to a remote model -- a takedown, most usefully -- and the question
// becomes which local files that fact is about.
func (s *Store) SHAsForOriginModel(provider, modelID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT sha256 FROM model_origin
          WHERE provider = ? AND origin_model_id = ? ORDER BY sha256`,
		provider, modelID)
	if err != nil {
		return nil, fmt.Errorf("store: listing files for %s/%s: %w", provider, modelID, err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// OriginModelGone reports whether a model is recorded as removed upstream.
func (s *Store) OriginModelGone(provider, modelID string) (bool, error) {
	var gone sql.NullString
	err := s.db.QueryRow(
		`SELECT upstream_gone_at FROM origin_model_status
          WHERE provider = ? AND origin_model_id = ?`, provider, modelID).Scan(&gone)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return gone.Valid && gone.String != "", nil
}

// OwnedModel is one upstream model the library holds a version of.
type OwnedModel struct {
	Provider        string
	ModelID         string
	OwnedVersionIDs []string
	CheckedAt       string
}

// OwnedOriginModels lists the upstream models worth asking about.
//
// Ordered least-recently-checked first, never-checked first of all. A run
// capped by limit -- or cut short by a rate limit -- then makes forward
// progress across re-runs, instead of re-asking the head of the same list every
// time. (CheckUpdates sorts lexically for the same resumability reason;
// ordering by when we last asked is strictly better now that it is recorded.)
//
// maxAge skips models checked more recently than that. Zero checks everything.
//
// recheckGone re-queues models the provider has stopped serving. Off is the
// right default, and the reason is arithmetic: a 404 only advances checked_at,
// and maxAge defaults to zero so checked_at is not consulted either -- so
// without this filter a model taken down once is re-asked on every sweep for the
// life of the library. On a library that has outlived a few takedowns, that is
// the slowest part of every sweep, spent re-confirming something that will not
// change. Turning it on is how an operator asks "did any of them come back".
func (s *Store) OwnedOriginModels(provider string, maxAge time.Duration, limit int, recheckGone bool) ([]OwnedModel, error) {
	args := []any{provider}
	cutoff := ""
	if maxAge > 0 {
		cutoff = time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	}

	// COALESCE so a model with no status row sorts first: never asked is the
	// most stale a thing can be.
	query := `
        SELECT mo.origin_model_id,
               COALESCE(s.checked_at, ''),
               GROUP_CONCAT(mo.origin_version_id)
          FROM model_origin mo
          LEFT JOIN origin_model_status s
                 ON s.provider = mo.provider AND s.origin_model_id = mo.origin_model_id
         WHERE mo.provider = ?`
	if cutoff != "" {
		query += ` AND COALESCE(s.checked_at, '') < ?`
		args = append(args, cutoff)
	}
	if !recheckGone {
		query += ` AND s.upstream_gone_at IS NULL`
	}
	query += `
         GROUP BY mo.origin_model_id
         ORDER BY COALESCE(s.checked_at, '') ASC, mo.origin_model_id ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing owned origin models: %w", err)
	}
	defer rows.Close()

	out := []OwnedModel{}
	for rows.Next() {
		var m OwnedModel
		var versions string
		if err := rows.Scan(&m.ModelID, &m.CheckedAt, &versions); err != nil {
			return nil, err
		}
		m.Provider = provider
		for _, v := range strings.Split(versions, ",") {
			if v != "" {
				m.OwnedVersionIDs = append(m.OwnedVersionIDs, v)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PendingUpdate is one local model with a newer version upstream.
type PendingUpdate struct {
	SHA256   string `json:"sha256"`
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`

	HaveVersionID   string `json:"have_version_id,omitempty"`
	HaveVersionName string `json:"have_version_name,omitempty"`

	LatestVersionID   string `json:"latest_version_id"`
	LatestVersionName string `json:"latest_version_name,omitempty"`
	LatestPublishedAt string `json:"published_at,omitempty"`
	LatestBaseModel   string `json:"base_model,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	DownloadURL       string `json:"download_url,omitempty"`
	PageURL           string `json:"page_url,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`

	// Name and LocalPath come from the library, so the list can say which of
	// your files this is about rather than only naming a remote id.
	Name      string `json:"name,omitempty"`
	LocalPath string `json:"local_path,omitempty"`

	// BaseModelChanged marks an update that retargets a different base model.
	// Worth flagging rather than hiding: a LoRA rebuilt from SD 1.5 onto SDXL
	// ships as a new version of the same model but is not a drop-in
	// replacement, and installing it as an "update" would quietly break
	// whatever depended on the old one.
	BaseModelChanged bool `json:"base_model_changed,omitempty"`
}

// PendingUpdates reads the stored update list.
func (s *Store) PendingUpdates(limit int) ([]PendingUpdate, error) {
	query := `
        SELECT u.sha256, u.provider, u.origin_model_id,
               u.have_version_id, u.have_version_name,
               u.latest_version_id, u.latest_version_name, u.latest_published_at,
               u.latest_base_model, u.latest_size_bytes, u.latest_download_url,
               u.latest_page_url, u.checked_at,
               COALESCE(r.name, ''), COALESCE(r.base_model, ''),
               COALESCE((SELECT p.path FROM model_file_path p
                          WHERE p.sha256 = u.sha256
                          ORDER BY p.present DESC, p.id LIMIT 1), '')
          FROM model_update u
          LEFT JOIN model_record r ON r.sha256 = u.sha256
         ORDER BY COALESCE(r.name, ''), u.sha256`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing pending updates: %w", err)
	}
	defer rows.Close()

	out := []PendingUpdate{}
	for rows.Next() {
		var u PendingUpdate
		var haveBase string
		if err := rows.Scan(&u.SHA256, &u.Provider, &u.ModelID,
			&u.HaveVersionID, &u.HaveVersionName,
			&u.LatestVersionID, &u.LatestVersionName, &u.LatestPublishedAt,
			&u.LatestBaseModel, &u.SizeBytes, &u.DownloadURL,
			&u.PageURL, &u.CheckedAt, &u.Name, &haveBase, &u.LocalPath); err != nil {
			return nil, err
		}
		u.BaseModelChanged = baseModelChanged(haveBase, u.LatestBaseModel)
		out = append(out, u)
	}
	return out, rows.Err()
}

// baseModelChanged reports whether an update retargets a different base model.
// Blank on either side is not a change: an unknown base is not evidence of one.
func baseModelChanged(have, latest string) bool {
	h, l := strings.TrimSpace(have), strings.TrimSpace(latest)
	return h != "" && l != "" && !strings.EqualFold(h, l)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
