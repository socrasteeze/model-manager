package download

// Tests for the job lifecycle: single-flight, retry, cancel, and the
// verification paths added alongside them. These exist because the failure
// they guard against — two transfers interleaving into one .part file — is
// silent, multi-gigabyte, and lands inside the user's model root.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTokenIsScopedByHost is the leak test: one Manager serving two hosts must
// present each host only its own credential.
func TestTokenIsScopedByHost(t *testing.T) {
	var gotA, gotB string
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotA = r.Header.Get("Authorization")
		w.Write([]byte("aaaa"))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB = r.Header.Get("Authorization")
		w.Write([]byte("bbbb"))
	}))
	defer srvB.Close()

	m := newManager(t)
	m.TokenFor = func(url string) string {
		if strings.HasPrefix(url, srvA.URL) {
			return "token-for-a"
		}
		return ""
	}

	if _, err := m.Fetch(context.Background(), Job{URL: srvA.URL + "/a.bin", DestDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fetch(context.Background(), Job{URL: srvB.URL + "/b.bin", DestDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	if gotA != "Bearer token-for-a" {
		t.Errorf("host A saw %q, want its own token", gotA)
	}
	if gotB != "" {
		t.Errorf("host B saw %q, want no credential at all", gotB)
	}
}

// TestDuplicateFetchRejectedWhileInFlight: the second request for the same
// file must not open the same .part file — that is the interleaved-corruption
// bug — and must instead be told the job is already running.
func TestDuplicateFetchRejectedWhileInFlight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("head"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		w.Write([]byte("tail"))
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })

	m := newManager(t)
	dest := t.TempDir()

	firstDone := make(chan Job, 1)
	go func() {
		j, _ := m.Fetch(context.Background(), Job{URL: srv.URL + "/x.bin", DestDir: dest})
		firstDone <- j
	}()

	// Wait until the first transfer is genuinely in flight.
	deadline := time.After(5 * time.Second)
	for {
		if jobs := m.Jobs(); len(jobs) == 1 && jobs[0].State == StateDownloading {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first transfer never started")
		case <-time.After(5 * time.Millisecond):
		}
	}

	dup, err := m.Fetch(context.Background(), Job{URL: srv.URL + "/x.bin", DestDir: dest})
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("duplicate fetch: err = %v, want ErrInFlight", err)
	}
	if dup.ID == "" || dup.State != StateDownloading {
		t.Fatalf("duplicate should get the running job's snapshot, got %+v", dup)
	}

	once.Do(func() { close(release) })
	first := <-firstDone
	if first.State != StateComplete {
		t.Fatalf("first transfer state = %s", first.State)
	}

	// Terminal now — the same ID is startable again (retry path).
	again, err := m.Fetch(context.Background(), Job{URL: srv.URL + "/x.bin", DestDir: dest})
	if err != nil || again.State != StateComplete {
		t.Fatalf("retry after terminal failed: %v (%s)", err, again.State)
	}
}

