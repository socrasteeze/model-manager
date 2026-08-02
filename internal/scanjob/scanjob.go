// Package scanjob runs a scan in the background so the daemon can start one
// from an HTTP request.
//
// scan.Run has always been cancellable and always reported progress through a
// callback -- it was simply only ever called from the CLI, where a blocking
// call and a terminal spinner are the right shape. Over HTTP neither is: a scan
// of a 19k-file library is minutes of work, and holding the request open ties
// the scan's fate to a browser tab.
//
// The shape is the one the download manager already established: register
// synchronously so the caller gets an ID back in the 202, run detached, poll
// for progress, cancel by ID.
//
// One scan at a time, deliberately. Two concurrent scans of overlapping trees
// contend on the single SQLite writer for no gain, and if the trees actually do
// overlap they fight over `present` -- the same reason nested roots are refused
// outright.
package scanjob

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/store"
)

// ErrInFlight means a scan is already running.
var ErrInFlight = errors.New("scanjob: a scan is already running")

// State is where a scan is.
type State string

const (
	StateRunning   State = "running"
	StateComplete  State = "complete"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Job is one scan, as reported to a poller.
type Job struct {
	ID    string   `json:"id"`
	Roots []string `json:"roots"`
	State State    `json:"state"`

	StartedAt time.Time `json:"started_at"`

	// A pointer, because omitempty does not omit a zero time.Time -- a running
	// scan would otherwise report finishing in year 1.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	FilesTotal  int64 `json:"files_total"`
	FilesDone   int64 `json:"files_done"`
	FilesHashed int64 `json:"files_hashed"`
	FilesCached int64 `json:"files_cached"`
	BytesTotal  int64 `json:"bytes_total"`
	BytesDone   int64 `json:"bytes_done"`
	Errors      int64 `json:"errors"`

	Error string `json:"error,omitempty"`
}

// Manager owns the at-most-one running scan and the record of the last one.
type Manager struct {
	st   *store.Store
	opts scan.Options

	mu      sync.Mutex
	current *Job
	cancel  context.CancelFunc
	seq     int64
}

// New builds a Manager. The options supplied here carry the tuning (workers per
// device, buffer size); Roots are set per run.
func New(st *store.Store, opts scan.Options) *Manager {
	return &Manager{st: st, opts: opts}
}

// Start begins a scan of roots. It registers the job before returning, so the
// caller's first poll is guaranteed to see it.
//
// An empty roots slice means "every enabled managed root", which is what the
// rescan-everything button asks for.
func (m *Manager) Start(roots []string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current != nil && m.current.State == StateRunning {
		return *m.current, ErrInFlight
	}

	if len(roots) == 0 {
		enabled, err := m.st.EnabledRootPaths()
		if err != nil {
			return Job{}, err
		}
		roots = enabled
	}
	if len(roots) == 0 {
		return Job{}, errors.New("scanjob: no roots to scan")
	}

	m.seq++
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:        scanID(m.seq),
		Roots:     append([]string(nil), roots...),
		State:     StateRunning,
		StartedAt: time.Now(),
	}
	m.current = job
	m.cancel = cancel

	go m.run(ctx, job)
	return *job, nil
}

func (m *Manager) run(ctx context.Context, job *Job) {
	opts := m.opts
	opts.Roots = job.Roots
	opts.Progress = func(s scan.Snapshot) {
		m.mu.Lock()
		defer m.mu.Unlock()
		job.FilesTotal = s.FilesTotal
		job.FilesDone = s.FilesDone
		job.FilesHashed = s.FilesHashed
		job.FilesCached = s.FilesCached
		job.BytesTotal = s.BytesTotal
		job.BytesDone = s.BytesDone
		job.Errors = s.Errors
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = time.Second
	}

	result, err := scan.Run(ctx, m.st, opts)

	m.mu.Lock()
	defer m.mu.Unlock()
	finished := time.Now()
	job.FinishedAt = &finished
	switch {
	case err != nil:
		job.State = StateFailed
		job.Error = err.Error()
	case result != nil && result.Cancelled:
		job.State = StateCancelled
	default:
		job.State = StateComplete
	}

	// last_scanned_at is stamped only for roots the run actually finished. A
	// cancelled scan swept nothing, and claiming it as a completed scan would
	// make a half-walked root look freshly verified.
	if result != nil {
		for _, r := range result.Roots {
			if r.Status == store.StatusCompleted {
				_ = m.st.MarkRootScanned(r.Root)
			}
		}
	}
}

// Current returns the running or most recent scan, if any.
func (m *Manager) Current() (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return Job{}, false
	}
	return *m.current, true
}

// Cancel stops the running scan. Reports whether there was one to stop.
//
// A cancelled scan is not a lost scan: everything committed before the stop
// stays committed, and the absence sweep simply does not run for roots that
// did not finish, which is the whole reason the sweep is gated on completion.
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

func scanID(seq int64) string {
	const digits = "0123456789"
	if seq == 0 {
		return "scan-0"
	}
	var buf []byte
	for n := seq; n > 0; n /= 10 {
		buf = append([]byte{digits[n%10]}, buf...)
	}
	return "scan-" + string(buf)
}
