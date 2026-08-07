// Package enrichjob runs an enrichment sweep in the background so the daemon can
// start one from an HTTP request.
//
// origin.Enrich was always cancellable and always reported progress through a
// callback -- it was simply only ever called from `mm enrich`, where blocking
// and a terminal counter are the right shape. Over HTTP neither is: a sweep is
// one throttled request per model against a rate-limited public API, so a
// thousand models is many minutes, and holding the request open ties the sweep's
// fate to a browser tab.
//
// The shape is the one scanjob and the download manager already established:
// register synchronously so the caller gets an ID back in the 202, run detached,
// poll for progress, cancel by ID. The at-most-one-at-a-time bookkeeping behind
// that shape lives in internal/jobrun, shared with scanjob.
//
// One sweep at a time, deliberately. Two concurrent sweeps would each honour
// their own throttle and together double the request rate against the very API
// the throttle exists to stay polite to -- earning a rate limit that stops both.
package enrichjob

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/jobrun"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
)

// ErrInFlight means a sweep is already running.
var ErrInFlight = errors.New("enrichjob: an enrichment run is already in progress")

// State is where a sweep is. An alias, not a new type: enrichjob's State was
// already identical to jobrun's before jobrun existed, and aliasing keeps
// every existing enrichjob.State / enrichjob.StateRunning reference working
// unchanged.
type State = jobrun.State

const (
	StateRunning   = jobrun.StateRunning
	StateComplete  = jobrun.StateComplete
	StateFailed    = jobrun.StateFailed
	StateCancelled = jobrun.StateCancelled
)

// Options are the per-run knobs the UI exposes.
type Options struct {
	// Targets narrows the sweep to specific models. Empty means the whole
	// library.
	Targets []string

	// Refresh re-asks about models that already have an archived answer.
	Refresh bool

	// SkipImages avoids downloading preview images, which dominate the bytes.
	SkipImages bool

	// MaxImages per model. Zero takes origin's default.
	MaxImages int

	// Limit caps how many models are looked up. Zero means all of them.
	Limit int
}

// Job is one sweep, as reported to a poller.
type Job struct {
	ID    string `json:"id"`
	State State  `json:"state"`
	Scope string `json:"scope"`

	StartedAt time.Time `json:"started_at"`

	// A pointer, because omitempty does not omit a zero time.Time -- a running
	// sweep would otherwise report finishing in year 1.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	ModelsTotal int `json:"models_total"`
	ModelsDone  int `json:"models_done"`

	Fetched   int `json:"fetched"`
	CacheHits int `json:"cache_hits"`
	Found     int `json:"found"`
	Missing   int `json:"missing"`
	Images    int `json:"images"`
	Errors    int `json:"errors"`

	// ImageErrors counts previews that could not be fetched or stored, which is
	// not the same failure as a model that could not be looked up. Reported
	// separately because "found 400, images 0" otherwise reads as a library
	// whose models have no previews rather than a provider nobody could reach.
	ImageErrors int `json:"image_errors"`

	// RateLimited means the provider rejected a request during this run. The
	// run still ends in StateComplete -- it was not cancelled and nothing
	// failed -- so without this flag it reads exactly like an exhaustive
	// sweep, even though the model that got rejected has no result recorded
	// for it (ModelsDone can still equal ModelsTotal: the rejection can land
	// on the last eligible model as easily as an earlier one). Set live, not
	// just at the end: it is copied from origin.EnrichStats on every Progress
	// tick, the same way the other counters below are.
	RateLimited bool `json:"rate_limited"`

	// LastError is the most recent per-model failure. Kept because the errors
	// counter alone tells you something went wrong without saying what, and the
	// UI has nowhere else to look -- there is no console behind this.
	LastError string `json:"last_error,omitempty"`

	// Error is a failure of the run itself, as opposed to of one model.
	Error string `json:"error,omitempty"`
}

// Running and JobID satisfy jobrun.Runnable.
func (j Job) Running() bool { return j.State == StateRunning }
func (j Job) JobID() string { return j.ID }

