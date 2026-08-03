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
// poll for progress, cancel by ID.
//
// One sweep at a time, deliberately. Two concurrent sweeps would each honour
// their own throttle and together double the request rate against the very API
// the throttle exists to stay polite to -- earning a rate limit that stops both.
package enrichjob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
)

// ErrInFlight means a sweep is already running.
var ErrInFlight = errors.New("enrichjob: an enrichment run is already in progress")

// State is where a sweep is.
type State string

const (
	StateRunning   State = "running"
	StateComplete  State = "complete"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
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

	// LastError is the most recent per-model failure. Kept because the errors
	// counter alone tells you something went wrong without saying what, and the
	// UI has nowhere else to look -- there is no console behind this.
	LastError string `json:"last_error,omitempty"`

	// Error is a failure of the run itself, as opposed to of one model.
	Error string `json:"error,omitempty"`
}

// Manager owns the at-most-one running sweep and the record of the last one.
type Manager struct {
	st     *store.Store
	blobs  *blobstore.Store
	client func() *origin.Client

	mu      sync.Mutex
	current *Job
	cancel  context.CancelFunc
	seq     int64
}

// New builds a Manager.
//
// The client is supplied as a function rather than a value so each run picks up
// the credentials configured at the time it starts. A key pasted into Settings
// has to apply to the next sweep without restarting the daemon.
func New(st *store.Store, blobs *blobstore.Store, client func() *origin.Client) *Manager {
	return &Manager{st: st, blobs: blobs, client: client}
}

// Start begins a sweep. It registers the job before returning, so the caller's
// first poll is guaranteed to see it.
func (m *Manager) Start(scope string, opts Options) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current != nil && m.current.State == StateRunning {
		return *m.current, ErrInFlight
	}

	m.seq++
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:        jobID(m.seq),
		State:     StateRunning,
		Scope:     scope,
		StartedAt: time.Now(),
	}
	m.current = job
	m.cancel = cancel

	go m.run(ctx, job, opts)
	return *job, nil
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
		Progress: func(done, total int) {
			m.mu.Lock()
			defer m.mu.Unlock()
			job.ModelsDone, job.ModelsTotal = done, total
		},
		Logf: func(format string, args ...any) {
			m.mu.Lock()
			defer m.mu.Unlock()
			job.LastError = fmt.Sprintf(format, args...)
		},
	}
	if opts.SkipImages {
		eo.Blobs = nil
	}

	stats, err := origin.Enrich(ctx, m.st, eo)

	m.mu.Lock()
	defer m.mu.Unlock()
	finished := time.Now()
	job.FinishedAt = &finished

	if stats != nil {
		job.Fetched, job.CacheHits = stats.Fetched, stats.CacheHits
		job.Found, job.Missing = stats.Found, stats.Missing
		job.Images, job.Errors = stats.Images, stats.Errors
	}

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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return Job{}, false
	}
	return *m.current, true
}

// Cancel stops the running sweep. Reports whether there was one to stop.
//
// A cancelled sweep is not a lost sweep: every response already archived stays
// archived, and re-running continues where this stopped -- which is the whole
// reason the archive is consulted before the network.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.State != StateRunning {
		return false
	}
	if id != "" && id != m.current.ID {
		return false
	}
	if m.cancel != nil {
		m.cancel()
	}
	return true
}

func jobID(seq int64) string {
	const digits = "0123456789"
	if seq == 0 {
		return "enrich-0"
	}
	var buf []byte
	for n := seq; n > 0; n /= 10 {
		buf = append([]byte{digits[n%10]}, buf...)
	}
	return "enrich-" + string(buf)
}
