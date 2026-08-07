// Package archivejob acquires a model from a provider, completely enough that
// the provider can vanish and nothing is lost.
//
// Same shape as enrichjob and updatejob, and for the same reason: a handful of
// throttled requests plus a preview fetch each is minutes, not milliseconds, so
// the work is registered and polled rather than held open in a request.
//
// One thing here is deliberately different. The model file itself is handed to
// the download manager and this job does NOT wait for it. That is what keeps an
// intake to minutes: waiting on a twelve-gigabyte transfer would hold the shared
// provider throttle for an hour and stop every enrichment and update sweep
// behind it. The file's arrival is recorded later, by the download completion
// hook, which is also where the staged previews are attached.
package archivejob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/jobrun"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// ErrInFlight means an intake is already running.
var ErrInFlight = errors.New("archivejob: an archive run is already in progress")

// State is where a run is.
type State = jobrun.State

const (
	StateRunning   = jobrun.StateRunning
	StateComplete  = jobrun.StateComplete
	StateFailed    = jobrun.StateFailed
	StateCancelled = jobrun.StateCancelled
)

// Target names one model version to archive.
type Target struct {
	Provider  string `json:"provider"`
	ModelID   string `json:"model_id"`
	VersionID string `json:"version_id,omitempty"`

	// Watch adds the model to the watchlist as part of the intake, which is the
	// ordinary intent: somebody archiving a model usually wants its next version
	// too.
	Watch bool `json:"watch,omitempty"`
}

// Options are the per-run knobs.
type Options struct {
	Targets []Target

	// PreviewLimit caps images per version. Zero takes origin's default.
	PreviewLimit int

	// Force re-fetches steps already recorded as complete. Without it a re-run
	// only fills the gaps, which is what makes a partial archive cheap to
	// finish.
	Force bool

	// StartDownload hands the model file to the download manager. Nil archives
	// the metadata and previews only, which is the honest fallback on a daemon
	// with downloads disabled -- and is still worth doing, because metadata is
	// what disappears first.
	StartDownload func(t Target, file origin.RemoteFile) error
}

