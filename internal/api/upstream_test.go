package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// clientFor builds a daemon pointed at another daemon, so these tests exercise
// both halves of the wire rather than a hand-written fixture of one of them.
// The "NAS" here is a real api.Server; only the transport is a test one.
func clientFor(t *testing.T, upstream *Server, token string, mutate func(*Config)) *Server {
	t.Helper()
	nas := httptest.NewServer(upstream)
	t.Cleanup(nas.Close)

	return newServer(t, func(c *Config) {
		c.Origin = &origin.Client{UpstreamBase: nas.URL, UpstreamToken: token}
		if mutate != nil {
			mutate(c)
		}
	})
}

func statusOf(t *testing.T, s *Server) UpstreamStatus {
	t.Helper()
	w := do(s, "GET", "http://localhost/api/upstream", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d", w.Code)
	}
	var got UpstreamStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func withDownloads(t *testing.T) func(*Config) {
	t.Helper()
	return func(c *Config) {
		mgr, err := download.NewManager(filepath.Join(testutil.TempDir(t), "downloads"))
		if err != nil {
			t.Fatal(err)
		}
		c.Downloads = mgr
	}
}

func TestUpstreamStatusUnconfigured(t *testing.T) {
	s := newServer(t, nil)
	got := statusOf(t, s)

	if got.Configured || got.Reachable || got.CanPull {
		t.Fatalf("unconfigured daemon reported %+v", got)
	}
	if !strings.Contains(got.Error, "MM_UPSTREAM_URL") {
		t.Errorf("the error should name the variable to set: %q", got.Error)
	}
	// The three states must stay distinguishable even before anything is set.
	if got.Files != UpstreamFilesUnknown {
		t.Errorf("files = %q, want %q", got.Files, UpstreamFilesUnknown)
	}
}

// The happy path, end to end: a NAS serving files, a client able to write.
func TestUpstreamStatusReadyToPull(t *testing.T) {
	nas := serveFilesServer(t, nil)
	s := clientFor(t, nas.srv, "", withDownloads(t))

	got := statusOf(t, s)
	if !got.Configured || !got.Reachable || !got.Authenticated {
		t.Fatalf("status = %+v", got)
	}
	if got.Files != UpstreamFilesYes {
		t.Errorf("files = %q, want yes", got.Files)
	}
	if !got.CanPull {
		t.Errorf("can_pull = false with everything in place: %+v", got)
	}
	if got.Error != "" {
		t.Errorf("error set on a working upstream: %q", got.Error)
	}
	if got.Version != "test" {
		t.Errorf("version = %q, want the upstream's", got.Version)
	}
}

// Reachable, authenticated, new enough -- but the operator has not opted in.
// That is a different fix from every other failure here, so it gets its own
// message naming the flag.
func TestUpstreamStatusFileServingOff(t *testing.T) {
	nas := serveFilesServer(t, func(c *Config) { c.ServeFiles = false })
	s := clientFor(t, nas.srv, "", withDownloads(t))

	got := statusOf(t, s)
	if !got.Reachable || !got.Authenticated {
		t.Fatalf("status = %+v", got)
	}
	if got.Files != UpstreamFilesNo {
		t.Fatalf("files = %q, want no", got.Files)
	}
	if got.CanPull {
		t.Error("can_pull with an upstream that will not serve files")
	}
	if !strings.Contains(got.Error, "--serve-files") {
		t.Errorf("the error should name the flag: %q", got.Error)
	}
}

// The local half of the question. An upstream that will hand over files is no
// use on a daemon that has been told not to write any, and blaming the NAS for
// that would send the operator to the wrong machine.
func TestUpstreamStatusBlockedByLocalReadOnly(t *testing.T) {
	nas := serveFilesServer(t, nil)
	s := clientFor(t, nas.srv, "", func(c *Config) { c.ReadOnly = true })

	got := statusOf(t, s)
	if got.Files != UpstreamFilesYes {
		t.Fatalf("files = %q, want yes: the upstream is fine", got.Files)
	}
	if got.CanPull {
		t.Error("can_pull on a read-only daemon")
	}
	if !strings.Contains(got.Error, "--writable") {
		t.Errorf("the error should point at this daemon, not the upstream: %q", got.Error)
	}
}

func TestUpstreamStatusUnreachable(t *testing.T) {
	// A real address with nothing behind it: started so the port is genuine,
	// closed so the connection is refused at once. A blackholed address would
	// test the timeout instead, and would cost five seconds to do it.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	s := newServer(t, func(c *Config) {
		c.Origin = &origin.Client{UpstreamBase: deadURL}
	})
	got := statusOf(t, s)

	if !got.Configured {
		t.Error("configured should be true even when unreachable; the two are different failures")
	}
	if got.Reachable || got.CanPull {
		t.Errorf("status = %+v", got)
	}
	if !strings.Contains(got.Error, "could not reach") {
		t.Errorf("error = %q", got.Error)
	}
}