// Manager owns the at-most-one running sweep and the record of the last one.
type Manager struct {
	st     *store.Store
	blobs  *blobstore.Store
	client func() *origin.Client
	runner *jobrun.Runner[Job]
}

// New builds a Manager.
//
// The client is supplied as a function rather than a value so each run picks up
// the credentials configured at the time it starts. A key pasted into Settings
// has to apply to the next sweep without restarting the daemon.
func New(st *store.Store, blobs *blobstore.Store, client func() *origin.Client) *Manager {
	return &Manager{st: st, blobs: blobs, client: client, runner: jobrun.New[Job](jobrun.GenID("enrich"))}
}

// InFlight reports whether a sweep is running, without registering anything.
//
// Used by the update endpoints, which refuse to start their own sweep while
// this one is going: both spend the same shared origin.Client throttle, and
// jobrun only enforces at-most-one within a Runner.
func (m *Manager) InFlight() (Job, bool) { return m.runner.InFlight() }

// Start begins a sweep. It registers the job before returning, so the caller's
// first poll is guaranteed to see it.
func (m *Manager) Start(scope string, opts Options) (Job, error) {
	job, snapshot, ctx, ok := m.runner.Start(func(id string) *Job {
		return &Job{ID: id, State: StateRunning, Scope: scope, StartedAt: time.Now()}
	})
	if !ok {
		return snapshot, ErrInFlight
	}

	go m.run(ctx, job, opts)
	return snapshot, nil
}

func (m *Manager) run(ctx context.Context, job *Job, opts Options) {
	eo := origin.EnrichOptions{
		Client:     m.client(),
		Blobs:      m.blobs,
		Targets:    opts.Targets,
		Refresh:    opts.Refresh,
		SkipImages: opts.SkipImages,
		MaxImages:  opts.MaxImages,
		Limit:      opts.Limit,
		// Copies the whole snapshot on every tick, not just done/total, so a
		// poller mid-run sees live counts instead of every field but progress
		// staying at zero until the run ends -- and picks up RateLimited the
		// moment origin.Enrich sets it, which is also how the guaranteed final
		// call (Enrich always calls Progress once more after its loop, even on
		// an early exit) makes the post-run copy below unnecessary.
		Progress: func(done, total int, stats origin.EnrichStats) {
			m.runner.Lock()
			defer m.runner.Unlock()
			job.ModelsDone, job.ModelsTotal = done, total
			job.Fetched, job.CacheHits = stats.Fetched, stats.CacheHits
			job.Found, job.Missing = stats.Found, stats.Missing
			job.Images, job.Errors = stats.Images, stats.Errors
			job.ImageErrors = stats.ImageErrors
			job.RateLimited = stats.RateLimited
		},
		Logf: func(format string, args ...any) {
			m.runner.Lock()
			defer m.runner.Unlock()
			job.LastError = fmt.Sprintf(format, args...)
		},
	}
	if opts.SkipImages {
		eo.Blobs = nil
	}

	// stats itself is unused below: Enrich's own final Progress call already
	// wrote every counter (including RateLimited) into job via the callback
	// above, and that call happens on every return path that produces a
	// non-nil stats, so copying them again here would only repeat it.
	_, err := origin.Enrich(ctx, m.st, eo)

	m.runner.Lock()
	defer m.runner.Unlock()
	finished := time.Now()
	job.FinishedAt = &finished

	switch {
	case err != nil:
		job.State = StateFailed
		job.Error = err.Error()
	case ctx.Err() != nil:
		// Enrich returns cleanly on cancellation rather than surfacing the
		// context error, because everything it committed before the stop is
		// still committed. The distinction only matters for what to call it.
		job.State = StateCancelled
	default:
		job.State = StateComplete
	}
}

// Current returns the running or most recent sweep, if any.
func (m *Manager) Current() (Job, bool) {
	return m.runner.Current()
}

// Cancel stops the running sweep. Reports whether there was one to stop.
//
// A cancelled sweep is not a lost sweep: every response already archived stays
// archived, and re-running continues where this stopped -- which is the whole
// reason the archive is consulted before the network.
func (m *Manager) Cancel(id string) bool {
	return m.runner.Cancel(id)
}
