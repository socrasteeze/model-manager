package api

// The offline contract, checked against the whole route surface.
//
// The hub is the only machine that talks to the public internet, and it has to
// keep working when it cannot. That is a claim about roughly fifty routes, and
// until the outbound calls in this package went through one constructor it was
// not a claim anything could check: each built an anonymous client inline, so a
// request could be neither observed nor forbidden.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// offlineTransport answers every outbound request the way an unrouted network
// does, and records that the attempt was made.
//
// It returns an error rather than panicking. A panic would be the louder signal,
// but several of these paths run on goroutines the handler spawned, where a
// panic kills the test binary instead of failing a test -- and a transport error
// is what exercises the real code anyway. The recorded attempts are the stronger
// assertion regardless: a local route returning 200 only proves it did not fail,
// while an empty attempt list proves it did not ask.
type offlineTransport struct {
	mu       sync.Mutex
	attempts []string
}

func (t *offlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.attempts = append(t.attempts, r.Method+" "+r.URL.String())
	t.mu.Unlock()
	return nil, &net_OpError{op: "dial", host: r.URL.Host}
}

func (t *offlineTransport) recorded() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.attempts...)
}

// net_OpError stands in for the error a real dial failure produces, without
// depending on the exact shape net/http wraps it in.
type net_OpError struct {
	op, host string
}

func (e *net_OpError) Error() string {
	return e.op + " " + e.host + ": no route to host (offline test)"
}
func (e *net_OpError) Timeout() bool   { return false }
func (e *net_OpError) Temporary() bool { return false }

