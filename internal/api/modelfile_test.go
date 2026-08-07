package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// fileFixture is a real file on disk with a real hash, because every assertion
// in this file is about the relationship between the index and the bytes.
type fileFixture struct {
	srv  *Server
	st   *store.Store
	root string
	sha  string
	body []byte
	path string
	// rootID lets a test disable the root and watch the endpoint refuse.
	rootID int64
	// pathID lets a test mark the path absent without touching the disk.
	pathID int64
}

func serveFilesServer(t *testing.T, mutate func(*Config)) *fileFixture {
	t.Helper()

	root := testutil.TempDir(t)
	st, err := store.Open(filepath.Join(testutil.TempDir(t), "master.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	addedRoot, err := st.AddRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 8 KiB so a range request has something to range over, and non-uniform so a
	// wrong offset produces a wrong body rather than the same byte repeated.
	body := make([]byte, 8192)
	for i := range body {
		body[i] = byte(i * 7)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	path := filepath.Join(root, "model.safetensors")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := st.BeginScanRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p", Size: int64(len(body)), Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: path, Root: root, Device: 1, Inode: 1,
			Size: int64(len(body)), MtimeNs: 1, Present: true, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(run, store.StatusCompleted, store.ScanCounters{FilesSeen: 1}); err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.New(filepath.Join(testutil.TempDir(t), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, Blobs: blobs, Version: "test", ServeFiles: true}
	if mutate != nil {
		mutate(&cfg)
	}

	rows, err := st.PathsFor(sha)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one path row, got %v (%v)", rows, err)
	}

	return &fileFixture{
		srv: New(cfg), st: st, root: root, sha: sha, body: body, path: path,
		rootID: addedRoot.ID, pathID: rows[0].ID,
	}
}

func (f *fileFixture) get(t *testing.T, method string, headers map[string]string) *http.Response {
	t.Helper()
	return do(f.srv, method, "http://localhost/api/models/"+f.sha+"/file", "", headers).Result()
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestServeModelFileFullBody is the base case: the bytes that come back are the
// bytes the hash in the URL names.
func TestServeModelFileFullBody(t *testing.T) {
	f := serveFilesServer(t, nil)
	resp := f.get(t, "GET", nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := readAll(t, resp)
	if string(got) != string(f.body) {
		t.Errorf("body differs from the file on disk (%d bytes vs %d)", len(got), len(f.body))
	}

	// A strong validator that is the content hash. Set before ServeContent so
	// If-Range and If-None-Match both see it.
	if want := `"` + f.sha + `"`; resp.Header.Get("ETag") != want {
		t.Errorf("ETag = %q, want %q", resp.Header.Get("ETag"), want)
	}
	// A safetensors file opens with a JSON header, so a sniffed type would come
	// back as text/plain and some clients would render a checkpoint as a page.
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("no Accept-Ranges; resume would silently restart from zero")
	}
	// mtime is an accident of how a copy was written. Emitting it would invite a
	// client to validate on it instead of on the hash.
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		t.Errorf("Last-Modified = %q, want it omitted", lm)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "model.safetensors") {
		t.Errorf("Content-Disposition = %q, want the filename in it", cd)
	}
}

// TestServeModelFileRangeResume covers exactly what the download manager does
// when it finds a partial file: ask for the tail, and expect 206 with a
// Content-Range. A 200 here would mean a resumed pull silently re-downloads.
func TestServeModelFileRangeResume(t *testing.T) {
	f := serveFilesServer(t, nil)
	resp := f.get(t, "GET", map[string]string{"Range": "bytes=4096-"})

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	got := readAll(t, resp)
	if string(got) != string(f.body[4096:]) {
		t.Errorf("range body wrong: got %d bytes", len(got))
	}
	want := fmt.Sprintf("bytes 4096-%d/%d", len(f.body)-1, len(f.body))
	if cr := resp.Header.Get("Content-Range"); cr != want {
		t.Errorf("Content-Range = %q, want %q", cr, want)
	}
}

// TestServeModelFileUnsatisfiableRange pins the 416 branch, because the download
// manager reads the total out of its Content-Range to decide whether a partial
// is already complete. Without the total it cannot tell "done" from "corrupt".
func TestServeModelFileUnsatisfiableRange(t *testing.T) {
	f := serveFilesServer(t, nil)
	resp := f.get(t, "GET", map[string]string{
		"Range": fmt.Sprintf("bytes=%d-", len(f.body)+10),
	})

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
	if want := fmt.Sprintf("bytes */%d", len(f.body)); resp.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range = %q, want %q", resp.Header.Get("Content-Range"), want)
	}
}

func TestServeModelFileNotModified(t *testing.T) {
	f := serveFilesServer(t, nil)
	resp := f.get(t, "GET", map[string]string{"If-None-Match": `"` + f.sha + `"`})

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if body := readAll(t, resp); len(body) != 0 {
		t.Errorf("304 carried %d bytes of body", len(body))
	}
}

// A HEAD costs nothing and is how a client probes whether an upstream can serve
// files at all. Go's pattern routing matches it against the GET registration.
func TestServeModelFileHeadHasLengthWithoutBody(t *testing.T) {
	f := serveFilesServer(t, nil)
	resp := f.get(t, "HEAD", nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(f.body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(f.body))
	}
	if body := readAll(t, resp); len(body) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(body))
	}
}

