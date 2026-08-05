package scanjob

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

func testManager(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(testutil.TempDir(t), "master.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	root := testutil.TempDir(t)
	// A file big enough to be worth hashing, so the scan has something to do.
	if err := os.WriteFile(filepath.Join(root, "a.safetensors"),
		make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRoot(root, "", ""); err != nil {
		t.Fatal(err)
	}
	return New(st, scan.Options{WorkersPerDevice: 1}), st, root
}

func waitFor(t *testing.T, m *Manager, want State) Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if job, ok := m.Current(); ok && job.State == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := m.Current()
	t.Fatalf("scan never reached %s (state %s, error %q)", want, job.State, job.Error)
	return Job{}
}

func TestScanRunsAndStampsTheRoot(t *testing.T) {
	m, st, root := testManager(t)

	job, err := m.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Error("no job id returned; the caller cannot poll for progress")
	}
	if job.State != StateRunning {
		t.Errorf("state = %q immediately after Start; registration must be synchronous", job.State)
	}

	done := waitFor(t, m, StateComplete)
	if done.FilesDone == 0 {
		t.Error("scan completed having done nothing")
	}

	r, err := st.RootByPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.LastScannedAt == "" {
		t.Error("a completed scan did not stamp last_scanned_at")
	}
	if r.Files != 1 {
		t.Errorf("root reports %d files, want 1", r.Files)
	}
}

// One scan at a time. Two concurrent scans of overlapping trees contend on the
// single SQLite writer for no gain, and if the trees do overlap they fight over
// `present` -- the same reason nested roots are refused outright.
func TestOnlyOneScanRunsAtATime(t *testing.T) {
	m, _, _ := testManager(t)

	first, err := m.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(nil)
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("second scan started concurrently: %v", err)
	}
	// The refusal still carries the running job, so a client can attach to it
	// rather than being told only that it failed.
	if second.ID != first.ID {
		t.Errorf("refusal reported job %q, want the running %q", second.ID, first.ID)
	}

	waitFor(t, m, StateComplete)

	// Once terminal, a new scan is allowed.
	if _, err := m.Start(nil); err != nil {
		t.Fatalf("could not start a scan after the previous one finished: %v", err)
	}
}

func TestStartWithNoRootsScansEveryEnabledOne(t *testing.T) {
	m, st, root := testManager(t)

	job, err := m.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Roots) != 1 || job.Roots[0] != root {
		t.Errorf("roots = %v, want [%s]", job.Roots, root)
	}
	waitFor(t, m, StateComplete)

	// A disabled root is not scanned, and with none left there is nothing to do
	// rather than an implicit full-disk walk.
	r, _ := st.RootByPath(root)
	if err := st.SetRootEnabled(r.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(nil); err == nil {
		t.Error("started a scan with every root disabled")
	}
}

func TestCancelStopsTheRunningScan(t *testing.T) {
	m, _, _ := testManager(t)

	job, err := m.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Cancel(job.ID) {
		t.Fatal("cancel reported nothing to stop")
	}
	// Cancelled or complete are both acceptable: a one-file scan can finish
	// before the cancel lands, and that is not a failure of cancellation.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := m.Current(); ok && j.State != StateRunning {
			if j.State != StateCancelled && j.State != StateComplete {
				t.Fatalf("state = %q after cancel", j.State)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan still running long after cancel")
}

func TestCancelWithAWrongIDDoesNothing(t *testing.T) {
	m, _, _ := testManager(t)
	if _, err := m.Start(nil); err != nil {
		t.Fatal(err)
	}
	if m.Cancel("scan-not-this-one") {
		t.Error("cancelled a scan the caller did not name")
	}
}
