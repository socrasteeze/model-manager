package comfy

// Renders as background jobs.
//
// A render is tens of seconds of someone else's GPU. Holding the HTTP request
// open for it would tie the render's fate to a browser tab, which is the same
// reason downloads and scans are jobs -- so this is the same shape a third
// time: register synchronously so the ID goes back in the 202, run detached,
// poll for state, cancel by ID.
//
// Unlike a scan, several may run at once. ComfyUI has its own queue and is
// better placed to decide the order than this app is; what is capped here is
// how many renders this daemon will track, so a stuck ComfyUI cannot grow an
// unbounded job table.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is where a render is.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateComplete State = "complete"
	StateFailed   State = "failed"
	StateCanceled State = "cancelled"
)

// MaxTrackedJobs bounds the job table.
const MaxTrackedJobs = 50

// Job is one render.
type Job struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	State  State  `json:"state"`

	PromptID  string     `json:"prompt_id,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// ImageSHA256 is the preview that was attached, once one was.
	ImageSHA256 string `json:"image_sha256,omitempty"`

	Error string `json:"error,omitempty"`
}

func (j Job) active() bool { return j.State == StateQueued || j.State == StateRunning }

// Attach stores a rendered image against a model and returns its content
// address. Supplied by the caller so this package never touches the store or
// the blob store directly.
type Attach func(sha string, image []byte) (string, error)

// Manager owns the running renders.
type Manager struct {
	// Dial builds a client from current settings. A function rather than a
	// client, because the ComfyUI address is a setting the user can change
	// while the daemon runs, and a render should use the address that is
	// configured now.
	Dial func() (*Client, error)

	Attach Attach

	// PollInterval is how often a running prompt is checked.
	PollInterval time.Duration

	// Timeout bounds one render.
	Timeout time.Duration

	mu      sync.Mutex
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc
	seq     int64
}

// NewManager builds a Manager.
func NewManager(dial func() (*Client, error), attach Attach) *Manager {
	return &Manager{
		Dial:         dial,
		Attach:       attach,
		PollInterval: time.Second,
		Timeout:      10 * time.Minute,
		jobs:         map[string]*Job{},
		cancels:      map[string]context.CancelFunc{},
	}
}

// ErrInFlight means this model already has a render running.
//
// Per model rather than globally: two renders of the same model race to attach
// a preview and one of them is wasted GPU time, but renders of *different*
// models are exactly what a queue is for.
var ErrInFlight = errors.New("comfy: a render for this model is already running")

// Start queues a render of graph for sha.
func (m *Manager) Start(sha string, graph json.RawMessage) (Job, error) {
	if m.Attach == nil {
		return Job{}, errors.New("comfy: no way to attach the result")
	}
	// Shape-checked before anything is registered. Queue would catch it too,
	// but by then the caller has a 202 and an ID, and an editor-format workflow
	// would look like a render that started and then quietly died -- when it is
	// a mistake the user can fix in one step, if told immediately.
	if err := checkAPIFormat(graph); err != nil {
		return Job{}, err
	}
	// Fail before registering a job if ComfyUI is not configured, so the user
	// gets "set the address" rather than a job that fails a second later.
	client, err := m.Dial()
	if err != nil {
		return Job{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, j := range m.jobs {
		if j.SHA256 == sha && j.active() {
			return *j, ErrInFlight
		}
	}
	m.evictLocked()

	m.seq++
	id := fmt.Sprintf("render-%d", m.seq)
	ctx, cancel := context.WithTimeout(context.Background(), m.Timeout)

	job := &Job{ID: id, SHA256: sha, State: StateQueued, StartedAt: time.Now()}
	m.jobs[id] = job
	m.cancels[id] = cancel

	go m.run(ctx, client, job, graph)
	return *job, nil
}

func (m *Manager) run(ctx context.Context, client *Client, job *Job, graph json.RawMessage) {
	defer func() {
		m.mu.Lock()
		if cancel, ok := m.cancels[job.ID]; ok {
			cancel()
			delete(m.cancels, job.ID)
		}
		m.mu.Unlock()
	}()

	fail := func(err error) {
		m.finish(job, StateFailed, "", err)
	}

	promptID, err := client.Queue(ctx, graph, "model-manager-"+job.ID)
	if err != nil {
		fail(err)
		return
	}
	m.mu.Lock()
	job.PromptID = promptID
	job.State = StateRunning
	m.mu.Unlock()

	result, err := client.Wait(ctx, promptID, m.PollInterval)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.finish(job, StateCanceled, "", nil)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			fail(fmt.Errorf("comfy: render did not finish within %s", m.Timeout))
			return
		}
		fail(err)
		return
	}

	// The first image is the thumbnail. A workflow that saves several is
	// usually a batch, and picking one beats attaching all of them to a model
	// that needs one picture.
	data, err := client.Fetch(ctx, result.Images[0])
	if err != nil {
		fail(err)
		return
	}
	imageSHA, err := m.Attach(job.SHA256, data)
	if err != nil {
		fail(err)
		return
	}
	m.finish(job, StateComplete, imageSHA, nil)
}

func (m *Manager) finish(job *Job, state State, imageSHA string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	job.State = state
	job.EndedAt = &now
	job.ImageSHA256 = imageSHA
	if err != nil {
		job.Error = err.Error()
	}
}

// Jobs returns every tracked render, newest first.
func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Job returns one render.
func (m *Manager) Job(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Cancel stops a running render. ComfyUI keeps the queued prompt -- this stops
// this app waiting for it, which is the part the user asked to stop.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// evictLocked drops the oldest finished jobs once the table is full. Only
// finished ones: a running render is not history to trim.
func (m *Manager) evictLocked() {
	if len(m.jobs) < MaxTrackedJobs {
		return
	}
	var finished []*Job
	for _, j := range m.jobs {
		if !j.active() {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].StartedAt.Before(finished[j].StartedAt)
	})
	for _, j := range finished {
		if len(m.jobs) < MaxTrackedJobs {
			return
		}
		delete(m.jobs, j.ID)
	}
}
