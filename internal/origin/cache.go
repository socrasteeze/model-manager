// Package origin fetches metadata from Civitai and HuggingFace and archives what
// it gets.
//
// The cache here is not an optimization. Models are removed from Civitai
// regularly, and once gone the metadata is unrecoverable anywhere (spec §12.1) --
// so the full raw response is stored and never expired. For a taken-down model,
// this local copy may be the only one left.
package origin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/timestamp"
)

// Providers.
const (
	ProviderCivitai     = "civitai"
	ProviderHuggingFace = "huggingface"
)

// NegativeTTL is how long a miss is remembered.
//
// Negative caching matters at this scale: without it every run re-queries
// thousands of known misses, which is most of a self-trained library. With an
// expiring one, a model that appears upstream later is still picked up.
const NegativeTTL = 14 * 24 * time.Hour

// CacheEntry is a stored lookup.
type CacheEntry struct {
	Provider   string
	LookupKey  string
	Found      bool
	Raw        json.RawMessage
	HTTPStatus int
	FetchedAt  time.Time
}

// Cache reads and writes the origin archive.
type Cache struct {
	st *store.Store
}

// NewCache wraps a store.
func NewCache(st *store.Store) *Cache { return &Cache{st: st} }

// Get returns a cached lookup, if one is usable.
//
// A positive hit never expires -- that is the archive property. A negative hit
// expires, so the answer "not on Civitai" can change when someone uploads it.
func (c *Cache) Get(provider, key string) (*CacheEntry, bool, error) {
	var e CacheEntry
	var raw sql.NullString
	var found, status int
	var fetchedAt string
	var expiresAt sql.NullString

	err := c.st.DB().QueryRow(`
        SELECT provider, lookup_key, found, raw_response, COALESCE(http_status, 0),
               fetched_at, expires_at
          FROM origin_cache WHERE provider = ? AND lookup_key = ?`,
		provider, key,
	).Scan(&e.Provider, &e.LookupKey, &found, &raw, &status, &fetchedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("origin: reading cache: %w", err)
	}

	e.Found = found == 1
	e.HTTPStatus = status
	e.FetchedAt, _ = timestamp.Parse(fetchedAt)
	if raw.Valid {
		e.Raw = json.RawMessage(raw.String)
	}

	if !e.Found && expiresAt.Valid {
		if expiry, err := timestamp.Parse(expiresAt.String); err == nil {
			if time.Now().After(expiry) {
				return nil, false, nil
			}
		}
	}
	return &e, true, nil
}

// PutFound stores a successful lookup. The raw body is kept verbatim and
// forever; extracting fields from it is a separate, re-runnable step.
func (c *Cache) PutFound(provider, key string, raw json.RawMessage, status int) error {
	return c.put(provider, key, true, raw, status, nil)
}

// PutMissing stores a negative lookup with an expiry.
func (c *Cache) PutMissing(provider, key string, status int) error {
	expiry := time.Now().Add(NegativeTTL)
	return c.put(provider, key, false, nil, status, &expiry)
}

func (c *Cache) put(provider, key string, found bool, raw json.RawMessage, status int, expires *time.Time) error {
	var rawValue any
	if len(raw) > 0 {
		rawValue = string(raw)
	}
	var expiresValue any
	if expires != nil {
		expiresValue = timestamp.Format(*expires)
	}
	foundInt := 0
	if found {
		foundInt = 1
	}

	// A previously archived positive response is never replaced by a later
	// negative one. If Civitai has taken the model down, the copy already here is
	// the only one left, and overwriting it with a 404 would destroy exactly what
	// §12.1 says to preserve.
	_, err := c.st.DB().Exec(`
        INSERT INTO origin_cache (provider, lookup_key, found, raw_response, http_status, fetched_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(provider, lookup_key) DO UPDATE SET
            found        = CASE WHEN origin_cache.found = 1 AND excluded.found = 0
                                THEN 1 ELSE excluded.found END,
            raw_response = CASE WHEN origin_cache.found = 1 AND excluded.found = 0
                                THEN origin_cache.raw_response ELSE excluded.raw_response END,
            http_status  = excluded.http_status,
            fetched_at   = excluded.fetched_at,
            expires_at   = excluded.expires_at`,
		provider, key, foundInt, rawValue, status,
		timestamp.Now(), expiresValue)
	if err != nil {
		return fmt.Errorf("origin: writing cache: %w", err)
	}
	return nil
}

// PutHashes records every hash type an origin reports for a file.
//
// Storing all of them, not just SHA256, is what lets this database answer a
// question posed in another tool's vocabulary -- AutoV2 is how A1111 and Civitai
// itself refer to the same file.
func (c *Cache) PutHashes(sha, provider string, hashes map[string]string) error {
	for hashType, value := range hashes {
		if hashType == "" || value == "" {
			continue
		}
		if _, err := c.st.DB().Exec(`
            INSERT INTO origin_hash (sha256, hash_type, hash_value, provider)
            VALUES (?, ?, ?, ?)
            ON CONFLICT(sha256, hash_type, provider) DO UPDATE SET hash_value = excluded.hash_value`,
			sha, hashType, value, provider); err != nil {
			return fmt.Errorf("origin: recording %s hash: %w", hashType, err)
		}
	}
	return nil
}

// ArchiveStats summarizes what has been preserved.
type ArchiveStats struct {
	Positive        int `json:"positive"`
	Negative        int `json:"negative"`
	NegativeExpired int `json:"negative_expired"`
	Bytes           int `json:"bytes"`
}

// Stats reports the archive's size.
func (c *Cache) Stats() (*ArchiveStats, error) {
	var s ArchiveStats
	err := c.st.DB().QueryRow(`
        SELECT
            COALESCE(SUM(CASE WHEN found = 1 THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN found = 0 THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN found = 0 AND expires_at IS NOT NULL
                               AND expires_at < ? THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(LENGTH(COALESCE(raw_response, ''))), 0)
          FROM origin_cache`,
		timestamp.Now(),
	).Scan(&s.Positive, &s.Negative, &s.NegativeExpired, &s.Bytes)
	if err != nil {
		return nil, fmt.Errorf("origin: archive stats: %w", err)
	}
	return &s, nil
}
