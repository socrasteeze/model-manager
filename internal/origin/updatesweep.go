package origin

// The persisted update sweep.
//
// CheckUpdates (updates.go) answers "what is out of date right now" in memory
// and returns it. That shape is right for the CLI, which prints the answer and
// exits. It is wrong for the library's badge, which has to survive a restart
// and be filterable -- so this records what the provider said instead of
// returning a verdict, and the verdict is computed on read by the model_update
// view.
//
// The two are deliberately not layered on each other. CheckUpdates compares
// against a LocalIndex built in memory; this one writes facts and lets SQL do
// the comparing. Sharing a code path would mean one of them doing the other's
// job badly.

import (
	"context"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/store"
)

// SweepOptions configures a persisted update sweep.
type SweepOptions struct {
	Client *Client

	// Limit caps how many models are asked about. Zero means all of them.
	Limit int

	// MaxAge skips models whose status was refreshed more recently than this.
	// The update sweep's equivalent of enrichment's Refresh, and for the same
	// reason: a full pass over a large library is thousands of throttled
	// requests, and the ordinary ask is "check what has not been looked at
	// lately", not "ask everything again". Zero checks every model.
	MaxAge time.Duration

	Progress func(done, total int, stats UpdateStats)
	Logf     func(format string, args ...any)
}

// SweepUpdates asks each owned model what its newest version is, and records
// the answer.
//
// Records per model as it goes rather than accumulating and writing at the end:
// a sweep cut short by a rate limit must keep everything it learned before the
// cut. That is the same resumability property enrichment gets from archiving
// each response as it arrives, and it is what makes MaxAge a useful resume
// mechanism rather than a cache.
func SweepUpdates(ctx context.Context, st *store.Store, opts SweepOptions) (*UpdateStats, error) {
	started := time.Now()
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Everything enriched before this feature existed has its identity only in
	// the archive. Without this the first sweep on an existing library would
	// find nothing to check and report, confidently, that nothing needs an
	// update.
	if n, err := BackfillModelOrigin(st); err != nil {
		logf("backfilling origin identity: %v", err)
	} else if n > 0 {
		logf("recorded origin identity for %d model(s)", n)
	}

	owned, err := st.OwnedOriginModels(ProviderCivitaiID, opts.MaxAge, opts.Limit)
	if err != nil {
		return nil, err
	}

	stats := &UpdateStats{}
	done := 0

	for i, m := range owned {
		if ctx.Err() != nil {
			break
		}
		if opts.Progress != nil {
			opts.Progress(done, len(owned), *stats)
		}
		// Counted as reached before the lookup runs, matching Enrich: a model
		// that hits the rate limit below was still attempted, and is reflected
		// in stats.Errors.
		done = i + 1
		stats.Checked++

		latest, err := opts.Client.LatestVersion(ctx, m.ModelID)
		if err != nil {
			stats.Errors++
			logf("model %s: %v", m.ModelID, err)
			// Advance checked_at without touching latest_*: a failed check
			// tells you nothing new about the newest version, and blanking a
			// known answer would silently retract a correct badge.
			if markErr := st.MarkOriginModelChecked(
				ProviderCivitaiID, m.ModelID, 0, err.Error()); markErr != nil {
				logf("model %s: recording the failure: %v", m.ModelID, markErr)
			}
			if isRateLimit(err) {
				stats.RateLimited = true
				logf("stopping early: the API is rate limiting. Re-run to continue where this left off.")
				break
			}
			continue
		}

		if latest == nil || latest.VersionID == "" {
			// A 404 means the model was removed upstream. Not an error, and not
			// a reason to clear what was already known: a newer version may
			// still have been published, and may still be reachable elsewhere.
			if markErr := st.MarkOriginModelChecked(
				ProviderCivitaiID, m.ModelID, 404, ""); markErr != nil {
				logf("model %s: recording the miss: %v", m.ModelID, markErr)
			}
			continue
		}

		status := store.OriginModelStatus{
			Provider:          ProviderCivitaiID,
			ModelID:           m.ModelID,
			LatestVersionID:   latest.VersionID,
			LatestVersionName: latest.VersionName,
			LatestPublishedAt: latest.PublishedAt,
			LatestBaseModel:   latest.BaseModel,
			LatestPageURL:     latest.PageURL,
			HTTPStatus:        200,
		}
		if f := latest.PrimaryFile(); f != nil {
			// Lower-cased on the way in by the store, but done here too so the
			// value this code reasons about matches what lands.
			status.LatestFileSHA256 = strings.ToLower(f.SHA256)
			status.LatestSizeBytes = f.SizeBytes
			status.LatestDownloadURL = f.DownloadURL
		}
		if err := st.PutOriginModelStatus(status); err != nil {
			stats.Errors++
			logf("model %s: recording: %v", m.ModelID, err)
			continue
		}

		// "Found" counts models where the newest version is not one this
		// library already holds. It is a progress signal, not the badge: the
		// badge is the view's answer, which also excludes an update whose file
		// is already on disk under another model.
		if !containsString(m.OwnedVersionIDs, latest.VersionID) {
			stats.Found++
		}
	}

	stats.Elapsed = time.Since(started)
	if opts.Progress != nil {
		opts.Progress(done, len(owned), *stats)
	}
	return stats, nil
}
