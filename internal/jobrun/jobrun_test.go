package jobrun

import (
	"testing"
	"time"
)

// fakeJob is a minimal Runnable for exercising Runner in isolation, the way
// scanjob.Job and enrichjob.Job do for real.
type fakeJob struct {
	ID    string
	State State
	A, B  int // paired fields a caller's run() writes together, under Lock
}

func (j fakeJob) Running() bool { return j.State == StateRunning }
func (j fakeJob) JobID() string { return j.ID }

func newRunner() *Runner[fakeJob] {
	return New[fakeJob](GenID("test"))
}

func TestStartRegistersBeforeReturning(t *testing.T) {
	r := newRunner()

	job, snapshot, ctx, ok := r.Start(func(id string) *fakeJob {
		return &fakeJob{ID: id, State: StateRunning}
	})
	if !ok {
		t.Fatal("Start refused an empty Runner")
	}
	if job.ID != "test-1" || snapshot.ID != "test-1" {
		t.Errorf("ID = %q / snapshot ID = %q, want test-1 (first sequence number)", job.ID, snapshot.ID)
	}
	if snapshot.State != StateRunning {
		t.Errorf("snapshot.State = %q, want running", snapshot.State)
	}
	if ctx == nil || ctx.Err() != nil {
		t.Error("Start did not hand back a live context")
	}

	// The whole point of registering before Start returns: the very next
	// Current call must already see it.
	current, ok := r.Current()
	if !ok || current.ID != job.ID {
		t.Fatalf("Current() = %+v, %v -- did not see the job Start just registered", current, ok)
	}
}

func TestStartRefusesASecondJobWhileOneIsRunning(t *testing.T) {
	r := newRunner()
	first, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("first Start refused")
	}

	second, snapshot, ctx, ok := r.Start(func(id string) *fakeJob {
		t.Fatal("build was called for a second job despite one already running")
		return nil
	})
	if ok {
		t.Fatal("Start allowed two jobs to run at once")
	}
	if ctx != nil {
		t.Error("a refused Start returned a live context")
	}
	// The refusal carries the running job, not a zero value, so a caller that
	// lost track of it can pick it back up.
	if second == nil || second.ID != first.ID {
		t.Errorf("refusal returned %+v, want the running job %+v", second, first)
	}
	if snapshot.ID != first.ID {
		t.Errorf("refusal snapshot = %+v, want the running job %+v", snapshot, first)
	}

	// Once the job is no longer running, Start must work again.
	r.Lock()
	first.State = StateComplete
	r.Unlock()
	if _, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} }); !ok {
		t.Error("Start still refused after the previous job finished")
	}
}

func TestCurrentOnAnEmptyRunner(t *testing.T) {
	r := newRunner()
	job, ok := r.Current()
	if ok {
		t.Errorf("Current() = %+v, true on a Runner nothing has ever run on", job)
	}
}

func TestInFlightReportsWithoutRegistering(t *testing.T) {
	r := newRunner()
	if _, running := r.InFlight(); running {
		t.Error("InFlight true on an empty Runner")
	}

	job, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("Start refused")
	}
	current, running := r.InFlight()
	if !running || current.ID != job.ID {
		t.Fatalf("InFlight() = %+v, %v -- did not see the running job", current, running)
	}

	// InFlight must not itself register anything -- calling it twice must not
	// consume a sequence number or otherwise disturb Start's own guard.
	if _, running := r.InFlight(); !running {
		t.Error("second InFlight call saw a different answer than the first")
	}
}

func TestCancelRequiresARunningJobAndAMatchingID(t *testing.T) {
	r := newRunner()
	if r.Cancel("") {
		t.Error("cancelled something on an empty Runner")
	}

	job, _, ctx, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("Start refused")
	}

	if r.Cancel("not-this-job") {
		t.Error("cancelled despite a non-matching id")
	}
	if ctx.Err() != nil {
		t.Fatal("context was cancelled by the wrong-id call")
	}

	if !r.Cancel(job.ID) {
		t.Fatal("Cancel reported nothing to stop for a matching, running job")
	}
	if ctx.Err() == nil {
		t.Error("the context Start returned was not cancelled")
	}

	// A second Cancel of the same (now non-running, since the caller marks it
	// terminal separately) job id should still report false once the state
	// reflects that -- Runner does not infer "stopped" from cancelling the
	// context alone.
	r.Lock()
	job.State = StateCancelled
	r.Unlock()
	if r.Cancel(job.ID) {
		t.Error("cancelled a job that was no longer running")
	}
}

func TestCancelWithNoIDStopsWhicheverIsRunning(t *testing.T) {
	r := newRunner()
	_, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("Start refused")
	}
	if !r.Cancel("") {
		t.Error("an empty id did not cancel the one running job")
	}
}

// LockUnlock is what a caller's own run() function uses to mutate the job it
// was handed, under the same mutex Start/Current/Cancel use -- verified here
// by racing a writer against Current under -race.
func TestLockUnlockSerializesWithCurrent(t *testing.T) {
	r := newRunner()
	job, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("Start refused")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.Lock()
			job.State = StateRunning
			r.Unlock()
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.Current()
		select {
		case <-done:
			return
		default:
		}
	}
	t.Fatal("writer goroutine never finished")
}

// Start's snapshot must be a point-in-time copy taken under the lock, not a
// window onto the live job -- otherwise a caller reading it after a
// concurrent writer has started mutating (or, on the refusal path, a writer
// that was already mutating before Start was even called) can observe a
// torn combination of fields that were only ever meant to change together.
func TestStartSnapshotIsNotTornByAConcurrentWriter(t *testing.T) {
	r := newRunner()
	job, _, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
	if !ok {
		t.Fatal("Start refused")
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		n := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			n++
			r.Lock()
			job.A, job.B = n, n // always written together, under the lock
			r.Unlock()
		}
	}()
	defer func() { close(stop); <-done }()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		// The refusal path: snapshot must match whatever the writer last
		// committed atomically, never a mix of two different iterations.
		_, snapshot, _, ok := r.Start(func(id string) *fakeJob { return &fakeJob{ID: id, State: StateRunning} })
		if ok {
			t.Fatal("a second Start succeeded while the first job is still running")
		}
		if snapshot.A != snapshot.B {
			t.Fatalf("torn snapshot from the refusal path: A=%d B=%d", snapshot.A, snapshot.B)
		}
	}
}

func TestGenIDProducesSequentialPrefixedIDs(t *testing.T) {
	gen := GenID("thing")
	want := []string{"thing-0", "thing-1", "thing-2", "thing-10"}
	got := []string{gen(0), gen(1), gen(2), gen(10)}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GenID(%d) = %q, want %q", i, got[i], want[i])
		}
	}
}