// Job is one run, as reported to a poller.
type Job struct {
	ID    string `json:"id"`
	State State  `json:"state"`

	StartedAt time.Time `json:"started_at"`
	// A pointer, because omitempty does not omit a zero time.Time.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	Total int `json:"total"`
	Done  int `json:"done"`

	Archived int `json:"archived"`
	Partial  int `json:"partial"`
	Gone     int `json:"gone"`
	Errors   int `json:"errors"`

	// RateLimited means the provider cut the run short, so the result covers
	// only the targets reached. Without it a truncated run reads exactly like a
	// complete one.
	RateLimited bool `json:"rate_limited"`

	LastError string `json:"last_error,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (j Job) Running() bool { return j.State == StateRunning }
func (j Job) JobID() string { return j.ID }

// Manager owns the at-most-one running intake.
type Manager struct {
	st     *store.Store
	blobs  *blobstore.Store
	client func() *origin.Client
	runner *jobrun.Runner[Job]

	// busy asks the shared-throttle group whether another sweep is running, and
	// autoDownload hands a file to the download queue. Both are set by the
	// daemon through SetHooks rather than injected as dependencies, so this
	// package does not import the API layer that owns them.
	busy         func() (string, string, bool)
	autoDownload func(Target, origin.RemoteFile) error
}

// New builds a Manager. The client is a function for the same reason the other
// job packages take one: a key pasted into Settings must apply to the next run
// without restarting the daemon.
func New(st *store.Store, blobs *blobstore.Store, client func() *origin.Client) *Manager {
	return &Manager{st: st, blobs: blobs, client: client,
		runner: jobrun.New[Job](jobrun.GenID("archive"))}
}

func (m *Manager) InFlight() (Job, bool) { return m.runner.InFlight() }
func (m *Manager) Current() (Job, bool)  { return m.runner.Current() }
func (m *Manager) Cancel(id string) bool { return m.runner.Cancel(id) }

// Start begins an intake, registering the job before returning so the caller's
// first poll is guaranteed to see it.
func (m *Manager) Start(opts Options) (Job, error) {
	job, snapshot, ctx, ok := m.runner.Start(func(id string) *Job {
		return &Job{ID: id, State: StateRunning, StartedAt: time.Now(),
			Total: len(opts.Targets)}
	})
	if !ok {
		return snapshot, ErrInFlight
	}
	go m.run(ctx, job, opts)
	return snapshot, nil
}

func (m *Manager) run(ctx context.Context, job *Job, opts Options) {
	var runErr error
	for i, t := range opts.Targets {
		if ctx.Err() != nil {
			break
		}
		res := m.archiveOne(ctx, t, opts)

		m.runner.Lock()
		job.Done = i + 1
		switch {
		case res.err != nil:
			job.Errors++
			job.LastError = res.err.Error()
		case res.gone:
			job.Gone++
		case res.complete:
			job.Archived++
		default:
			job.Partial++
		}
		if res.rateLimited {
			job.RateLimited = true
		}
		m.runner.Unlock()

		if res.rateLimited {
			// Stopping here rather than pushing through: every step recorded so
			// far is recorded, and a re-run resumes from the incomplete list.
			break
		}
	}

	m.runner.Lock()
	defer m.runner.Unlock()
	finished := time.Now()
	job.FinishedAt = &finished
	switch {
	case runErr != nil:
		job.State, job.Error = StateFailed, runErr.Error()
	case ctx.Err() != nil:
		job.State = StateCancelled
	default:
		job.State = StateComplete
	}
}

// result is what one target produced.
type result struct {
	complete    bool
	gone        bool
	rateLimited bool
	err         error
}

// archiveOne captures everything about one model version.
//
// The order is the point, and it is chosen so that a run cut short at any step
// leaves something worth having:
//
//  1. the archive_item row, which has no foreign keys, so a partial is recordable
//  2. the version body, first, because it is the takedown insurance
//  3. the model body, for the context the version body does not repeat
//  4. the previews, staged as blobs before there is a file to attach them to
//  5. the file, handed to the download manager and not waited for
//
// model_origin and the field observations cannot be written here at all: both
// key on model_file, and the file has not landed yet. They happen in the
// completion hook.
func (m *Manager) archiveOne(ctx context.Context, t Target, opts Options) result {
	client := m.client()
	cache := origin.NewCache(m.st)

	if t.Provider == "" {
		t.Provider = origin.ProviderCivitaiID
	}
	if t.Provider != origin.ProviderCivitaiID {
		return result{err: fmt.Errorf("archive: %s intake is not implemented", t.Provider)}
	}

	// The model body first, because without an explicit version there is nothing
	// to identify yet.
	_, listings, present, err := client.ArchiveModelBody(ctx, cache, t.ModelID)
	if err != nil && listings == nil {
		return result{err: err, rateLimited: origin.IsRateLimit(err)}
	}
	if !present {
		// The model itself is gone. Recorded where model-level facts live, so
		// the update sweep stops re-asking as well.
		if markErr := m.st.MarkOriginModelGone(t.Provider, t.ModelID); markErr != nil {
			return result{err: markErr}
		}
		return result{gone: true}
	}

	chosen, ok := origin.PickVersion(listings, t.VersionID)
	if !ok {
		return result{err: fmt.Errorf("archive: model %s has no version %s", t.ModelID, t.VersionID)}
	}
	t.VersionID = chosen.VersionID

	item := store.ArchiveItem{Provider: t.Provider, ModelID: t.ModelID, VersionID: t.VersionID}
	if err := m.st.PutArchiveItem(item); err != nil {
		return result{err: err}
	}
	if t.Watch {
		if err := m.st.PutArchiveWatch(store.ArchiveWatch{
			Provider: t.Provider, ModelID: t.ModelID,
		}); err != nil {
			return result{err: err}
		}
	}

	existing, err := m.st.ArchiveItemFor(t.Provider, t.ModelID, t.VersionID)
	if err != nil {
		return result{err: err}
	}
	skip := func(done bool) bool { return done && !opts.Force }

	var problems []string
	note := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// The version body. Archived before anything is decoded from it, because the
	// bytes are the part that has to survive a takedown.
	var version *origin.CivitaiVersion
	if !skip(existing.OriginCacheOK) {
		raw, v, stillThere, err := client.ArchiveVersionBody(ctx, cache, t.ModelID, t.VersionID)
		if err != nil && raw == nil {
			m.recordError(t, err.Error())
			return result{err: err, rateLimited: origin.IsRateLimit(err)}
		}
		if !stillThere {
			if markErr := m.st.MarkArchiveVersionGone(t.Provider, t.ModelID, t.VersionID); markErr != nil {
				return result{err: markErr}
			}
			return result{gone: true}
		}
		version = v
		if err != nil {
			note("the version body could not be decoded, but was archived: %v", err)
		}
		if err := m.st.MarkArchiveStep(t.Provider, t.ModelID, t.VersionID, "origin_cache"); err != nil {
			return result{err: err}
		}
		// meta_ok records that the metadata is captured. The field observations
		// derived from it are written when the file lands, since they key on a
		// model_file row that does not exist yet.
		if err := m.st.MarkArchiveStep(t.Provider, t.ModelID, t.VersionID, "meta"); err != nil {
			return result{err: err}
		}
	}

	// Previews.
	if !skip(existing.PreviewsOK) {
		if err := m.archivePreviews(ctx, t, chosen, version, opts.PreviewLimit); err != nil {
			note("previews: %v", err)
			if origin.IsRateLimit(err) {
				m.recordError(t, strings.Join(problems, "; "))
				return result{rateLimited: true}
			}
		}
	}

	// The file, handed over and not waited for.
	if !skip(existing.FileOK) && opts.StartDownload != nil {
		if file := chosen.PrimaryFile(); file != nil && file.DownloadURL != "" {
			if err := opts.StartDownload(t, *file); err != nil {
				note("starting the download: %v", err)
			}
		} else {
			note("no downloadable file is listed for this version")
		}
	}

	if len(problems) > 0 {
		m.recordError(t, strings.Join(problems, "; "))
	}
	final, err := m.st.ArchiveItemFor(t.Provider, t.ModelID, t.VersionID)
	if err != nil {
		return result{err: err}
	}
	return result{complete: final != nil && final.Complete()}
}

// archivePreviews stages every image it can into the blob store.
//
// Staged rather than attached: preview_image has a foreign key to model_file, so
// nothing can be attached until the download lands -- and for a model taken down
// before it could be fetched, that may be never. The images would be lost in
// exactly the case this feature exists for.
func (m *Manager) archivePreviews(ctx context.Context, t Target, listing origin.Listing,
	version *origin.CivitaiVersion, limit int) error {

	if m.blobs == nil {
		return nil
	}
	client := m.client()
	if limit <= 0 {
		limit = origin.DefaultPreviewLimit
	}

	var bodyURLs []string
	nsfw := listing.NSFW
	if version != nil {
		for _, img := range version.Images {
			if img.URL != "" {
				bodyURLs = append(bodyURLs, img.URL)
			}
		}
		if version.Model.NSFW != nil && *version.Model.NSFW {
			nsfw = true
		}
	}

	// The gallery is additional coverage beyond what the uploader attached. A
	// failure here is not fatal: the body's images are still worth keeping.
	var galleryErr error
	gallery, err := client.GalleryImages(ctx, t.VersionID, limit, nsfw)
	if err != nil {
		galleryErr = err
	}

	urls := origin.MergePreviewURLs(bodyURLs, gallery, limit)
	already, err := m.st.ArchivedPreviewURLs(t.Provider, t.ModelID, t.VersionID)
	if err != nil {
		return err
	}

	got := len(already)
	var failures []string
	for i, u := range urls {
		if ctx.Err() != nil {
			break
		}
		if _, have := already[u]; have {
			continue
		}
		data, err := client.FetchImage(ctx, u)
		if err != nil {
			// One retry against the width-transformed still. Most oversize
			// rejections are a full-resolution asset that has a storable form.
			if still := origin.StillURL(u, 1024); still != u {
				data, err = client.FetchImage(ctx, still)
			}
		}
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if !blobstore.IsImage(data) {
			// An animated preview is refused by the MIME sniff regardless of
			// size. Counted as a gap rather than hidden: the archive genuinely
			// does not have that image.
			failures = append(failures, "not a still image")
			continue
		}
		blob, err := m.blobs.Put(data)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := m.st.PutArchivePreview(store.ArchivePreview{
			Provider: t.Provider, ModelID: t.ModelID, VersionID: t.VersionID,
			ImageSHA256: blob.SHA256, SourceURL: u, Position: i,
			MIME: blob.MIME, Bytes: blob.Bytes,
		}); err != nil {
			return err
		}
		got++
	}

	if err := m.st.SetArchivePreviewCounts(t.Provider, t.ModelID, t.VersionID, len(urls), got); err != nil {
		return err
	}
	switch {
	case galleryErr != nil:
		return galleryErr
	case len(failures) > 0:
		return fmt.Errorf("%d of %d previews stored (%s)", got, len(urls), failures[0])
	}
	return nil
}

func (m *Manager) recordError(t Target, msg string) {
	if msg == "" {
		return
	}
	_ = m.st.SetArchiveError(t.Provider, t.ModelID, t.VersionID, msg)
}

// AttachStagedPreviews moves an intake's staged images onto a model that has now
// landed.
//
// Called from the download completion hook, which is the first moment a
// model_file row exists for the foreign key to point at. Source is the mirror's
// or the provider's, preserved rather than reassigned, so a preview keeps saying
// where it came from.
func AttachStagedPreviews(st *store.Store, blobs *blobstore.Store,
	provider, modelID, versionID, sha string,
	attach func(sha string, data []byte, source string, position int) error) (int, error) {

	staged, err := st.ArchivePreviews(provider, modelID, versionID)
	if err != nil || len(staged) == 0 {
		return 0, err
	}
	moved := 0
	for _, p := range staged {
		data, err := blobs.Read(p.ImageSHA256)
		if err != nil {
			continue
		}
		if err := attach(sha, data, provenance.SourceCivitai, p.Position); err != nil {
			continue
		}
		moved++
	}
	return moved, nil
}
