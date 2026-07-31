package origin

// Update checking.
//
// "Which of my LoRAs have a newer version?" is only answerable because the
// enrichment archive already records which remote model each local file came
// from. The check walks the models the library owns a version of and asks each
// one what its newest version is now.
//
// Deliberately asks per owned model rather than searching. A search would
// return the popular models rather than the owned ones, and could not
// distinguish "no newer version" from "did not appear in these results".

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Update is one model with a newer version available.
type Update struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	Name     string `json:"name"`

	HaveVersionID   string `json:"have_version_id,omitempty"`
	HaveVersionName string `json:"have_version_name,omitempty"`

	LatestVersionID   string `json:"latest_version_id"`
	LatestVersionName string `json:"latest_version_name,omitempty"`
	PublishedAt       string `json:"published_at,omitempty"`

	// LocalSHA256/LocalPath identify the copy currently held, so an update can
	// be applied knowing exactly which file it supersedes.
	LocalSHA256 string `json:"local_sha256,omitempty"`
	LocalPath   string `json:"local_path,omitempty"`

	BaseModel   string `json:"base_model,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	PageURL     string `json:"page_url,omitempty"`

	// BaseModelChanged marks an update that retargets a different base model.
	//
	// Worth flagging rather than hiding: a LoRA rebuilt from SD1.5 onto SDXL is
	// published as a new version of the same model but is not a drop-in
	// replacement, and installing it as an "update" would quietly break
	// whatever workflow depended on the old one.
	BaseModelChanged bool `json:"base_model_changed,omitempty"`
}

// UpdateOptions configures a check.
type UpdateOptions struct {
	Client *Client

	// Limit caps how many owned models are checked. Zero means all.
	Limit int

	Progress func(done, total int)
	Logf     func(format string, args ...any)
}

// UpdateStats summarizes a check.
type UpdateStats struct {
	Checked int
	Found   int
	Errors  int

	// RateLimited means the provider cut the run short: the result covers
	// only the models checked so far. Without this flag a truncated sweep is
	// indistinguishable from "everything else is up to date", which is a
	// wrong answer presented confidently.
	RateLimited bool

	Elapsed time.Duration
}

// CheckUpdates finds newer versions of models already held.
func CheckUpdates(ctx context.Context, idx *LocalIndex, opts UpdateOptions) ([]Update, *UpdateStats, error) {
	started := time.Now()
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	owned := idx.OwnedModelIDs(ProviderCivitaiID)
	// Stable order so a --limit run checks the same models each time and can be
	// resumed meaningfully rather than sampling randomly.
	sort.Strings(owned)
	if opts.Limit > 0 && len(owned) > opts.Limit {
		owned = owned[:opts.Limit]
	}

	stats := &UpdateStats{}
	var out []Update

	for i, modelID := range owned {
		if ctx.Err() != nil {
			break
		}
		if opts.Progress != nil {
			opts.Progress(i, len(owned))
		}
		stats.Checked++

		latest, err := opts.Client.LatestVersion(ctx, modelID)
		if err != nil {
			stats.Errors++
			logf("model %s: %v", modelID, err)
			if isRateLimit(err) {
				stats.RateLimited = true
				logf("stopping early: the API is rate limiting. Re-run to continue.")
				break
			}
			continue
		}
		if latest == nil || latest.VersionID == "" {
			// A 404 here means the model was removed upstream. Not an update,
			// and not an error: the local copy may now be the only one left.
			continue
		}

		held := idx.OwnedVersionIDs(ProviderCivitaiID, modelID)
		if containsString(held, latest.VersionID) {
			continue
		}

		u := Update{
			Provider:          ProviderCivitaiID,
			ModelID:           modelID,
			Name:              latest.Name,
			LatestVersionID:   latest.VersionID,
			LatestVersionName: latest.VersionName,
			PublishedAt:       latest.PublishedAt,
			BaseModel:         latest.BaseModel,
			PageURL:           latest.PageURL,
		}
		if versions := idx.ownedVersions[ProviderCivitaiID+"/"+modelID]; len(versions) > 0 {
			u.HaveVersionID = versions[0].VersionID
			u.HaveVersionName = versions[0].VersionName
			u.LocalSHA256 = versions[0].SHA256
			u.LocalPath = idx.bySHA[versions[0].SHA256]
		}
		if f := latest.PrimaryFile(); f != nil {
			u.SizeBytes = f.SizeBytes
			u.DownloadURL = f.DownloadURL

			// If the newest version's file is already on disk under a different
			// model id, this is not an update at all.
			if f.SHA256 != "" && idx.Has(f.SHA256) {
				continue
			}
		}

		out = append(out, u)
		stats.Found++
	}

	if opts.Progress != nil {
		opts.Progress(len(owned), len(owned))
	}
	stats.Elapsed = time.Since(started)
	return out, stats, nil
}

// MarkBaseModelChanges flags updates that retarget a different base model.
//
// Separated from the fetch loop so it can be applied to a stored result set
// without re-querying.
func MarkBaseModelChanges(updates []Update, baseOf func(sha256 string) string) {
	for i := range updates {
		if updates[i].LocalSHA256 == "" || updates[i].BaseModel == "" {
			continue
		}
		have := baseOf(updates[i].LocalSHA256)
		if have != "" && !equalFoldTrim(have, updates[i].BaseModel) {
			updates[i].BaseModelChanged = true
		}
	}
}

// Summary renders the stats for the CLI.
func (s *UpdateStats) Summary() string {
	out := fmt.Sprintf("checked %d  updates %d", s.Checked, s.Found)
	if s.Errors > 0 {
		out += fmt.Sprintf("  errors %d", s.Errors)
	}
	if s.RateLimited {
		out += "  (rate limited — partial result, re-run to continue)"
	}
	return out + fmt.Sprintf("  (%s)", s.Elapsed.Round(time.Second))
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
