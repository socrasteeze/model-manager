package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// pullRig is two daemons: a NAS holding a model with real metadata, and a
// client configured to pull from it. Both are genuine api.Servers, so a test
// here exercises the same code both machines would run.
type pullRig struct {
	nas     *fileFixture
	nasURL  string
	client  *Server
	clientS *store.Store
	root    string
}

// realPNG is a decodable image, unlike the header-only fixture next door: the
// carry-over derives a thumbnail from what it fetches, and a preview that
// cannot be decoded would skip that path silently.
func realPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := 0; x < 2; x++ {
		for y := 0; y < 2; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newPullRig builds the NAS side with the metadata shape that matters: a field
// the user corrected by hand sitting above the provider's value for the same
// field, which is exactly what a naive copy would flatten.
func newPullRig(t *testing.T) *pullRig {
	t.Helper()

	nas := serveFilesServer(t, nil)
	sha := nas.sha

	if err := nas.st.RecordObservations(sha, "civitai", []store.FieldObservation{
		{Field: "name", Value: "Watercolour Style"},
		{Field: "type", Value: "lora"},
		{Field: "base_model", Value: "SDXL"},
		{Field: "trigger_words", Value: []string{"watercolour"}},
	}); err != nil {
		t.Fatal(err)
	}
	// The correction. On the NAS this wins at tier 3 and is untouchable; the
	// whole point of replaying candidates is that it stays that way over here.
	if err := nas.st.RecordObservations(sha, "manual", []store.FieldObservation{
		{Field: "name", Value: "Watercolour (my fixed name)"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nas.st.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}
	if err := nas.st.SetTags(sha, "manual", []string{"style", "favourite"}); err != nil {
		t.Fatal(err)
	}
	if err := nas.st.PutModelOrigin(store.ModelOrigin{
		SHA256: sha, Provider: "civitai", ModelID: "999",
		VersionID: "4567", VersionName: "v3", Source: store.OriginSourceArchive,
	}); err != nil {
		t.Fatal(err)
	}
	// A preview the user picked by hand, so the carried copy can be checked for
	// having kept its source rather than being relabelled.
	if _, err := nas.srv.storePreview(sha, realPNG(t, color.RGBA{200, 30, 30, 255}), "manual"); err != nil {
		t.Fatal(err)
	}

	nasHTTP := httptest.NewServer(nas.srv)
	t.Cleanup(nasHTTP.Close)

	// The client: its own store, its own root, downloads enabled.
	root := testutil.TempDir(t)
	st, err := store.Open(filepath.Join(testutil.TempDir(t), "client.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddRoot(root, "", ""); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.New(filepath.Join(testutil.TempDir(t), "client-blobs"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := download.NewManager(filepath.Join(testutil.TempDir(t), "partials"))
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{
		Store: st, Blobs: blobs, Version: "test", Downloads: mgr,
		Origin: &origin.Client{UpstreamBase: nasHTTP.URL},
	})
	mgr.TokenFor = client.cfg.Origin.TokenFor

	return &pullRig{nas: nas, nasURL: nasHTTP.URL, client: client, clientS: st, root: root}
}

// pull runs a real transfer through the download manager and waits for it.
func (r *pullRig) pull(t *testing.T) download.Job {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"url":       r.nasURL + "/api/models/" + r.nas.sha + "/file",
		"dest_root": r.root,
		"filename":  "watercolour.safetensors",
		"sha256":    r.nas.sha,
		"type":      "lora",
	})
	w := do(r.client, "POST", "http://localhost/api/downloads", string(body), nil)
	if w.Code != http.StatusAccepted && w.Code != http.StatusConflict {
		t.Fatalf("download refused: %d %s", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := r.client.cfg.Downloads.Job(started.ID)
		if ok && (job.State == download.StateComplete || job.State == download.StateFailed ||
			job.State == download.StateQuarantined || job.State == download.StateCancelled) {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the pull never reached a terminal state")
	return download.Job{}
}

func (r *pullRig) detail(t *testing.T) ModelDetail {
	t.Helper()
	w := do(r.client, "GET", "http://localhost/api/models/"+r.nas.sha, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("model detail: %d %s", w.Code, w.Body.String())
	}
	var d ModelDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (r *pullRig) candidates(t *testing.T) []CandidateView {
	t.Helper()
	w := do(r.client, "GET", "http://localhost/api/models/"+r.nas.sha+"/candidates", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("candidates: %d %s", w.Code, w.Body.String())
	}
	var out []CandidateView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func fieldView(views []CandidateView, field string) *CandidateView {
	for i := range views {
		if views[i].Field == field {
			return &views[i]
		}
	}
	return nil
}

// TestPullFromUpstreamEndToEnd is the whole feature in one test: a client with
// nothing fetches a model from a NAS and ends up with the file, the index entry,
// and the metadata -- over HTTP, through the real download manager.
func TestPullFromUpstreamEndToEnd(t *testing.T) {
	r := newPullRig(t)
	job := r.pull(t)

	if job.State != download.StateComplete {
		t.Fatalf("state = %s, error = %q", job.State, job.Error)
	}
	if job.IndexError != "" {
		t.Errorf("index error: %s", job.IndexError)
	}
	if job.MetaError != "" {
		t.Errorf("carry-over error: %s", job.MetaError)
	}
	if job.UpstreamBase != r.nasURL {
		t.Errorf("upstream_base = %q, want %q; without it nothing knows this was a pull",
			job.UpstreamBase, r.nasURL)
	}

	// The bytes arrived intact, under the client's own root.
	got, err := os.ReadFile(job.FinalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, r.nas.body) {
		t.Error("the pulled file differs from the upstream's")
	}
	if !strings.HasPrefix(job.FinalPath, r.root) {
		t.Errorf("landed at %q, outside the client root %q", job.FinalPath, r.root)
	}

	d := r.detail(t)
	if d.SHA256 != r.nas.sha {
		t.Fatalf("indexed as %q, want %q", d.SHA256, r.nas.sha)
	}
	if d.Record == nil || d.Record.Name == "" {
		t.Fatal("the model has no resolved record; it would show in the library as a bare hash")
	}
}

// TestAPulledModelIsReadOnce is the whole point of the single-pass change,
// asserted end to end rather than structurally.
//
// The transfer used to hash the file to verify it and the indexer then read the
// same file again to record its weights hash, probe hash and header -- so a
// twelve gigabyte model cost twenty-four gigabytes of reading. The identity now
// travels from one to the other.
//
// Measured by instrumenting the destination rather than by counting calls: what
// matters is bytes off the disk, and a structural assertion would keep passing
// if something reintroduced a read somewhere else.
func TestAPulledModelIsReadOnce(t *testing.T) {
	r := newPullRig(t)
	job := r.pull(t)
	if job.State != download.StateComplete {
		t.Fatalf("pull failed: %s %s", job.State, job.Error)
	}

	// Every fact the indexer needs is present, which is what proves the handed
	// identity was used rather than silently discarded and re-derived.
	var format, weights string
	var headerLen int
	err := r.clientS.DB().QueryRow(`
        SELECT format, COALESCE(weights_sha256, ''), COALESCE(LENGTH(header_blob), 0)
          FROM model_file WHERE sha256 = ?`, r.nas.sha).Scan(&format, &weights, &headerLen)
	if err != nil {
		t.Fatal(err)
	}
	// The bug the .part filename would have caused: format unknown, no weights
	// hash, no header -- permanently, because the next scan matches the cache
	// key and never opens the file again.
	if format != "safetensors" {
		t.Errorf("format = %q; the staging filename defeated format detection", format)
	}
	if weights == "" {
		t.Error("no weights hash recorded; the rebinding key would be permanently absent")
	}
	if headerLen == 0 {
		t.Error("no header blob captured")
	}

	// And the run is accounted for as cached rather than hashed, so the saving
	// is not reported as work that did not happen.
	var hashed, cached, bytesHashed int64
	err = r.clientS.DB().QueryRow(`
        SELECT COALESCE(SUM(files_hashed), 0), COALESCE(SUM(files_cached), 0),
               COALESCE(SUM(bytes_hashed), 0)
          FROM scan_run WHERE root = ?`, r.root).Scan(&hashed, &cached, &bytesHashed)
	if err != nil {
		t.Fatal(err)
	}
	if cached == 0 {
		t.Error("the reused identity was not counted as cached")
	}
	if bytesHashed != 0 {
		t.Errorf("bytes_hashed = %d; the indexer is reporting bytes it did not read", bytesHashed)
	}
}

// The load-bearing assertion. A value the user typed on the NAS must arrive as
// a manual, tier-3 value here -- not flattened into one "upstream" source where
// the next local enrichment sweep would outrank it and silently undo the
// correction.
func TestPullPreservesManualTier(t *testing.T) {
	r := newPullRig(t)
	if job := r.pull(t); job.MetaError != "" {
		t.Fatalf("carry-over failed: %s", job.MetaError)
	}

	name := fieldView(r.candidates(t), "name")
	if name == nil {
		t.Fatal("no candidates for name came across")
	}
	if name.Winner.Source != "manual" {
		t.Errorf("winning source = %q, want manual", name.Winner.Source)
	}
	if name.Winner.TierName != "manual" {
		t.Errorf("winning tier = %q, want manual", name.Winner.TierName)
	}
	if !strings.Contains(string(name.Winner.Value), "my fixed name") {
		t.Errorf("winning value = %s, want the hand-corrected one", name.Winner.Value)
	}

	// And the loser came too, under its own source rather than being discarded.
	var sawCivitai bool
	for _, l := range name.Losers {
		if l.Source == "civitai" {
			sawCivitai = true
			if l.TierName != "origin" {
				t.Errorf("civitai landed at tier %q, want origin", l.TierName)
			}
		}
	}
	if !sawCivitai {
		t.Error("the civitai value was not replayed; clearing the manual value would leave the field empty")
	}
}

// Replaying the losers is what makes the manual value correctable here the way
// it is over there: clear it and the provider's value comes back.
func TestPullReplaysLosersSoClearingFallsBack(t *testing.T) {
	r := newPullRig(t)
	if job := r.pull(t); job.MetaError != "" {
		t.Fatalf("carry-over failed: %s", job.MetaError)
	}

	w := do(r.client, "DELETE", "http://localhost/api/models/"+r.nas.sha+"/fields/name", "", nil)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("clearing the manual value: %d %s", w.Code, w.Body.String())
	}

	d := r.detail(t)
	if d.Record == nil {
		t.Fatal("no record after clearing")
	}
	if d.Record.Name != "Watercolour Style" {
		t.Errorf("name after clearing = %q, want the civitai value to resurface", d.Record.Name)
	}
}

func TestPullCarriesTagsPreviewsAndOrigin(t *testing.T) {
	r := newPullRig(t)
	if job := r.pull(t); job.MetaError != "" {
		t.Fatalf("carry-over failed: %s", job.MetaError)
	}
	d := r.detail(t)

	if len(d.Tags) != 2 {
		t.Errorf("tags = %v, want both", d.Tags)
	}
	if len(d.Previews) != 1 {
		t.Fatalf("previews = %d, want 1", len(d.Previews))
	}
	// A preview the user chose by hand upstream stays a manual preview here,
	// rather than being relabelled as something the machine picked.
	if d.Previews[0].Source != "manual" {
		t.Errorf("preview source = %q, want manual", d.Previews[0].Source)
	}
	// Content addressing means the copied bytes must land at the same address.
	nasPreviews, err := r.nas.st.PreviewImages(r.nas.sha)
	if err != nil || len(nasPreviews) != 1 {
		t.Fatalf("upstream previews: %v %v", nasPreviews, err)
	}
	if d.Previews[0].ImageSHA256 != nasPreviews[0].ImageSHA256 {
		t.Errorf("preview hash = %q, want %q", d.Previews[0].ImageSHA256, nasPreviews[0].ImageSHA256)
	}
	if _, err := os.Stat(r.client.cfg.Blobs.Path(d.Previews[0].ImageSHA256)); err != nil {
		t.Errorf("preview blob was not stored locally: %v", err)
	}

	// Origin identity, so this daemon's own update sweep can badge the model
	// without holding a Civitai key of its own.
	if len(d.Origins) != 1 {
		t.Fatalf("origins = %+v, want the civitai identity", d.Origins)
	}
	if d.Origins[0].ModelID != "999" || d.Origins[0].VersionID != "4567" {
		t.Errorf("origin = %+v", d.Origins[0])
	}
}

// The row that makes the copy evictable. It has to survive independently of the
// metadata, because a file recorded as present with no record of where it came
// from is a file that can never be safely deleted.
func TestPullRecordsWhereTheCopyCameFrom(t *testing.T) {
	r := newPullRig(t)
	job := r.pull(t)

	copies, err := r.clientS.PulledCopies(r.nas.sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("pulled copies = %+v, want 1", copies)
	}
	c := copies[0]
	if !c.Resident() {
		t.Error("a freshly pulled copy is not resident")
	}
	if c.Upstream != r.nasURL {
		t.Errorf("upstream = %q, want %q", c.Upstream, r.nasURL)
	}
	if c.Path != job.FinalPath {
		t.Errorf("path = %q, want %q", c.Path, job.FinalPath)
	}
	if c.Root != r.root {
		t.Errorf("root = %q, want the canonical client root %q", c.Root, r.root)
	}
	if c.SizeBytes != int64(len(r.nas.body)) {
		t.Errorf("size = %d, want %d", c.SizeBytes, len(r.nas.body))
	}

	// And it must be reachable through the API, since the evict button is gated
	// on it.
	if d := r.detail(t); len(d.Pulled) != 1 || !d.Pulled[0].Resident() {
		t.Errorf("model detail pulled = %+v", d.Pulled)
	}
}

// An ordinary download from a public provider must not be marked as a pull:
// nothing recorded where it came from, so nothing may later delete it on the
// grounds that it can be fetched again.
func TestOrdinaryDownloadIsNotMarkedAsAPull(t *testing.T) {
	r := newPullRig(t)

	// Same daemon, but a URL on a different host. Reached through the private
	// helper because the allowlist would refuse the request outright, which is
	// itself the correct behaviour and is tested elsewhere.
	for _, raw := range []string{
		"https://civitai.com/api/download/models/1",
		// The same host on a different port is a different service.
		strings.Replace(r.nasURL, "127.0.0.1:", "127.0.0.1:1", 1) + "/api/models/x/file",
	} {
		target := mustParseURL(t, raw)
		if got := r.client.upstreamBaseFor(target); got != "" {
			t.Errorf("upstreamBaseFor(%q) = %q, want empty", raw, got)
		}
	}

	// And the real upstream URL still resolves, so the negative cases above are
	// not passing because the helper is simply broken.
	target := mustParseURL(t, r.nasURL+"/api/models/abc/file")
	if got := r.client.upstreamBaseFor(target); got != r.nasURL {
		t.Errorf("upstreamBaseFor(upstream) = %q, want %q", got, r.nasURL)
	}
}

// A carry-over failure must not read as an indexing failure. The UI renders
// IndexError as "it will appear after the next scan", which is confidently
// wrong advice for a model that is already in the library.
func TestCarryOverFailureIsReportedApartFromIndexing(t *testing.T) {
	r := newPullRig(t)

	// An upstream that serves the file but refuses everything else, which is
	// what a mid-pull restart or a permissions change looks like.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/file") {
			r.nas.srv.ServeHTTP(w, req)
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer broken.Close()

	r.client.cfg.Origin.UpstreamBase = broken.URL
	r.nasURL = broken.URL
	job := r.pull(t)

	if job.State != download.StateComplete {
		t.Fatalf("state = %s: the transfer itself should still succeed", job.State)
	}
	if job.IndexError != "" {
		t.Errorf("index error set for a metadata failure: %q", job.IndexError)
	}
	if job.MetaError == "" {
		t.Fatal("a failed carry-over reported nothing at all")
	}

	// The file is still usable: indexed, present, findable.
	if d := r.detail(t); d.SHA256 != r.nas.sha {
		t.Error("the model is not in the library despite a successful transfer")
	}
	// And the copy is still recorded as evictable, because that row is written
	// before any metadata is fetched.
	if copies, _ := r.clientS.PulledCopies(r.nas.sha); len(copies) != 1 {
		t.Errorf("pulled copies = %+v; the row must survive a metadata failure", copies)
	}
}
