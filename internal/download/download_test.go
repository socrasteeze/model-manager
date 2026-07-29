package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatal(err)
	}
	m.MaxRetries = 2
	return m
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchVerifiesAndPublishes(t *testing.T) {
	body := []byte("pretend this is a model file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m := newManager(t)
	dest := filepath.Join(t.TempDir(), "models", "loras")

	job, err := m.Fetch(context.Background(), Job{
		URL:            srv.URL + "/model.safetensors",
		DestDir:        dest,
		ExpectedSHA256: sha256Of(body),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if job.State != StateComplete {
		t.Fatalf("state = %s: %s", job.State, job.Error)
	}

	got, err := os.ReadFile(job.FinalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatal("published file does not match what was served")
	}
	if filepath.Base(job.FinalPath) != "model.safetensors" {
		t.Errorf("filename = %s", filepath.Base(job.FinalPath))
	}
}

// The check that keeps a corrupt or substituted file out of the index.
func TestChecksumMismatchQuarantines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not what was promised"))
	}))
	defer srv.Close()

	m := newManager(t)
	dest := filepath.Join(t.TempDir(), "models")

	job, err := m.Fetch(context.Background(), Job{
		URL:            srv.URL + "/m.safetensors",
		DestDir:        dest,
		ExpectedSHA256: sha256Of([]byte("something else entirely")),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if job.State != StateQuarantined {
		t.Fatalf("state = %s, want quarantined", job.State)
	}

	// Nothing may reach the destination: a half-trusted file in a models folder
	// is one a tool will happily load.
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("quarantined download still published %d file(s)", len(entries))
	}

	// Quarantine means "not admitted", not "destroyed" -- the bytes stay for
	// inspection.
	partials, _ := filepath.Glob(filepath.Join(m.WorkDir, "*.part"))
	if len(partials) != 1 {
		t.Fatalf("%d partial files kept, want the quarantined one", len(partials))
	}
}

// Multi-GB transfers drop. Everything already on disk must be reused.
func TestResumesAfterAnInterruptedTransfer(t *testing.T) {
	body := []byte(strings.Repeat("abcdefghij", 2000)) // 20 KB
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		start := int64(0)
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		}
		remaining := body[start:]
		if n == 1 {
			// Cut the first attempt short, mid-file.
			half := len(remaining) / 2
			_, _ = w.Write(remaining[:half])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Closing without writing the rest simulates a dropped connection.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return
		}
		_, _ = w.Write(remaining)
	}))
	defer srv.Close()

	m := newManager(t)
	m.MaxRetries = 3
	backoffOriginal := backoffTestHook
	backoffTestHook = true
	defer func() { backoffTestHook = backoffOriginal }()

	job, err := m.Fetch(context.Background(), Job{
		URL:            srv.URL + "/big.safetensors",
		DestDir:        filepath.Join(t.TempDir(), "models"),
		ExpectedSHA256: sha256Of(body),
	})
	if err != nil {
		t.Fatalf("Fetch: %v (state %s)", err, job.State)
	}
	if job.State != StateComplete {
		t.Fatalf("state = %s: %s", job.State, job.Error)
	}
	if attempts.Load() < 2 {
		t.Fatalf("only %d attempt(s); the resume path was not exercised", attempts.Load())
	}

	got, _ := os.ReadFile(job.FinalPath)
	if string(got) != string(body) {
		t.Fatalf("resumed file is %d bytes, want %d — the resume appended wrongly",
			len(got), len(body))
	}
}

