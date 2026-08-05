// Package updatejob runs an update sweep in the background so the daemon can
// start one from an HTTP request.
//
// Same shape and same reasoning as enrichjob: one throttled request per owned
// model against a rate-limited public API, so a large library is many minutes
// and holding the request open would tie the sweep's fate to a browser tab.
// Register synchronously so the caller gets an ID back in the 202, run
// detached, poll for progress, cancel by ID. The at-most-one bookkeeping lives
// in internal/jobrun, shared with scanjob and enrichjob.
//
// One sweep at a time here, and the API layer additionally refuses to start one
// while an enrichment sweep is running. jobrun only enforces at-most-one within
// a Runner, and both sweeps spend the same shared origin.Client throttle -- run
// together they are individually polite and jointly double the request rate
// against the very API the throttle exists to placate.
package updatejob

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/jobrun"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
)

// ErrInFlight means a sweep is already running.
var ErrInFlight = errors.New("updatejob: an update check is already in progress")

// State is where a sweep is. An alias, so nothing that names
// updatejob.StateRunning has to know jobrun exists.
type State = jobrun.State

const (
	StateRunning   = jobrun.StateRunning
	StateComplete  = jobrun.StateComplete
	StateFailed    = jobrun.StateFailed
	StateCancelled = jobrun.StateCancelled
)

// Options are the per-run knobs the UI exposes.
type Options struct {
	// MaxAge skips models checked more recently than this. Zero checks all.
	MaxAge time.Duration

	// Limit caps how many models are asked about. Zero means all of them.
	Limit int
}

// Job is one sweep, as reported to a poller.
type Job struct {
	ID    string `json:"id"`
	State State  `json:"state"`

	StartedAt time.Time `json:"started_at"`

	// A pointer, because omitempty does not omit a zero time.Time -- a running
	// sweep would otherwise report finishing in year 1.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	ModelsTotal int `json:"models_total"`
	ModelsDone  int `json:"models_done"`

	Checked int `json:"checked"`
	Found   int `json:"found"`
	Errors  int `json:"errors"`

	// RateLimited means the provider rejected a request during this run. The
	// run still ends in StateComplete -- it was not cancelled and nothing
	// failed outright -- so without this flag it reads exactly like an
	// exhaustive sweep, and "nothing else needs an update" would be a wrong
	// answer presented confidently. Copied from UpdateStats on every Progress
	// tick, not just at the end, so a poller sees it the moment it happens.
	RateLimited bool `json:"rate_limited"`

	// LastError is the most recent per-model failure. The counter alone says
	// something went wrong without saying what, and there is no console behind
	// this UI to go and look in.
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
	client func() *origin.Client
	runner *jobrun.Runner[Job]
}

// New builds a Manager.
//
// The client is a function rather than a value for the same reason as
// enrichjob's: a key pasted into Settings has to apply to the next sweep
// without restarting the daemon.
func New(st *store.Store, client func() *origin.Client) *Manager {
	return &Manager{st: st, client: client, runner: jobrun.New[Job](jobrun.GenID("updates"))}
}

// InFlight reports whether a sweep is running, for callers that need to check
// before doing setup work.
func (m *Manager) InFlight() (Job, bool) { return m.runner.InFlight() }

// Start begins a sweep. It registers the job before returning, so the caller's
// first poll is guaranteed to see it.
func (m *Manager) Start(opts Options) (Job, error) {
	job, snapshot, ctx, ok := m.runner.Start(func(id string) *Job {
		return &Job{ID: id, State: StateRunning, StartedAt: time.Now()}
	})
	if !ok {
		return snapshot, ErrInFlight
	}

	go m.run(ctx, job, opts)
	return snapshot, nil
}

func (m *Manager) run(ctx context.Context, job *Job, opts Options) {
	so := origin.SweepOptions{
		Client: m.client(),
		Limit:  opts.Limit,
		MaxAge: opts.MaxAge,
		// Copies the whole snapshot on every tick, not just done/total, so a
		// poller mid-run sees live counts instead of every field but progress
		// staying at zero until the run ends -- and picks up RateLimited the
		// moment the sweep sets it.
		Progress: func(done, total int, stats origin.UpdateStats) {
			m.runner.Lock()
			defer m.runner.Unlock()
			job.ModelsDone, job.ModelsTotal = done, total
			job.Checked, job.Found, job.Errors = stats.Checked, stats.Found, stats.Errors
			job.RateLimited = stats.RateLimited
		},
		Logf: func(format string, args ...any) {
			m.runner.Lock()
			defer m.runner.Unlock()
			job.LastError = fmt.Sprintf(format, args...)
		},
	}

	// The returned stats are unused: the sweep's own final Progress call has
	// already written every counter into job, on every return path.
	_, err := origin.SweepUpdates(ctx, m.st, so)

	m.runner.Lock()
	defer m.runner.Unlock()
	finished := time.Now()
	job.FinishedAt = &finished

	switch {
	case err != nil:
		job.State = StateFailed
		job.Error = err.Error()
	case ctx.Err() != nil:
		// The sweep returns cleanly on cancellation rather than surfacing the
		// context error, because everything it recorded before the stop is
		// still recorded. The distinction only matters for what to call it.
		job.State = StateCancelled
	default:
		job.State = StateComplete
	}
}

// Current returns the running or most recent sweep, if any.
func (m *Manager) Current() (Job, bool) { return m.runner.Current() }

// Cancel stops the running sweep. Reports whether there was one to stop.
//
// A cancelled sweep is not a lost sweep: every answer already recorded stays
// recorded, and re-running with a MaxAge continues where this stopped.
func (m *Manager) Cancel(id string) bool { return m.runner.Cancel(id) }