// TestStartRegistersSynchronously: the returned snapshot must already be
// visible to Jobs(), or a client's first poll can race the goroutine, see
// nothing, and stop polling forever.
func TestStartRegistersSynchronously(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("content"))
	}))
	defer srv.Close()

	m := newManager(t)
	snap, err := m.Start(context.Background(), Job{URL: srv.URL + "/y.bin", DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.State != StatePending {
		t.Fatalf("snapshot = %+v, want a pending job with an ID", snap)
	}
	if _, ok := m.Job(snap.ID); !ok {
		t.Fatal("job not visible immediately after Start returned")
	}

	waitTerminal(t, m, snap.ID)
	if j, _ := m.Job(snap.ID); j.State != StateComplete {
		t.Fatalf("state = %s, want complete", j.State)
	}
}

// TestCancelStopsInFlight: cancel must stop the transfer, mark it cancelled,
// and keep the partial so a retry resumes rather than restarting.
func TestCancelStopsInFlight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start-of-file"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })

	m := newManager(t)
	snap, err := m.Start(context.Background(), Job{URL: srv.URL + "/z.bin", DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if j, _ := m.Job(snap.ID); j.State == StateDownloading && j.Downloaded > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("transfer never reached downloading")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if !m.Cancel(snap.ID) {
		t.Fatal("Cancel returned false for an in-flight job")
	}
	waitTerminal(t, m, snap.ID)
	j, _ := m.Job(snap.ID)
	if j.State != StateCancelled {
		t.Fatalf("state = %s, want cancelled", j.State)
	}

	partials, _ := filepath.Glob(filepath.Join(m.WorkDir, "*.part"))
	if len(partials) != 1 {
		t.Fatalf("%d partials, want the resumable one kept", len(partials))
	}

	// Cancel on a terminal job is a no-op refusal; Remove forgets it.
	if m.Cancel(snap.ID) {
		t.Error("Cancel succeeded on a terminal job")
	}
	if !m.Remove(snap.ID) {
		t.Error("Remove failed on a terminal job")
	}
	if _, ok := m.Job(snap.ID); ok {
		t.Error("job still listed after Remove")
	}
}

// TestSizeMismatchQuarantines: with no hash available, the promised size is
// the only independent check — an HTML interstitial must not be published
// under a .safetensors name.
func TestSizeMismatchQuarantines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>please log in</html>"))
	}))
	defer srv.Close()

	m := newManager(t)
	dest := t.TempDir()

	job, err := m.Fetch(context.Background(), Job{
		URL:          srv.URL + "/big.safetensors",
		DestDir:      dest,
		ExpectedSize: 1 << 30,
	})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want a size mismatch", err)
	}
	if job.State != StateQuarantined {
		t.Fatalf("state = %s, want quarantined", job.State)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatal("mismatched download reached the destination")
	}

	// Correct size passes.
	ok, err := m.Fetch(context.Background(), Job{
		URL:          srv.URL + "/big.safetensors",
		DestDir:      dest,
		ExpectedSize: int64(len("<html>please log in</html>")),
	})
	if err != nil || ok.State != StateComplete {
		t.Fatalf("correct-size download failed: %v (%s)", err, ok.State)
	}
}

// Test416OversizedPartialTruncates: a poisoned partial larger than the real
// file used to be declared complete on 416. It must restart instead.
func Test416OversizedPartialTruncates(t *testing.T) {
	content := []byte("the real file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	m := newManager(t)
	dest := t.TempDir()

	job := Job{URL: srv.URL + "/f.bin", DestDir: dest}
	if err := normalize(&job); err != nil {
		t.Fatal(err)
	}
	// Seed an oversized poisoned partial under the job's own resume path.
	partial := filepath.Join(m.WorkDir, job.ID+".part")
	if err := os.WriteFile(partial, []byte("garbage far longer than the real file content is"), 0o644); err != nil {
		t.Fatal(err)
	}

	done, err := m.Fetch(context.Background(), job)
	if err != nil || done.State != StateComplete {
		t.Fatalf("fetch: %v (%s)", err, done.State)
	}
	got, err := os.ReadFile(done.FinalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("published %q, want the real file — the oversized partial won", got)
	}
}

// TestOnCompleteErrorRecorded: an indexing failure is the user's business.
func TestOnCompleteErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fine"))
	}))
	defer srv.Close()

	m := newManager(t)
	m.OnComplete = func(j Job) string { return "boom" }

	job, err := m.Fetch(context.Background(), Job{URL: srv.URL + "/g.bin", DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateComplete {
		t.Fatalf("state = %s", job.State)
	}
	// Fetch's return snapshot may predate the hook write; read the map.
	final, _ := m.Job(job.ID)
	if final.IndexError != "boom" {
		t.Fatalf("IndexError = %q, want the hook's message", final.IndexError)
	}
}

// TestJobsSortedByStartedAt: map iteration order must not reach the API.
func TestJobsSortedByStartedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	m := newManager(t)
	for i := 0; i < 5; i++ {
		if _, err := m.Fetch(context.Background(),
			Job{URL: fmt.Sprintf("%s/f%d.bin", srv.URL, i), DestDir: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	for run := 0; run < 3; run++ {
		jobs := m.Jobs()
		for i := 1; i < len(jobs); i++ {
			if jobs[i].StartedAt.Before(jobs[i-1].StartedAt) {
				t.Fatalf("jobs out of order at %d", i)
			}
		}
	}
}

func waitTerminal(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if j, ok := m.Job(id); ok && terminal(j.State) {
			return
		}
		select {
		case <-deadline:
			j, _ := m.Job(id)
			t.Fatalf("job never reached a terminal state: %+v", j)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