// offlineServer wires every outbound path in the daemon through one recording
// transport: this package's own clients, the origin client, the download
// manager, and ComfyUI.
func offlineServer(t *testing.T) (*Server, *offlineTransport) {
	t.Helper()
	tr := &offlineTransport{}

	root := testutil.TempDir(t)
	st, err := store.Open(root+"/master.db", store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddRoot(root, "", ""); err != nil {
		t.Fatal(err)
	}
	run, _ := st.BeginScanRun(root)
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: "aaa", ProbeSHA256: "p", Size: 4, Format: "safetensors"},
		store.FilePath{SHA256: "aaa", Path: root + "/a.safetensors", Root: root,
			Device: 1, Inode: 1, Size: 4, MtimeNs: 1, Present: true, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := download.NewManager(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	mgr.HTTP = &http.Client{Transport: tr}

	// Explicitly set, not left zero: a zero-value origin.Client now falls back
	// to a real bounded client rather than panicking, so relying on the zero
	// value to be inert would silently let requests through.
	oc := origin.NewClient()
	oc.HTTP = &http.Client{Transport: tr}
	oc.UpstreamBase = "http://hub.invalid:8737"
	// No retries and no throttle: this suite is about whether a request is made
	// at all, and the production backoff curve would spend a minute proving it.
	oc.MaxRetries = 0
	oc.MinInterval = 0

	s := New(Config{
		Store: st, Blobs: blobs, Version: "test",
		Downloads: mgr, Origin: oc, Transport: tr,
		Renders:    comfy.NewManager(nil, nil),
		ServeFiles: true, AllowEvict: true,
	})
	// api.New replaces Dial, so the ComfyUI client is redirected afterwards.
	s.cfg.Renders.Dial = func() (*comfy.Client, error) {
		c, err := comfy.NewClient("http://comfy.invalid:8188")
		if err != nil {
			return nil, err
		}
		c.HTTP = &http.Client{Transport: tr}
		return c, nil
	}
	return s, tr
}

// requestFor turns a route pattern into something callable.
func requestFor(pattern string) (method, target string) {
	method, path, _ := strings.Cut(pattern, " ")
	path = strings.ReplaceAll(path, "{sha}", "aaa")
	path = strings.ReplaceAll(path, "{id}", "1")
	path = strings.ReplaceAll(path, "{image}", "deadbeef")
	path = strings.ReplaceAll(path, "{field}", "name")
	path = strings.ReplaceAll(path, "{key}", "library.filters")
	return method, "http://localhost" + path
}

// TestLocalRoutesMakeNoOutboundRequests is the offline contract.
//
// Non-GET routes are exercised with an empty body and any status is acceptable:
// the assertion that matters is that nothing was dialled, and a 400 from an
// unparseable body proves that just as well as a 200 would. That sidesteps
// constructing a valid body for every mutating route, which would be a second
// copy of each handler's contract kept in a test.
func TestLocalRoutesMakeNoOutboundRequests(t *testing.T) {
	s, tr := offlineServer(t)

	for pattern, r := range s.reachOf {
		if r != reachLocal {
			continue
		}
		method, target := requestFor(pattern)
		before := len(tr.recorded())
		do(s, method, target, "", nil)
		if got := tr.recorded(); len(got) != before {
			t.Errorf("%s reached the network: %v", pattern, got[before:])
		}
	}
}

// The other half of the contract: a route that does go out must fail in a way
// that names itself, rather than hanging, panicking, or returning something that
// reads as an empty library.
func TestOutboundRoutesFailByName(t *testing.T) {
	s, _ := offlineServer(t)

	for pattern, r := range s.reachOf {
		if r != reachOutbound {
			continue
		}
		method, target := requestFor(pattern)
		w := do(s, method, target, "", nil)

		// Either a non-2xx, or a 200 whose body carries the reason. Both are
		// acceptable -- the settings panels deliberately answer 200 with a named
		// field, because "configured but unreachable" is four separate facts and
		// a 502 would collapse them. A 200 with nothing to explain it is not.
		if w.Code >= 200 && w.Code < 300 {
			body := strings.ToLower(w.Body.String())
			named := strings.Contains(body, "error") ||
				strings.Contains(body, "reachable") ||
				strings.Contains(body, "configured") ||
				strings.Contains(body, "could not") ||
				strings.Contains(body, "no route")
			if !named {
				t.Errorf("%s returned %d offline with no named failure: %s",
					pattern, w.Code, w.Body.String())
			}
		}
	}
}

// Every route is classified. Without this, adding a route and forgetting the
// argument would silently shrink the surface the contract covers -- and register
// is the only way to add one, so the count is the check.
func TestEveryRouteIsClassified(t *testing.T) {
	s, _ := offlineServer(t)

	if len(s.reachOf) < 40 {
		t.Fatalf("only %d routes classified; register() is being bypassed", len(s.reachOf))
	}

	// The outbound set is small and deliberate. If this number moves, a route
	// started talking to a third party and that should be a decision, not a
	// diff nobody looked at.
	var outbound []string
	for pattern, r := range s.reachOf {
		if r == reachOutbound {
			outbound = append(outbound, pattern)
		}
	}
	want := map[string]bool{
		"GET /api/browse":               true,
		"GET /api/remote-image":         true,
		"GET /api/upstream":             true,
		"GET /api/comfy":                true,
		"POST /api/models/{sha}/enrich": true,
	}
	for _, p := range outbound {
		if !want[p] {
			t.Errorf("%s is newly outbound; the offline contract has to account for it", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("%s is no longer outbound; if that is right, drop it from this list", p)
	}

	// Deferred is the same kind of decision and gets the same pin. These four
	// return before they dial, which is why they are neither local nor outbound.
	deferred := map[string]bool{}
	for pattern, r := range s.reachOf {
		if r == reachDeferred {
			deferred[pattern] = true
		}
	}
	for _, p := range []string{
		"POST /api/enrich", "POST /api/updates", "POST /api/downloads",
		"POST /api/models/{sha}/previews/render", "POST /api/archive/pull",
	} {
		if !deferred[p] {
			t.Errorf("%s is no longer deferred; if it now dials inline it is outbound", p)
		}
		delete(deferred, p)
	}
	for p := range deferred {
		t.Errorf("%s newly spawns outbound work; say so deliberately", p)
	}
}

// A sanity check on the harness itself: the transport must actually record, or
// every test above passes for the wrong reason.
func TestOfflineTransportRecords(t *testing.T) {
	tr := &offlineTransport{}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	if _, err := (&http.Client{Transport: tr}).Get(srv.URL); err == nil {
		t.Fatal("the offline transport allowed a request through")
	}
	if len(tr.recorded()) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(tr.recorded()))
	}
}
