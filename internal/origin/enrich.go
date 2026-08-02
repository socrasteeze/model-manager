package origin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/thumb"
)

// EnrichOptions configures an enrichment run.
type EnrichOptions struct {
	Client *Client
	Blobs  *blobstore.Store

	// Limit caps how many models are looked up in one run. A 19k library against
	// a rate-limited public API is several hours; being able to stop and resume
	// is what makes it practical (§18: throttled, resumable batch runs).
	Limit int

	// Refresh re-queries models that already have a cached answer. Off by
	// default: the archive is the point, and re-fetching is how you lose it.
	Refresh bool

	// SkipImages avoids downloading preview images, which dominate the bytes.
	SkipImages bool

	// MaxImages per model.
	MaxImages int

	Progress func(done, total int)
	Logf     func(format string, args ...any)
}

// EnrichStats summarizes a run.
type EnrichStats struct {
	Considered int
	CacheHits  int
	Fetched    int
	Found      int
	Missing    int
	Images     int
	Errors     int
	Elapsed    time.Duration
}

// Enrich looks up models on Civitai by hash and merges what comes back.
func Enrich(ctx context.Context, st *store.Store, opts EnrichOptions) (*EnrichStats, error) {
	started := time.Now()
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	if opts.MaxImages <= 0 {
		opts.MaxImages = 4
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	cache := NewCache(st)
	stats := &EnrichStats{}

	targets, err := enrichTargets(st, opts.Limit)
	if err != nil {
		return nil, err
	}
	stats.Considered = len(targets)

	for i, sha := range targets {
		if ctx.Err() != nil {
			break
		}
		if opts.Progress != nil {
			opts.Progress(i, len(targets))
		}

		raw, fromCache, err := c_lookup(ctx, cache, opts, sha)
		if err != nil {
			stats.Errors++
			logf("%s: %v", short(sha), err)
			// A rate limit is not a per-model problem; continuing would just
			// keep hitting it, and the run is designed to be resumed.
			if isRateLimit(err) {
				logf("stopping early: the API is rate limiting. Re-run to continue where this left off.")
				break
			}
			continue
		}
		if fromCache {
			stats.CacheHits++
		} else {
			stats.Fetched++
		}
		if raw == nil {
			stats.Missing++
			continue
		}
		stats.Found++

		obs, tags, hashes, images := ObservationsFromCivitai(raw, sha)
		if len(obs) > 0 {
			if err := st.RecordObservations(sha, provenance.SourceCivitai, obs); err != nil {
				stats.Errors++
				logf("%s: recording: %v", short(sha), err)
				continue
			}
		}
		if len(tags) > 0 {
			if err := st.SetTags(sha, provenance.SourceCivitai, tags); err != nil {
				stats.Errors++
				logf("%s: tags: %v", short(sha), err)
			}
		}
		if len(hashes) > 0 {
			if err := cache.PutHashes(sha, ProviderCivitai, hashes); err != nil {
				logf("%s: hashes: %v", short(sha), err)
			}
		}

		if !opts.SkipImages && opts.Blobs != nil {
			n := fetchImages(ctx, opts, st, sha, images)
			stats.Images += n
		}

		if _, err := st.ResolveModel(sha); err != nil {
			stats.Errors++
			logf("%s: resolve: %v", short(sha), err)
		}
	}

	if opts.Progress != nil {
		opts.Progress(len(targets), len(targets))
	}
	stats.Elapsed = time.Since(started)
	return stats, nil
}

// c_lookup returns the archived response, fetching it if needed.
func c_lookup(ctx context.Context, cache *Cache, opts EnrichOptions, sha string) (json.RawMessage, bool, error) {
	if !opts.Refresh {
		if entry, ok, err := cache.Get(ProviderCivitai, sha); err != nil {
			return nil, false, err
		} else if ok {
			if entry.Found {
				return entry.Raw, true, nil
			}
			return nil, true, nil
		}
	}

	raw, status, err := opts.Client.LookupCivitaiByHash(ctx, sha)
	if err != nil {
		return nil, false, err
	}
	if raw == nil {
		// A definite "not there". Cached with a TTL so it is not re-asked every
		// run, but can change if someone uploads the model later.
		return nil, false, cache.PutMissing(ProviderCivitai, sha, status)
	}
	if err := cache.PutFound(ProviderCivitai, sha, raw, status); err != nil {
		return nil, false, err
	}
	return raw, false, nil
}

func fetchImages(ctx context.Context, opts EnrichOptions, st *store.Store, sha string, urls []string) int {
	stored := 0
	for i, url := range urls {
		if i >= opts.MaxImages || ctx.Err() != nil {
			break
		}
		data, err := opts.Client.fetchBytes(ctx, url)
		if err != nil {
			continue
		}
		// Sniff rather than trust the URL's extension: a preview that is not an
		// image must never reach the blob store.
		if !blobstore.IsImage(data) {
			continue
		}
		blob, err := opts.Blobs.Put(data)
		if err != nil {
			continue
		}

		p := store.PreviewImage{
			SHA256:      sha,
			ImageSHA256: blob.SHA256,
			MIME:        blob.MIME,
			Bytes:       blob.Bytes,
			Source:      provenance.SourceCivitai,
			Position:    i,
		}
		// A grid-sized copy, derived once here so the library does not send a
		// full-size render to every card on every page. Failure is not fatal:
		// an image that will not scale is still a valid preview, and the grid
		// falls back to the full one.
		if t, err := thumb.Derive(data); err == nil {
			if tb, err := opts.Blobs.Put(t.Data); err == nil {
				p.ThumbSHA256 = tb.SHA256
			}
			p.Width, p.Height = t.SourceWidth, t.SourceHeight
		} else if w, h, err := thumb.Dimensions(data); err == nil {
			p.Width, p.Height = w, h
		}

		if err := st.AddPreviewImage(p); err == nil {
			stored++
		}
	}
	return stored
}

// enrichTargets picks which models to look up.
//
// Only models with a real, confirmed hash: a provisional path was bound by
// sampled probe rather than a full read, and querying an origin with a hash we
// are not sure of would archive someone else's metadata under this file (§10.1).
func enrichTargets(st *store.Store, limit int) ([]string, error) {
	query := `
        SELECT f.sha256
          FROM model_file f
         WHERE EXISTS (
                   SELECT 1 FROM model_file_path p
                    WHERE p.sha256 = f.sha256 AND p.present = 1 AND p.provisional = 0
               )
         ORDER BY f.size DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := st.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("origin: selecting enrichment targets: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

func (c *Client) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	if err := c.throttle(ctx); err != nil {
		return nil, err
	}
	req, err := newRequest(ctx, url, c.UserAgent, c.TokenFor(url))
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("origin: %s returned %d", url, resp.StatusCode)
	}
	return readLimited(resp.Body, blobstore.MaxBlobBytes)
}

// Summary renders the stats for the CLI.
func (s *EnrichStats) Summary() string {
	out := fmt.Sprintf(
		"considered %d  fetched %d  cached %d  found %d  not on civitai %d",
		s.Considered, s.Fetched, s.CacheHits, s.Found, s.Missing)
	if s.Images > 0 {
		out += fmt.Sprintf("  images %d", s.Images)
	}
	if s.Errors > 0 {
		out += fmt.Sprintf("  errors %d", s.Errors)
	}
	return out + fmt.Sprintf("  (%s)", s.Elapsed.Round(time.Second))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
