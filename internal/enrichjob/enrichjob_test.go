package enrichjob

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
)

func testManager(t *testing.T, handler http.HandlerFunc) (*Manager, *store.Store) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := origin.NewClient()
	client.MinInterval = 0
	client.CivitaiBase = srv.URL
	client.APIKey, client.HFToken = "", ""
	// Real exponential backoff between retries; capping retries at 1 (the
	// minimum that still exercises isRateLimit's retry-then-give-up path)
	// keeps a rate-limited-run test from taking tens of seconds.
	client.MaxRetries = 1

	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	return New(st, blobs, func() *origin.Client { return client }), st
}

func seed(t *testing.T, st *store.Store, sha string) {
	t.Helper()
	run, err := st.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 1000, Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: "/models/" + sha + ".safetensors", Root: "/models",
			Device: 1, Inode: uint64(len(sha)) + uint64(sha[0]), Size: 1000, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
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
	t.Fatalf("run never reached %s (state %s, error %q)", want, job.State, job.Error)
	return Job{}
}

// A run the provider rate-limited ends in StateComplete -- it was not
// cancelled and nothing failed outright -- so RateLimited is the only signal
// that distinguishes it from an exhaustive sweep, and it has to survive the
// trip through origin.Enrich's Progress callback into the polled Job.
func TestRateLimitedRunIsFlaggedNotSilentlyMarkedComplete(t *testing.T) {
	m, st := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	seed(t, st, "aaa")
	seed(t, st, "bbb")

	if _, err := m.Start("all", Options{SkipImages: true}); err != nil {
		t.Fatal(err)
	}
	job := waitFor(t, m, StateComplete)

	if !job.RateLimited {
		t.Fatal("job.RateLimited is false after the provider rate-limited every request")
	}
	if job.ModelsDone == job.ModelsTotal {
		t.Fatalf("models_done (%d) == models_total (%d): a rate-limited run must not report full completion",
			job.ModelsDone, job.ModelsTotal)
	}
	if job.ModelsDone != 1 || job.ModelsTotal != 2 {
		t.Fatalf("models_done/total = %d/%d, want 1/2 (one model attempted before the run gave up)",
			job.ModelsDone, job.ModelsTotal)
	}
}

// A poller mid-run should see live counts, not every field pinned at zero
// until the run happens to finish.
func TestLiveCountsUpdateDuringARun(t *testing.T) {
	var seen int32
	m, st := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/image.png") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&seen, 1)
		// Slow enough that a poll is very likely to land mid-request.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusNotFound)
	})
	for _, sha := range []string{"a1", "b2", "c3", "d4", "e5"} {
		seed(t, st, sha)
	}

	if _, err := m.Start("all", Options{SkipImages: true}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var sawLiveProgress bool
	for time.Now().Before(deadline) {
		job, ok := m.Current()
		if !ok {
			t.Fatal("no job registered")
		}
		if job.State != StateRunning {
			break
		}
		// Missing is incremented synchronously for every 404 the loop has
		// already processed -- if it is ever nonzero while State is still
		// "running", live counters are flowing through, not just the terminal
		// copy.
		if job.Missing > 0 {
			sawLiveProgress = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, m, StateComplete)

	if !sawLiveProgress {
		t.Fatal("job.Missing never went above zero while the run was still in progress; " +
			"live counters are not updating until the run terminates")
	}
	if atomic.LoadInt32(&seen) < 2 {
		t.Fatalf("provider only saw %d requests; the run finished too fast for this test to be meaningful", seen)
	}
}
