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

	"github.com/socrasteeze/model-manager/internal/hashing"
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
	m.OnComplete = func(j Job, _ *hashing.Result) string { return "boom" }

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

// rangeServer serves content in two halves and records the conditional headers
// each request carried, so a test can assert what was sent as well as what came
// back.
type rangeServer struct {
	body       []byte
	etag       string
	mu         sync.Mutex
	ifRanges   []string // one entry per request, "" when the header was absent
	rangeSeen  []string
	cutAtFirst int // bytes to send before hanging up on the first request
}

func (rs *rangeServer) handler(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	n := len(rs.ifRanges)
	rs.ifRanges = append(rs.ifRanges, r.Header.Get("If-Range"))
	rs.rangeSeen = append(rs.rangeSeen, r.Header.Get("Range"))
	rs.mu.Unlock()

	if rs.etag != "" {
		w.Header().Set("ETag", rs.etag)
	}
	w.Header().Set("Accept-Ranges", "bytes")

	start := 0
	if rng := r.Header.Get("Range"); rng != "" {
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start > 0 && start <= len(rs.body) {
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(rs.body)-1, len(rs.body)))
			w.WriteHeader(http.StatusPartialContent)
		}
	}
	remaining := rs.body[start:]
	if n == 0 && rs.cutAtFirst > 0 && rs.cutAtFirst < len(remaining) {
		// Half a body and then a hangup, which is what a dropped connection
		// looks like to the client -- a clean short write would read as a
		// complete (if wrong-sized) response and never trigger a resume.
		w.Write(remaining[:rs.cutAtFirst])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	w.Write(remaining)
}

func (rs *rangeServer) conditionals() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.ifRanges...)
}

