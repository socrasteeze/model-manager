package api

// Destination validation is the highest-risk code in the daemon: a hole here
// turns a browse click into an arbitrary filesystem write. These tests are
// deliberately adversarial.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/store"
)

// serverWithRoot builds a server whose index knows exactly one model root.
func serverWithRoot(t *testing.T, root string, mutate func(*Config)) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "master.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.AddRoot(root, "", ""); err != nil {
		t.Fatal(err)
	}
	run, _ := st.BeginScanRun(root)
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: "aaa", ProbeSHA256: "p", Size: 4096, Format: "safetensors"},
		store.FilePath{SHA256: "aaa", Path: filepath.Join(root, "a.safetensors"),
			Root: root, Device: 1, Inode: 1, Size: 4096, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, Blobs: blobs, Version: "test"}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg)
}

func TestResolveDestinationAcceptsScannedRoot(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, nil)

	got, _, err := s.resolveDestination(root, "loras/style")
	if err != nil {
		t.Fatalf("rejected a legitimate destination: %v", err)
	}
	want := filepath.Join(root, "loras", "style")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveDestinationRejectsUnknownRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	s := serverWithRoot(t, root, nil)

	// A directory that exists and is writable, but was never scanned. Existing
	// on disk is not what makes a destination legal.
	if _, _, err := s.resolveDestination(other, ""); err == nil {
		t.Fatal("accepted a root that was never scanned")
	}
}

func TestResolveDestinationRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, nil)

	for _, subdir := range []string{
		"../escape",
		"../../etc",
		"loras/../../escape",
		"/etc",
		"..\\..\\windows",
	} {
		got, _, err := s.resolveDestination(root, subdir)
		if err != nil {
			continue // rejected outright, also fine
		}
		if !withinRoot(root, got) {
			t.Errorf("subdir %q escaped the root: %q", subdir, got)
		}
	}
}

// A subdirectory that is a symlink pointing out of the tree is the traversal
// that string-cleaning alone does not catch.
func TestResolveDestinationRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	s := serverWithRoot(t, root, nil)

	link := filepath.Join(root, "sneaky")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if got, _, err := s.resolveDestination(root, "sneaky"); err == nil {
		resolved, _ := filepath.EvalSymlinks(got)
		outsideResolved, _ := filepath.EvalSymlinks(outside)
		if strings.HasPrefix(resolved, outsideResolved) {
			t.Fatalf("symlink escape accepted: %q resolves outside the root", got)
		}
	}
}

func TestDownloadHostAllowlist(t *testing.T) {
	s := serverWithRoot(t, t.TempDir(), func(c *Config) {
		c.Origin = nil
	})

	allowed := []string{
		"civitai.com", "civitai.red", "civarchive.com",
		"huggingface.co", "cdn-lfs-us-1.huggingface.co",
	}
	for _, h := range allowed {
		if !s.downloadHostAllowed(h) {
			t.Errorf("provider host %q rejected", h)
		}
	}

	// A lookalike must not pass on a substring match.
	denied := []string{
		"evil.com", "civitai.com.attacker.net", "notciviai.com",
		"huggingface.co.evil.test", "169.254.169.254", "localhost",
	}
	for _, h := range denied {
		if s.downloadHostAllowed(h) {
			t.Errorf("host %q was allowed as a download source", h)
		}
	}
}

func TestDownloadRefusedInReadOnlyMode(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, func(c *Config) { c.ReadOnly = true })

	body := `{"url":"https://civitai.com/api/download/models/1","dest_root":"` + root + `"}`
	w := do(s, "POST", "http://localhost/api/downloads", body, nil)
	if w.Code != 403 {
		t.Fatalf("read-only server accepted a download: status %d", w.Code)
	}
}

func TestDownloadRejectsDisallowedHostAndRoot(t *testing.T) {
	root := t.TempDir()
	// Writable, but with no manager configured the endpoint must still refuse
	// rather than fall through to anything.
	s := serverWithRoot(t, root, func(c *Config) { c.ReadOnly = false })

	body := `{"url":"https://evil.test/x.safetensors","dest_root":"` + root + `"}`
	if w := do(s, "POST", "http://localhost/api/downloads", body, nil); w.Code == 202 {
		t.Error("accepted a download from a disallowed host")
	}
}

func TestDownloadRootsEndpointListsScannedRoots(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, nil)

	w := do(s, "GET", "http://localhost/api/downloads/roots", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var roots []string
	if err := json.Unmarshal(w.Body.Bytes(), &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != root {
		t.Fatalf("got %v, want [%s]", roots, root)
	}
}

func TestCleanSubdirStripsTraversal(t *testing.T) {
	cases := map[string]string{
		"loras":             "loras",
		"loras/style":       filepath.Join("loras", "style"),
		"../../etc":         "etc",
		"./loras":           "loras",
		"loras/../style":    filepath.Join("loras", "style"),
		"":                  "",
		"a/b/../../../../c": filepath.Join("a", "b", "c"),
	}
	for in, want := range cases {
		if got := cleanSubdir(in); got != want {
			t.Errorf("cleanSubdir(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bundled UI's own asset requests carry no header and no query parameter —
// the browser issues them. The cookie minted when /?token= validates is what
// lets them through, and without it the UI is a blank page in exactly the
// off-loopback deployment the token exists for.
func TestAssetsAuthenticateViaCookie(t *testing.T) {
	s := serverWithRoot(t, t.TempDir(), func(c *Config) {
		c.Security = Security{RequireToken: true, Token: "sekrit"}
		c.UI = fstest.MapFS{
			"index.html":    {Data: []byte("<html><head></head><body></body></html>")},
			"assets/app.js": {Data: []byte("console.log(1)")},
		}
	})

	// An asset with no credential is refused — that is the point of the token.
	if w := do(s, "GET", "http://localhost/assets/app.js", "", nil); w.Code != 401 {
		t.Fatalf("uncredentialed asset: status %d, want 401", w.Code)
	}

	// Opening the page with the token mints the cookie.
	w := do(s, "GET", "http://localhost/?token=sekrit", "", nil)
	if w.Code != 200 {
		t.Fatalf("page load: status %d", w.Code)
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == "mm_token" {
			cookie = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie not scoped tightly: %+v", c)
			}
		}
	}
	if cookie != "sekrit" {
		t.Fatalf("cookie = %q, want the validated token", cookie)
	}

	// A wrong query token must never be echoed into a cookie.
	bad := do(s, "GET", "http://localhost/?token=wrong", "", nil)
	for _, c := range bad.Result().Cookies() {
		if c.Name == "mm_token" {
			t.Fatal("an invalid token was persisted into a cookie")
		}
	}

	// The asset request, as a browser would send it: cookie only.
	req := httptest.NewRequest("GET", "http://localhost/assets/app.js", nil)
	req.AddCookie(&http.Cookie{Name: "mm_token", Value: cookie})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("asset with cookie: status %d, want 200", rec.Code)
	}
}
