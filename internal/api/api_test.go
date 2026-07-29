package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	run, _ := s.BeginScanRun("/models")
	if err := s.UpsertFileAndPath(
		store.ModelFile{SHA256: "aaa", ProbeSHA256: "p", Size: 4096, Format: "safetensors",
			WeightsSHA256: "www"},
		store.FilePath{SHA256: "aaa", Path: "/models/loras/thing.safetensors", Root: "/models",
			Device: 1, Inode: 1, Size: 4096, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservations("aaa", provenance.SourceCivitai, []store.FieldObservation{
		{Field: provenance.FieldName, Value: "Test LoRA"},
		{Field: provenance.FieldType, Value: "lora"},
		{Field: provenance.FieldBaseModel, Value: "SDXL"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel("aaa"); err != nil {
		t.Fatal(err)
	}
	return s
}

func newServer(t *testing.T, mutate func(*Config)) *Server {
	t.Helper()
	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Store:    testStore(t),
		Blobs:    blobs,
		Version:  "test",
		Security: Security{},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg)
}

func do(s *Server, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// --- §11 hardening -------------------------------------------------------------

// The DNS-rebinding defense. A browser tab on any site can resolve a hostname it
// controls to 127.0.0.1 and reach a localhost server; the connection is
// genuinely local, so only the Host header distinguishes the attack.
func TestHostAllowlistBlocksRebinding(t *testing.T) {
	s := newServer(t, nil)

	for _, host := range []string{"localhost", "localhost:8737", "127.0.0.1", "127.0.0.1:8737", "[::1]:8737"} {
		w := do(s, "GET", "http://"+host+"/api/health", "", nil)
		if w.Code != http.StatusOK {
			t.Errorf("loopback host %q rejected with %d", host, w.Code)
		}
	}

	for _, host := range []string{"evil.example.com", "attacker.test:8737", "models.internal"} {
		w := do(s, "GET", "http://"+host+"/api/health", "", nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("host %q returned %d, want 403", host, w.Code)
		}
	}
}

func TestConfiguredHostIsAllowed(t *testing.T) {
	s := newServer(t, func(c *Config) {
		c.Security.AllowedHosts = []string{"models.tailnet.ts.net"}
	})
	w := do(s, "GET", "http://models.tailnet.ts.net/api/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("configured host rejected with %d", w.Code)
	}
}

// A wildcard on an API that can read every model file lets any page on the
// internet enumerate the library, so there is deliberately no wildcard option.
func TestCORSHasNoWildcard(t *testing.T) {
	s := newServer(t, func(c *Config) {
		c.Security.AllowedOrigins = []string{"http://localhost:5173"}
	})

	w := do(s, "GET", "http://localhost/api/health", "", map[string]string{
		"Origin": "http://localhost:5173",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("allowed origin rejected: %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Allow-Origin = %q", got)
	}

	w = do(s, "GET", "http://localhost/api/health", "", map[string]string{
		"Origin": "https://evil.example.com",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed origin returned %d, want 403", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("a wildcard Allow-Origin was emitted")
	}
}

func TestTokenRequiredWhenSet(t *testing.T) {
	s := newServer(t, func(c *Config) {
		c.Security.RequireToken = true
		c.Security.Token = "secret-token"
	})

	if w := do(s, "GET", "http://localhost/api/health", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d, want 401", w.Code)
	}
	if w := do(s, "GET", "http://localhost/api/health", "", map[string]string{
		"Authorization": "Bearer wrong",
	}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token returned %d, want 401", w.Code)
	}
	if w := do(s, "GET", "http://localhost/api/health", "", map[string]string{
		"Authorization": "Bearer secret-token",
	}); w.Code != http.StatusOK {
		t.Fatalf("correct token returned %d", w.Code)
	}
	// The query form exists so a preview image can be opened in a new tab.
	if w := do(s, "GET", "http://localhost/api/health?token=secret-token", "", nil); w.Code != http.StatusOK {
		t.Fatalf("query token returned %d", w.Code)
	}
}

// A tailnet is authenticated before a packet arrives, so §11 permits exempting
// it -- but only when explicitly configured.
func TestTrustedCIDRExemptsTheToken(t *testing.T) {
	cidrs, err := ParseCIDRs(TailscaleCIDRs)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t, func(c *Config) {
		c.Security.RequireToken = true
		c.Security.Token = "secret"
		c.Security.TrustedCIDRs = cidrs
		c.Security.AllowedHosts = []string{"box.tailnet.ts.net"}
	})

	r := httptest.NewRequest("GET", "http://box.tailnet.ts.net/api/health", nil)
	r.RemoteAddr = "100.101.102.103:40000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("tailnet address returned %d, want 200", w.Code)
	}

	// An address outside the trusted range still needs the token.
	r = httptest.NewRequest("GET", "http://box.tailnet.ts.net/api/health", nil)
	r.RemoteAddr = "192.168.1.50:40000"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted address returned %d, want 401", w.Code)
	}
}

// X-Forwarded-For is attacker-controlled unless a trusted proxy set it, and this
// daemon is not behind one by design.
func TestForwardedHeaderCannotForgeATrustedAddress(t *testing.T) {
	cidrs, _ := ParseCIDRs(TailscaleCIDRs)
	s := newServer(t, func(c *Config) {
		c.Security.RequireToken = true
		c.Security.Token = "secret"
		c.Security.TrustedCIDRs = cidrs
	})

	r := httptest.NewRequest("GET", "http://localhost/api/health", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("X-Forwarded-For", "100.101.102.103")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a forged X-Forwarded-For bypassed the token: %d", w.Code)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "GET", "http://localhost/api/health", "", nil)
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// Phase 1's contract is that the index is proven before anything acts on it.
func TestReadOnlyModeRefusesMutations(t *testing.T) {
	s := newServer(t, func(c *Config) { c.ReadOnly = true })

	mutations := []struct{ method, path, body string }{
		{"PATCH", "/api/models/aaa", `{"name":"new"}`},
		{"PUT", "/api/models/aaa/tags", `["x"]`},
		{"PUT", "/api/models/aaa/training", `{"notes":"x"}`},
		{"DELETE", "/api/models/aaa/fields/name", ""},
		{"POST", "/api/suggestions/1/accept", ""},
	}
	for _, m := range mutations {
		w := do(s, m.method, "http://localhost"+m.path, m.body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s returned %d, want 403 in read-only mode", m.method, m.path, w.Code)
		}
	}
	// Reads still work.
	if w := do(s, "GET", "http://localhost/api/models", "", nil); w.Code != http.StatusOK {
		t.Errorf("read returned %d in read-only mode", w.Code)
	}
}

// --- endpoints -----------------------------------------------------------------

func TestSearchEndpoint(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "GET", "http://localhost/api/models?q=test", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var results store.SearchResults
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results.Hits) != 1 || results.Hits[0].SHA256 != "aaa" {
		t.Fatalf("hits = %+v", results.Hits)
	}
}

func TestModelDetailEndpoint(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "GET", "http://localhost/api/models/aaa", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var d ModelDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.SHA256 != "aaa" || d.Record == nil || d.Record.Name != "Test LoRA" {
		t.Fatalf("detail = %+v", d)
	}
	if d.WeightsSHA256 != "www" {
		t.Errorf("weights hash missing from the detail response")
	}
	if len(d.Paths) != 1 {
		t.Errorf("paths = %+v", d.Paths)
	}

	if w := do(s, "GET", "http://localhost/api/models/nope", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown hash returned %d, want 404", w.Code)
	}
}

func TestManualEditAndClear(t *testing.T) {
	s := newServer(t, nil)

	w := do(s, "PATCH", "http://localhost/api/models/aaa", `{"name":"Renamed By Hand"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("patch failed %d: %s", w.Code, w.Body)
	}
	var rec store.ModelRecord
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "Renamed By Hand" {
		t.Fatalf("name = %q", rec.Name)
	}

	// Clearing restores the origin value rather than emptying the field.
	w = do(s, "DELETE", "http://localhost/api/models/aaa/fields/name", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("clear failed %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "Test LoRA" {
		t.Fatalf("name after clear = %q, want the origin value back", rec.Name)
	}
}

// A null value is an explicit clear, which differs from omitting the field and
// from setting it to an empty string.
func TestNullValueClearsTheField(t *testing.T) {
	s := newServer(t, nil)

	do(s, "PATCH", "http://localhost/api/models/aaa", `{"name":"Manual"}`, nil)
	w := do(s, "PATCH", "http://localhost/api/models/aaa", `{"name":null}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("null patch failed %d: %s", w.Code, w.Body)
	}
	var rec store.ModelRecord
	_ = json.Unmarshal(w.Body.Bytes(), &rec)
	if rec.Name != "Test LoRA" {
		t.Fatalf("name = %q, want the origin value after an explicit clear", rec.Name)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "PATCH", "http://localhost/api/models/aaa", `{"not_a_field":"x"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field returned %d, want 400", w.Code)
	}
}

func TestCandidatesShowProvenance(t *testing.T) {
	s := newServer(t, nil)
	do(s, "PATCH", "http://localhost/api/models/aaa", `{"base_model":"Anima 2B"}`, nil)

	w := do(s, "GET", "http://localhost/api/models/aaa/candidates", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var views []CandidateView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}

	for _, v := range views {
		if v.Field != "base_model" {
			continue
		}
		if v.Winner.TierName != "manual" {
			t.Fatalf("winner tier = %q, want manual", v.Winner.TierName)
		}
		if len(v.Losers) == 0 {
			t.Fatal("the origin value was not reported as a loser")
		}
		return
	}
	t.Fatal("base_model was not in the candidates response")
}

func TestTrainingRecordRoundTrip(t *testing.T) {
	s := newServer(t, nil)

	body := `{"dataset":"curated-v3","dataset_size":120,"trainer":"ai-toolkit","notes":"worked well"}`
	w := do(s, "PUT", "http://localhost/api/models/aaa/training", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("put failed %d: %s", w.Code, w.Body)
	}

	w = do(s, "GET", "http://localhost/api/models/aaa/training", "", nil)
	var tr store.TrainingRecord
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Dataset != "curated-v3" || tr.DatasetSize != 120 {
		t.Fatalf("training = %+v", tr)
	}
	// Anything through this endpoint is the user typing, whatever the body said.
	if tr.Source != "manual" {
		t.Fatalf("source = %q, want manual", tr.Source)
	}
}

func TestPreviewIsServedWithSniffedType(t *testing.T) {
	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0}
	blob, err := blobs.Put(png)
	if err != nil {
		t.Fatal(err)
	}
	// A blob whose recorded type would be a lie if it were trusted.
	html, err := blobs.Put([]byte("<html><script>alert(1)</script></html>"))
	if err != nil {
		t.Fatal(err)
	}

	s := newServer(t, func(c *Config) { c.Blobs = blobs })

	w := do(s, "GET", "http://localhost/api/previews/"+blob.SHA256, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}

	w = do(s, "GET", "http://localhost/api/previews/"+html.SHA256, "", nil)
	if got := w.Header().Get("Content-Type"); strings.Contains(got, "html") {
		t.Fatalf("HTML blob served as %q; it would execute in the UI's origin", got)
	}

	if w := do(s, "GET", "http://localhost/api/previews/../../etc/passwd", "", nil); w.Code == http.StatusOK {
		t.Fatal("a traversal path was served")
	}
}

func TestOpenAPIIsValidJSON(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "GET", "http://localhost/openapi.json", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var spec map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if spec["openapi"] == nil || spec["paths"] == nil {
		t.Fatal("openapi.json is missing required top-level keys")
	}
	paths, _ := spec["paths"].(map[string]any)
	if _, ok := paths["/api/models"]; !ok {
		t.Fatal("the search endpoint is undocumented")
	}
}

// --- UI ------------------------------------------------------------------------

// A browser cannot read a token off disk, so the served page must carry it.
func TestUITokenIsInjected(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html><head><title>x</title></head><body></body></html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	s := newServer(t, func(c *Config) {
		c.UI = ui
		c.Security.RequireToken = true
		c.Security.Token = "injected-secret"
	})

	w := do(s, "GET", "http://localhost/?token=injected-secret", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "injected-secret") {
		t.Fatal("the token was not injected into the page")
	}
	if !strings.Contains(body, "window.__MM__") {
		t.Fatal("the config object was not injected")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("a page carrying a credential was served with Cache-Control %q", got)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
}

// The UI is a single-page app, so a deep link must not 404 -- but a genuinely
// missing script must, or debugging becomes maddening.
func TestUIFallbackOnlyAppliesToRoutes(t *testing.T) {
	ui := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><head></head><body>app</body></html>")},
	}
	s := newServer(t, func(c *Config) { c.UI = ui })

	if w := do(s, "GET", "http://localhost/models/abc123", "", nil); w.Code != http.StatusOK {
		t.Errorf("deep link returned %d, want the SPA shell", w.Code)
	}
	if w := do(s, "GET", "http://localhost/assets/missing.js", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing asset returned %d, want 404", w.Code)
	}
}

func TestNoUIStillServesAPI(t *testing.T) {
	s := newServer(t, nil)
	w := do(s, "GET", "http://localhost/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "openapi") {
		t.Fatal("the root response does not point at the API")
	}
}

// --- token file ----------------------------------------------------------------

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "api-token")

	token, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(token))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The token is equivalent to read access to every model file the daemon can
	// see, so the file must not be group- or world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 600", perm)
	}

	again, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Fatal("a second load generated a different token")
	}
}

func TestListenAddrLoopbackDetection(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true,
		"0.0.0.0": false, "192.168.1.5": false, "": false,
	}
	for host, want := range cases {
		if got := (ListenAddr{Host: host, Port: 1}).IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}