// A resume must carry the validator the server itself gave us, so the server can
// answer "that is stale" instead of splicing the tail of a new representation
// onto the head of an old one.
func TestResumeReplaysTheServersOwnValidator(t *testing.T) {
	rs := &rangeServer{body: []byte(strings.Repeat("abcdefgh", 512)),
		etag: `"v1-strong"`, cutAtFirst: 1000}
	srv := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer srv.Close()

	m := newManager(t)
	m.MaxRetries = 3
	defer func(prev bool) { backoffTestHook = prev }(backoffTestHook)
	backoffTestHook = true

	dest := t.TempDir()
	job, err := m.Fetch(context.Background(), Job{
		URL: srv.URL + "/m.safetensors", DestDir: dest, Filename: "m.safetensors",
		ExpectedSize: int64(len(rs.body)),
	})
	if err != nil {
		t.Fatalf("fetch: %v (state %s)", err, job.State)
	}

	conds := rs.conditionals()
	if len(conds) < 2 {
		t.Fatalf("expected a resume; requests = %d", len(conds))
	}
	if conds[0] != "" {
		t.Errorf("the first request carried If-Range %q; nothing had been validated yet", conds[0])
	}
	if conds[1] != `"v1-strong"` {
		t.Errorf("resume sent If-Range %q, want the server's own ETag", conds[1])
	}

	got, err := os.ReadFile(filepath.Join(dest, "m.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(rs.body) {
		t.Errorf("resumed file is %d bytes, want %d", len(got), len(rs.body))
	}
}

// The regression guard for the version of this that was proposed and rejected:
// sending our own expected sha256 as If-Range to a public provider.
//
// A provider's ETag is a CDN entity tag that can never equal our hash, so the
// mismatch is guaranteed. RFC 7233 then requires a 200 with the whole body, and
// the 200 branch truncates the partial -- so every resume would restart from
// zero and a large transfer over a flaky link could never finish. No invented
// validator is better than a wrong one.
func TestResumeSendsNoInventedValidatorToAPublicProvider(t *testing.T) {
	rs := &rangeServer{body: []byte(strings.Repeat("x", 4096)), cutAtFirst: 500}
	srv := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer srv.Close()

	m := newManager(t)
	m.MaxRetries = 2
	defer func(prev bool) { backoffTestHook = prev }(backoffTestHook)
	backoffTestHook = true

	// An ordinary download: an expected hash, but no upstream. This is the exact
	// shape the rejected design would have sent a sha-based If-Range for.
	job, err := m.Fetch(context.Background(), Job{
		URL: srv.URL + "/m.safetensors", DestDir: t.TempDir(), Filename: "m.safetensors",
		ExpectedSHA256: strings.Repeat("a", 64),
	})
	if err == nil {
		// The hash will not match; that is fine and not what this asserts.
		_ = job
	}

	conds := rs.conditionals()
	if len(conds) < 2 {
		t.Fatalf("no resume happened, so this asserts nothing; requests = %d", len(conds))
	}
	for i, c := range conds {
		if c != "" {
			t.Fatalf("request %d carried If-Range %q; a hash we invented is not this server's validator", i, c)
		}
	}
	// And the resume did append rather than restart, which is the property the
	// rejected design would have destroyed.
	if rs.rangeSeen[1] == "" {
		t.Error("the resume sent no Range header")
	}
}

// The one case where a validator is knowable without having seen a response: our
// own file endpoint sets its ETag to the content hash. This is what covers a
// resume on the first attempt after a daemon restart, where nothing was captured
// because the in-memory job did not survive.
func TestResumeUsesTheContentHashOnlyForAnUpstream(t *testing.T) {
	m := newManager(t)

	upstream := Job{ID: "j1", UpstreamBase: "http://hub:8737", ExpectedSHA256: "ABCDEF"}
	public := Job{ID: "j2", ExpectedSHA256: "ABCDEF"}
	captured := Job{ID: "j3", UpstreamBase: "http://hub:8737", ExpectedSHA256: "ABCDEF",
		Validator: `"from-the-server"`}
	for _, j := range []Job{upstream, public, captured} {
		job := j
		m.jobs[job.ID] = &job
	}

	if got := m.ifRange("j1"); got != `"abcdef"` {
		t.Errorf("upstream job sent %q, want the lower-cased content hash", got)
	}
	if got := m.ifRange("j2"); got != "" {
		t.Errorf("public job sent %q, want nothing", got)
	}
	if got := m.ifRange("j3"); got != `"from-the-server"` {
		t.Errorf("captured validator was overridden by the fallback: %q", got)
	}
	if got := m.ifRange("nope"); got != "" {
		t.Errorf("unknown job sent %q", got)
	}
}

// A weak ETag must never be replayed: weak comparison says two representations
// are equivalent, not byte-identical, and byte-identity is the only thing that
// makes appending to a partial safe.
func TestStrongValidatorRejectsWeakETags(t *testing.T) {
	cases := []struct {
		etag, lastMod, want string
	}{
		{`"strong"`, "", `"strong"`},
		{`W/"weak"`, "", ""},
		{`W/"weak"`, "Wed, 21 Oct 2026 07:28:00 GMT", "Wed, 21 Oct 2026 07:28:00 GMT"},
		{"", "Wed, 21 Oct 2026 07:28:00 GMT", "Wed, 21 Oct 2026 07:28:00 GMT"},
		{"", "", ""},
	}
	for _, tc := range cases {
		resp := &http.Response{Header: http.Header{}}
		if tc.etag != "" {
			resp.Header.Set("ETag", tc.etag)
		}
		if tc.lastMod != "" {
			resp.Header.Set("Last-Modified", tc.lastMod)
		}
		if got := strongValidator(resp); got != tc.want {
			t.Errorf("ETag %q / Last-Modified %q -> %q, want %q",
				tc.etag, tc.lastMod, got, tc.want)
		}
	}
}

// TestAfterCompleteIsSeparateFromIndexing pins the reason there are two hooks
// rather than one. An indexing failure means the file is not in the library and
// a rescan is the fix; a carry-over failure means it is in the library with
// thinner metadata, which a rescan cannot help. Reporting the second through
// IndexError would give the user advice that cannot work.
func TestAfterCompleteIsSeparateFromIndexing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fine"))
	}))
	defer srv.Close()

	m := newManager(t)
	m.OnComplete = func(j Job, _ *hashing.Result) string { return "" }
	m.AfterComplete = func(j Job) string { return "metadata did not come across" }

	job, err := m.Fetch(context.Background(), Job{URL: srv.URL + "/g.bin", DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	final, _ := m.Job(job.ID)
	if final.State != StateComplete {
		t.Fatalf("state = %s", final.State)
	}
	if final.MetaError != "metadata did not come across" {
		t.Errorf("MetaError = %q, want the hook's message", final.MetaError)
	}
	if final.IndexError != "" {
		t.Errorf("IndexError = %q, want it left alone", final.IndexError)
	}
}

// A failed index means there is no local record to hang carried metadata on, so
// the second hook must not run at all -- and must certainly not overwrite the
// indexing failure with a downstream symptom of it.
func TestAfterCompleteSkippedWhenIndexingFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fine"))
	}))
	defer srv.Close()

	ran := false
	m := newManager(t)
	m.OnComplete = func(j Job, _ *hashing.Result) string { return "boom" }
	m.AfterComplete = func(j Job) string { ran = true; return "should not appear" }

	job, err := m.Fetch(context.Background(), Job{URL: srv.URL + "/g.bin", DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("AfterComplete ran despite a failed index")
	}
	final, _ := m.Job(job.ID)
	if final.IndexError != "boom" {
		t.Errorf("IndexError = %q", final.IndexError)
	}
	if final.MetaError != "" {
		t.Errorf("MetaError = %q, want empty", final.MetaError)
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
