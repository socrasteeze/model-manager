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

	// RecheckGone re-asks about models the provider has stopped serving.
	//
	// Off by default. A 404 only advances checked_at, and MaxAge defaults to
	// zero, so without the exclusion a taken-down model is re-asked on every
	// sweep forever -- on a library that has outlived a few takedowns, that
	// becomes most of the sweep. Turning it on is how an operator asks whether
	// any of them came back.
	RecheckGone bool

	Progress func(done, total int, stats UpdateStats)
	Logf     func(format string, args ...any)
}

// recoverFromMirror archives the mirror's copy of a record the provider has
// removed, and returns how many hashes it recovered.
//
// CivArchive mirrors Civitai records including ones Civitai has taken down, so a
// 404 is exactly the moment it stops being redundant. The body is archived and
// nothing is decoded from it: the archive property is keeping the bytes, and
// deriving fields from an endpoint this project documents as unverified against
// the live service would write guessed metadata under a trusted source name.
//
// Bounded two ways, so a self-trained library -- most of which 404s -- does not
// spend a second request per model on every sweep. Only models something already
// records an interest in reach here at all, and an existing cache entry, found
// or missing, means the question has been asked once and will not be asked
// again.
func recoverFromMirror(ctx context.Context, st *store.Store, opts SweepOptions, modelID string) (int, error) {
	cache := NewCache(st)
	shas, err := st.SHAsForOriginModel(ProviderCivitaiID, modelID)
	if err != nil || len(shas) == 0 {
		return 0, err
	}

	recovered := 0
	for _, sha := range shas {
		if ctx.Err() != nil {
			break
		}
		if _, asked, err := cache.Get(ProviderCivArchiveID, sha); err != nil {
			return recovered, err
		} else if asked {
			continue
		}
		raw, status, err := opts.Client.LookupCivArchiveByHash(ctx, sha)
		if err != nil {
			return recovered, err
		}
		if raw == nil {
			// The mirror does not have it either. Recorded so the question is
			// not re-asked every sweep for a record nobody has.
			if err := cache.PutMissing(ProviderCivArchiveID, sha, status); err != nil {
				return recovered, err
			}
			continue
		}
		if err := cache.PutFound(ProviderCivArchiveID, sha, raw, status); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
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

	owned, err := st.OwnedOriginModels(ProviderCivitaiID, opts.MaxAge, opts.Limit, opts.RecheckGone)
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
			//
			// Recorded as gone as well as checked. Without the stamp the model
			// returns to this queue on every subsequent sweep -- checked_at
			// advances but nothing reads it when MaxAge is zero, which is the
			// default -- so a library that has outlived a few takedowns spends
			// most of each sweep re-confirming absences.
			if markErr := st.MarkOriginModelChecked(
				ProviderCivitaiID, m.ModelID, 404, ""); markErr != nil {
				logf("model %s: recording the miss: %v", m.ModelID, markErr)
			}
			if markErr := st.MarkOriginModelGone(ProviderCivitaiID, m.ModelID); markErr != nil {
				logf("model %s: recording the takedown: %v", m.ModelID, markErr)
			}
			stats.Gone++

			// A 404 is the moment the mirror stops being redundant and becomes
			// the only surviving copy of this record. Asked here rather than on
			// a later pass because the model id is still in hand, and because a
			// takedown does not get easier to recover from with time.
			if n, err := recoverFromMirror(ctx, st, opts, m.ModelID); err != nil {
				logf("model %s: mirror lookup: %v", m.ModelID, err)
			} else if n > 0 {
				stats.Recovered += n
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