// A server that ignores the Range header sends the whole file again. Appending
// it onto the partial would silently double the file.
func TestServerIgnoringRangeRestartsCleanly(t *testing.T) {
	body := []byte(strings.Repeat("x", 5000))
	m := newManager(t)

	// Pre-seed a partial as though an earlier attempt had run.
	id := jobID("http://example/m.safetensors", "m.safetensors")
	if err := os.WriteFile(filepath.Join(m.WorkDir, id+".part"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore Range and answer 200 with the whole body.
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	job, err := m.Fetch(context.Background(), Job{
		ID:             id,
		URL:            srv.URL + "/m.safetensors",
		Filename:       "m.safetensors",
		DestDir:        filepath.Join(t.TempDir(), "models"),
		ExpectedSHA256: sha256Of(body),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := os.ReadFile(job.FinalPath)
	if len(got) != len(body) {
		t.Fatalf("file is %d bytes, want %d — the stale partial was appended to", len(got), len(body))
	}
}

// Civitai gates some models behind an account, and returns HTML rather than the
// file. Writing that to disk named .safetensors would be worse than failing.
func TestAuthFailureIsReportedWithTheFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("the real file"))
	}))
	defer srv.Close()

	m := newManager(t)
	_, err := m.Fetch(context.Background(), Job{
		URL:     srv.URL + "/gated.safetensors",
		DestDir: filepath.Join(t.TempDir(), "models"),
	})
	if err == nil {
		t.Fatal("an unauthorized download reported success")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v; it should say what to do about it", err)
	}

	// With the key, the same request succeeds.
	m.APIKey = "secret"
	job, err := m.Fetch(context.Background(), Job{
		URL:     srv.URL + "/gated.safetensors",
		DestDir: filepath.Join(t.TempDir(), "models"),
	})
	if err != nil || job.State != StateComplete {
		t.Fatalf("authenticated download failed: %v (%s)", err, job.State)
	}
}

// §14: the tool never modifies an existing file. A download landing on a taken
// name is exactly where that matters.
func TestExistingFileIsNeverOverwritten(t *testing.T) {
	body := []byte("new content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := t.TempDir()
	existing := filepath.Join(dest, "m.safetensors")
	if err := os.WriteFile(existing, []byte("PRECIOUS ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newManager(t)
	job, err := m.Fetch(context.Background(), Job{
		URL: srv.URL + "/m.safetensors", DestDir: dest, ExpectedSHA256: sha256Of(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.FinalPath == existing {
		t.Fatal("the download overwrote an existing file")
	}

	kept, _ := os.ReadFile(existing)
	if string(kept) != "PRECIOUS ORIGINAL" {
		t.Fatal("the existing file was modified")
	}
}

func TestCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m := newManager(t)
	job, err := m.Fetch(ctx, Job{URL: srv.URL + "/m.safetensors", DestDir: t.TempDir()})
	if err == nil {
		t.Fatal("a cancelled download reported success")
	}
	if job.State != StateCancelled {
		t.Fatalf("state = %s, want cancelled", job.State)
	}
}

// A hostile filename must not place a file outside the chosen destination.
func TestFilenameSanitization(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":       "passwd",
		"/absolute/path/m.bin":   "m.bin",
		`..\..\windows\evil.exe`: "evil.exe",
		"....//m.safetensors":    "m.safetensors",
		"":                       "download.bin",
		"normal.safetensors":     "normal.safetensors",
		"weird:name*.bin":        "weird_name_.bin",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilenameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://civitai.com/api/download/models/12345":        "12345",
		"https://example.com/path/model.safetensors?token=abc": "model.safetensors",
		"https://example.com/model.safetensors#frag":           "model.safetensors",
		"https://example.com/":                                 "download.bin",
	}
	for in, want := range cases {
		if got := filenameFromURL(in); got != want {
			t.Errorf("filenameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJobProgress(t *testing.T) {
	j := Job{Downloaded: 50, Total: 200}
	if got := j.Progress(); got != 25 {
		t.Fatalf("Progress = %v, want 25", got)
	}
	// An unknown total must not divide by zero.
	if got := (&Job{Downloaded: 10}).Progress(); got != 0 {
		t.Fatalf("Progress with no total = %v", got)
	}
}

// A download with no expected hash is accepted on arrival. That is weaker, and
// the job records what actually arrived so it can be checked later.
func TestUnverifiedDownloadStillRecordsItsHash(t *testing.T) {
	body := []byte("unverified content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m := newManager(t)
	job, err := m.Fetch(context.Background(), Job{
		URL: srv.URL + "/m.safetensors", DestDir: filepath.Join(t.TempDir(), "models"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ActualSHA != sha256Of(body) {
		t.Fatalf("ActualSHA = %s, want the hash of what arrived", job.ActualSHA)
	}
}

func TestJobsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	m := newManager(t)
	if _, err := m.Fetch(context.Background(), Job{
		URL: srv.URL + "/a.safetensors", DestDir: filepath.Join(t.TempDir(), "m"),
	}); err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs()) != 1 {
		t.Fatalf("%d jobs recorded", len(m.Jobs()))
	}
}
