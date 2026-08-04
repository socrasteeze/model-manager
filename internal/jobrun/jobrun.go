// Package jobrun is the at-most-one-job lifecycle shared by scanjob.Manager
// and enrichjob.Manager: register a job synchronously so the caller's first
// poll is guaranteed to see it, run the work detached, poll Current for
// progress, cancel by ID.
//
// Extracted because the two were, before this package existed, a near-total
// structural copy of each other -- same fields, same Start/Current/Cancel
// mutex dance, an ID-generating function identical apart from its string
// prefix -- with the only real difference being what a "job" actually does.
// That difference (the run() body, and the Job struct's own fields) stays in
// each package; only the surrounding at-most-one-at-a-time bookkeeping moves
// here.
//
// download.Manager was deliberately not folded into this: it runs several
// downloads concurrently rather than at most one, which is a different
// enough shape that forcing it through this abstraction would trade real
// duplication for a worse fit.
package jobrun

import (
	"context"
	"sync"
)

// State is where a job is. Lifted here because scanjob's and enrichjob's own
// State types were already byte-identical string enums before this package
// existed; each package re-exports it as a type alias so nothing that names
// scanjob.State or enrichjob.StateRunning has to change.
type State string

const (
	StateRunning   State = "running"
	StateComplete  State = "complete"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Runnable is what Runner needs from a caller's job type: whether it is
// still executing, and the ID a poller would name to cancel it. Value
// receivers, since Current hands jobs back by value and Runner never
// mutates through this interface -- the caller's own run() function does
// that, under Lock/Unlock.
type Runnable interface {
	Running() bool
	JobID() string
}

// Runner owns the at-most-one running job and the record of the last one.
// J is the caller's own job type; Runner stores and returns it but only ever
// looks inside it through the Runnable methods.
type Runner[J Runnable] struct {
	genID func(seq int64) string

	mu      sync.Mutex
	current *J
	cancel  context.CancelFunc
	seq     int64
}

// New builds a Runner. genID turns a monotonic sequence number into the ID a
// poller sees; GenID below is the shared implementation both existing
// packages used, parameterized by their own prefix.
func New[J Runnable](genID func(seq int64) string) *Runner[J] {
	return &Runner[J]{genID: genID}
}

// Start registers a new job, provided nothing is already running: build is
// called with the freshly allocated ID to construct the job value, which is
// stored as current -- and therefore visible to Current -- before this
// returns, so the caller's very next poll is guaranteed to see it.
//
// ok is false, with snapshot holding the currently-running job instead of a
// new one, when a job was already in flight. That mirrors the shape both
// existing callers relied on: the refusal still carries the running job, so
// a client that lost track of it can pick it back up rather than being told
// no with nothing to poll.
//
// snapshot is a value copy taken while still holding the lock, on both the
// refusal and the success path. The caller must return snapshot, not
// dereference job itself: by the time Start returns, the goroutine the
// caller is about to spawn does not exist yet on the success path, but
// dereferencing job outside this lock is still a data race against Lock/
// Unlock -- job exists only so the caller can pass it to its own run
// function, not so the caller can read through it directly.
//
// The caller is responsible for starting the goroutine that does the actual
// work, using the returned context -- Runner only owns the bookkeeping, not
// the work itself.
func (r *Runner[J]) Start(build func(id string) *J) (job *J, snapshot J, ctx context.Context, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current != nil && (*r.current).Running() {
		return r.current, *r.current, nil, false
	}

	r.seq++
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(context.Background())
	job = build(r.genID(r.seq))
	r.current, r.cancel = job, cancel
	return job, *job, ctx, true
}

// InFlight reports whether a job is currently running, and a snapshot of it
// if so. Lets a caller check the guard before doing expensive or fallible
// setup work that would otherwise run -- and be discarded -- on every
// refused Start; Start re-checks the same guard itself, so skipping this is
// always safe, just potentially wasteful.
func (r *Runner[J]) InFlight() (J, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && (*r.current).Running() {
		return *r.current, true
	}
	var zero J
	return zero, false
}

// Current returns the running or most recent job, if any.
func (r *Runner[J]) Current() (J, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		var zero J
		return zero, false
	}
	return *r.current, true
}

// Cancel stops the running job. Reports whether there was one to stop.
func (r *Runner[J]) Cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || !(*r.current).Running() {
		return false
	}
	if id != "" && id != (*r.current).JobID() {
		return false
	}
	if r.cancel != nil {
		r.cancel()
	}
	return true
}

// Lock and Unlock let a caller's own run function mutate the job it was
// handed under the same mutex Start, Current and Cancel use, so a poll can
// never observe a job half-written mid-update.
func (r *Runner[J]) Lock()   { r.mu.Lock() }
func (r *Runner[J]) Unlock() { r.mu.Unlock() }

// GenID builds an ID generator: prefix-0, prefix-1, prefix-2, ... Shared
// because scanjob's scanID and enrichjob's jobID were identical apart from
// the prefix.
func GenID(prefix string) func(seq int64) string {
	return func(seq int64) string {
		const digits = "0123456789"
		if seq == 0 {
			return prefix + "-0"
		}
		var buf []byte
		for n := seq; n > 0; n /= 10 {
			buf = append([]byte{digits[n%10]}, buf...)
		}
		return prefix + "-" + string(buf)
	}
}