// A wrong token must not read as a network problem: one sends the operator to
// their firewall, the other to their api-token file.
func TestUpstreamStatusDistinguishesBadCredentialsFromUnreachable(t *testing.T) {
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer nas.Close()

	s := newServer(t, func(c *Config) {
		c.Origin = &origin.Client{UpstreamBase: nas.URL, UpstreamToken: "wrong"}
	})
	got := statusOf(t, s)

	if !got.Reachable {
		t.Error("a 401 means we reached it; reachable must stay true")
	}
	if got.Authenticated {
		t.Error("authenticated after a 401")
	}
	if !strings.Contains(got.Error, "MM_UPSTREAM_TOKEN") {
		t.Errorf("the error should name the credential: %q", got.Error)
	}
}

// An upstream predating the capability list has to be probed. The discriminator
// is the content type, because a daemon without the route falls through to the
// SPA handler and answers deep links with HTML.
func TestUpstreamStatusProbesDaemonWithoutCapabilities(t *testing.T) {
	// An older daemon: health with no capabilities key, and no file route, so
	// the SPA fallback answers.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok","version":"0.3.0","schema_version":6}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><title>model-manager</title>"))
	}))
	defer old.Close()

	s := newServer(t, func(c *Config) { c.Origin = &origin.Client{UpstreamBase: old.URL} })
	got := statusOf(t, s)

	if got.Files != UpstreamFilesUnknown {
		t.Fatalf("files = %q, want unknown for a daemon that predates the feature", got.Files)
	}
	if got.CanPull {
		t.Error("can_pull against an upstream that cannot serve files")
	}
	if !strings.Contains(got.Error, "upgrade") {
		t.Errorf("the error should say what to do about it: %q", got.Error)
	}
	if got.Version != "0.3.0" {
		t.Errorf("version = %q", got.Version)
	}
}

// The response is rendered in a settings panel and may be copied into a bug
// report, so it must never carry the credential or any upstream filesystem path.
func TestUpstreamStatusLeaksNoSecrets(t *testing.T) {
	nas := serveFilesServer(t, nil)
	s := clientFor(t, nas.srv, "super-secret-token", withDownloads(t))

	body := do(s, "GET", "http://localhost/api/upstream", "", nil).Body.String()
	if strings.Contains(body, "super-secret-token") {
		t.Fatalf("token leaked into the status response: %s", body)
	}
	if strings.Contains(body, nas.root) || strings.Contains(body, nas.path) {
		t.Fatalf("an upstream path leaked into the status response: %s", body)
	}

	// The host is deliberately present: the operator typed it and needs to
	// confirm they typed the right one.
	var got UpstreamStatus
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Host == "" {
		t.Error("host omitted; the operator has no way to confirm the address")
	}
}

// A nil Origin must not panic the status endpoint, since that is the shape every
// daemon started with --no-remote and no upstream has.
func TestUpstreamStatusSurvivesNilOrigin(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = nil })
	if got := s.upstreamStatus(context.Background()); got.Configured {
		t.Errorf("status = %+v", got)
	}
}