// Off by default. The token is documented as equivalent to read access to every
// model file, but the default deployment is loopback with no token, where any
// local process that can open the port would otherwise get the whole library.
func TestServeModelFileDisabledByDefault(t *testing.T) {
	f := serveFilesServer(t, func(c *Config) { c.ServeFiles = false })
	resp := f.get(t, "GET", nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(readAll(t, resp)), "--serve-files") {
		t.Error("the refusal should name the flag that fixes it")
	}
}

// The whole point of the read-only mode is serving an index somebody else built.
// Serving the files that index describes is the same job, so this must NOT be
// gated the way a mutation is.
func TestServeModelFileAllowedInReadOnlyMode(t *testing.T) {
	f := serveFilesServer(t, func(c *Config) { c.ReadOnly = true })
	if resp := f.get(t, "GET", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; a read must not be refused by the read-only guard", resp.StatusCode)
	}
}

// §10.1: a probe-bound path is not proven to hold these bytes. Serving one under
// a hash the client verifies against would surface as a checksum failure at the
// far end of a multi-gigabyte transfer.
func TestServeModelFileRefusesProvisionalPath(t *testing.T) {
	f := serveFilesServer(t, nil)
	if _, err := f.st.DB().Exec(
		`UPDATE model_file_path SET provisional = 1 WHERE id = ?`, f.pathID); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(t, "GET", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a provisional path", resp.StatusCode)
	}
}

func TestServeModelFileRefusesAbsentPath(t *testing.T) {
	f := serveFilesServer(t, nil)
	if err := f.st.SetPathAbsent(f.pathID); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(t, "GET", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an absent path", resp.StatusCode)
	}
}

func TestServeModelFileUnknownHash(t *testing.T) {
	f := serveFilesServer(t, nil)
	w := do(f.srv, "GET", "http://localhost/api/models/"+strings.Repeat("f", 64)+"/file", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// RemoveRoot deliberately leaves path rows behind, and a root re-added with a
// different spelling strands them, so a recorded path can outlive the root that
// justified it. The write side refuses to write outside a managed root; this
// pins that the read side refuses to read outside one.
func TestServeModelFileRefusesPathOutsideEnabledRoots(t *testing.T) {
	f := serveFilesServer(t, nil)
	if err := f.st.SetRootEnabled(f.rootID, false); err != nil {
		t.Fatal(err)
	}
	resp := f.get(t, "GET", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 once no enabled root covers the path", resp.StatusCode)
	}
}

// The index promising N bytes while the disk holds M means the hash in the URL
// is not a promise this daemon can keep. Refuse now rather than after the client
// has pulled twelve gigabytes and failed its own checksum.
func TestServeModelFileRefusesWhenSizeDisagreesWithIndex(t *testing.T) {
	f := serveFilesServer(t, nil)
	if err := os.WriteFile(f.path, append(f.body, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := f.get(t, "GET", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestServeModelFileGoneFromDisk(t *testing.T) {
	f := serveFilesServer(t, nil)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(t, "GET", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A non-ASCII filename must survive in the RFC 5987 form and degrade to
// something readable in the plain one -- a checkpoint arriving as
// "_______.safetensors" is a real outcome worth a test.
func TestContentDispositionCarriesBothSpellings(t *testing.T) {
	got := contentDisposition("模型 v2.safetensors")
	if !strings.Contains(got, `filename="`) || !strings.Contains(got, "filename*=UTF-8''") {
		t.Fatalf("missing one of the two spellings: %q", got)
	}
	if !strings.Contains(got, ".safetensors") {
		t.Errorf("extension lost from the ASCII fallback: %q", got)
	}
	// A quote or a newline in a filename must not be able to close the header
	// value or start a new header.
	hostile := contentDisposition("a\"b\nc.safetensors")
	if strings.Contains(hostile, "\n") || strings.Count(hostile, `"`) != 2 {
		t.Errorf("filename escaped its header value: %q", hostile)
	}
	if contentDisposition("") == "" {
		t.Error("empty filename produced an empty header")
	}
}

// capabilities is what lets a client tell "this daemon will not serve files"
// from "this daemon is too old to know what that means" -- the second shows up
// as the key being absent from /api/health entirely.
func TestHealthAdvertisesServeFilesCapability(t *testing.T) {
	on := serveFilesServer(t, nil)
	if got := on.srv.capabilities(); len(got) != 1 || got[0] != CapServeFiles {
		t.Errorf("capabilities = %v, want [%s]", got, CapServeFiles)
	}

	off := serveFilesServer(t, func(c *Config) { c.ServeFiles = false })
	got := off.srv.capabilities()
	if got == nil {
		t.Fatal("capabilities must be non-nil so it serializes as [] rather than null")
	}
	if len(got) != 0 {
		t.Errorf("capabilities = %v, want empty", got)
	}

	body := string(readAll(t, do(on.srv, "GET", "http://localhost/api/health", "", nil).Result()))
	if !strings.Contains(body, CapServeFiles) {
		t.Errorf("/api/health did not advertise the capability: %s", body)
	}
}
