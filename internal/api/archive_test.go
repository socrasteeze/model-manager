package api

import (
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/archivejob"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/jobrun"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// fakeProvider stands in for Civitai: a model, a version, a gallery and images.
type fakeProvider struct {
	modelGone   bool
	versionGone bool
	galleryFail bool
	// videoPreview makes one image un-storable, which is the ordinary partial.
	videoPreview bool
	hits         map[string]int
}

func (f *fakeProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.hits == nil {
			f.hits = map[string]int{}
		}
		p := r.URL.Path
		f.hits[p]++
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasPrefix(p, "/models/"):
			if f.modelGone {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id":999,"name":"Watercolour","type":"LORA","nsfw":false,
              "modelVersions":[{"id":4567,"name":"v3","baseModel":"SDXL",
                "files":[{"name":"w.safetensors","sizeKB":4,"primary":true,
                  "hashes":{"SHA256":"`+strings.Repeat("A", 64)+`"},
                  "downloadUrl":"`+providerBase(r)+`/download/1"}]}]}`)

		case strings.HasPrefix(p, "/model-versions/"):
			if f.versionGone {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id":4567,"modelId":999,"name":"v3","baseModel":"SDXL",
              "trainedWords":["watercolour"],
              "model":{"name":"Watercolour","type":"LORA","nsfw":false},
              "files":[{"name":"w.safetensors","sizeKB":4,"primary":true,
                "hashes":{"SHA256":"`+strings.Repeat("A", 64)+`"}}],
              "images":[{"url":"`+providerBase(r)+`/img/body.png"}]}`)

		case p == "/images":
			if f.galleryFail {
				http.Error(w, "nope", http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `{"items":[
              {"id":11,"url":"`+providerBase(r)+`/img/gallery1.png"},
              {"id":12,"url":"`+providerBase(r)+`/img/gallery2.png"}],
              "metadata":{}}`)

		case strings.HasPrefix(p, "/img/"):
			if f.videoPreview && strings.Contains(p, "gallery2") {
				// Not an image. The MIME sniff refuses it regardless of size,
				// which is what an animated preview looks like from here.
				w.Header().Set("Content-Type", "video/mp4")
				w.Write([]byte("\x00\x00\x00\x20ftypmp42not-an-image"))
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(realPNG(t, colorFor(p)))

		default:
			http.NotFound(w, r)
		}
	}
}

func providerBase(r *http.Request) string { return "http://" + r.Host }

// archiveRig is a hub with archive intake enabled, pointed at a fake provider.
type archiveRig struct {
	srv      *Server
	st       *store.Store
	provider *fakeProvider
	base     string
	root     string
}

func newArchiveRig(t *testing.T, p *fakeProvider, mutate func(*Config)) *archiveRig {
	t.Helper()
	prov := httptest.NewServer(p.handler(t))
	t.Cleanup(prov.Close)

	root := testutil.TempDir(t)
	st, err := store.Open(filepath.Join(testutil.TempDir(t), "master.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddRoot(root, "", ""); err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	client := origin.NewClient()
	client.CivitaiBase = prov.URL
	client.MinInterval = 0
	client.MaxRetries = 0

	mgr, err := download.NewManager(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	archives := archivejob.New(st, blobs, func() *origin.Client { return client })

	cfg := Config{
		Store: st, Blobs: blobs, Version: "test",
		Origin: client, Downloads: mgr, Archives: archives,
		Jobs: &jobrun.Group{}, AllowArchive: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &archiveRig{srv: New(cfg), st: st, provider: p, base: prov.URL, root: root}
}

func (r *archiveRig) pull(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	if body == "" {
		body = `{"provider":"civitai","model_id":"999","dest_root":` + mustJSON(t, r.root) + `}`
	}
	return do(r.srv, "POST", "http://localhost/api/archive/pull", body, nil)
}

func (r *archiveRig) waitForItem(t *testing.T) *store.ArchiveItem {
	t.Helper()
	for i := 0; i < 200; i++ {
		if job, ok := r.srv.cfg.Archives.Current(); ok && job.State != archivejob.StateRunning {
			break
		}
		testutilSleep()
	}
	item, err := r.st.ArchiveItemFor("civitai", "999", "4567")
	if err != nil {
		t.Fatal(err)
	}
	if item == nil {
		t.Fatal("no archive item was recorded")
	}
	return item
}

// The whole point: the raw provider responses are kept, so a takedown tomorrow
// costs nothing.
func TestArchivePullCapturesRawResponses(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, nil)
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d: %s", w.Code, w.Body.String())
	}
	item := r.waitForItem(t)

	if !item.OriginCacheOK || !item.MetaOK {
		t.Errorf("metadata not captured: %+v", item)
	}

	// Both bodies, under their own key spaces.
	cache := origin.NewCache(r.st)
	for _, k := range []struct{ provider, key string }{
		{origin.ProviderCivitaiVersionID, "4567"},
		{origin.ProviderCivitaiModelID, "999"},
	} {
		entry, ok, err := cache.Get(k.provider, k.key)
		if err != nil || !ok || entry == nil || len(entry.Raw) == 0 {
			t.Errorf("no archived body for %s/%s (%v)", k.provider, k.key, err)
		}
	}
}

// Capturing metadata is not the same as applying it, and the gap between them
// is a whole transfer long.
//
// field_value and model_tag key on model_file, so nothing can be written until
// the download lands -- which is why the archived body is read back out of the
// cache in the completion hook rather than carried in memory. Without that step
// an archived model appears in the library as a bare hash with a filename:
// everything fetched, nothing showing.
func TestArchivedMetadataIsAppliedWhenTheFileLands(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, nil)
	sha := strings.ToLower(strings.Repeat("a", 64))

	// The file, as the download completion hook would find it.
	run, _ := r.st.BeginScanRun(r.root)
	if err := r.st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p", Size: 4096, Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: filepath.Join(r.root, "w.safetensors"), Root: r.root,
			Device: 1, Inode: 1, Size: 4096, MtimeNs: 1, Present: true, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d", w.Code)
	}
	r.waitForItem(t)

	// Before the hook runs there is no resolved record, which is the state the
	// bug left every archived model in.
	if msg := r.srv.afterArchiveDownload(download.Job{
		ArchiveKey: archiveKey("civitai", "999", "4567"), ActualSHA: sha,
	}); msg != "" {
		t.Fatalf("completion hook reported: %s", msg)
	}

	detail, err := r.srv.modelDetail(sha)
	if err != nil || detail == nil {
		t.Fatalf("modelDetail = %v, %v", detail, err)
	}
	if detail.Record == nil || detail.Record.Name == "" {
		t.Fatal("the archived model has no name; it would show in the library as a bare hash")
	}
	if detail.Record.Name != "Watercolour" {
		t.Errorf("name = %q, want the provider's", detail.Record.Name)
	}
	if detail.Record.BaseModel != "SDXL" {
		t.Errorf("base model = %q", detail.Record.BaseModel)
	}
	if len(detail.Record.TriggerWords) == 0 {
		t.Error("trigger words were not applied")
	}

	// The origin identity, recorded as a deliberate download rather than the
	// weaker archive-derived inference.
	if len(detail.Origins) != 1 || detail.Origins[0].VersionID != "4567" {
		t.Fatalf("origins = %+v", detail.Origins)
	}
	if detail.Origins[0].Source != store.OriginSourceDownload {
		t.Errorf("origin source = %q, want download", detail.Origins[0].Source)
	}

	// The staged previews moved onto the model now that there is a row to hang
	// them on.
	if len(detail.Previews) == 0 {
		t.Error("staged previews were not attached once the file landed")
	}

	// And the archive record now names the file it captured.
	item, _ := r.st.ArchiveItemFor("civitai", "999", "4567")
	if item == nil || item.SHA256 != sha || !item.FileOK {
		t.Errorf("archive item = %+v", item)
	}
}

// Id-keyed cache rows must not reach the ownership index. Filed under civitai
// they would inject an owned version whose hash is the literal string "4567",
// and the library would claim to own a model nobody downloaded.
func TestArchivedIdRowsDoNotPolluteOwnership(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, nil)
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d", w.Code)
	}
	r.waitForItem(t)

	idx, err := origin.BuildLocalIndex(r.st)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing has been downloaded, so nothing is owned. A leaked id row would
	// show up as an owned version here.
	for _, provider := range []string{"civitai", "civarchive"} {
		if ids := idx.OwnedModelIDs(provider); len(ids) != 0 {
			t.Errorf("%s ownership index polluted by archived id rows: %v", provider, ids)
		}
	}
}

// A partial archive is the normal case, must say which part is missing, and must
// be finishable by re-running.
func TestPartialArchiveIsVisibleAndReRunnable(t *testing.T) {
	p := &fakeProvider{videoPreview: true}
	r := newArchiveRig(t, p, nil)
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d", w.Code)
	}
	item := r.waitForItem(t)

	if item.PreviewsOK {
		t.Error("previews reported complete with one unstorable image")
	}
	if item.PreviewsGot >= item.PreviewsTotal {
		t.Errorf("counts = %d/%d, want a shortfall", item.PreviewsGot, item.PreviewsTotal)
	}
	if item.LastError == "" {
		t.Error("a partial archive with no explanation; partial must not mean unexplained")
	}

	// The images that could be stored were stored, rather than the whole step
	// failing.
	staged, err := r.st.ArchivePreviews("civitai", "999", "4567")
	if err != nil || len(staged) == 0 {
		t.Fatalf("no previews staged despite a partial success: %v", err)
	}

	// A re-run refetches only what is missing. The provider becomes healthy, so
	// the shortfall closes.
	p.videoPreview = false
	before := p.hits["/model-versions/4567"]
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("re-run returned %d", w.Code)
	}
	item = r.waitForItem(t)
	if p.hits["/model-versions/4567"] != before {
		t.Error("a re-run re-fetched the version body, which was already complete")
	}
	if !item.PreviewsOK {
		t.Errorf("the re-run did not close the preview shortfall: %+v", item)
	}
}

// The state the whole feature exists for. A version the provider has removed is
// stamped, and the model-level fact is recorded too so the update sweep stops
// asking.
func TestTakedownIsRecordedAndNotRequeued(t *testing.T) {
	p := &fakeProvider{}
	r := newArchiveRig(t, p, nil)
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d", w.Code)
	}
	r.waitForItem(t)

	// Now the provider removes the model.
	p.modelGone = true
	if w := r.pull(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("second pull returned %d", w.Code)
	}
	for i := 0; i < 200; i++ {
		if job, ok := r.srv.cfg.Archives.Current(); ok && job.State != archivejob.StateRunning {
			break
		}
		testutilSleep()
	}

	gone, err := r.st.OriginModelGone("civitai", "999")
	if err != nil || !gone {
		t.Fatalf("OriginModelGone = %v, %v; the sweep would keep asking forever", gone, err)
	}

	// And nothing was written to the negative cache under the hash key space,
	// which is the path that would have expired and re-asked.
	items, _ := r.st.ArchiveItems(store.ArchiveItemsQuery{})
	if len(items) == 0 {
		t.Error("the archive record was dropped when the model went away; it is the surviving copy")
	}
}

// Intake initiates outbound requests on a timer with nobody present, which is a
// permission --writable never implied.
func TestArchiveRefusedWithoutTheFlag(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, func(c *Config) { c.AllowArchive = false })

	w := r.pull(t, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pull returned %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "--allow-archive") {
		t.Errorf("the refusal should name the flag: %s", w.Body.String())
	}
}

func TestArchiveRefusedInReadOnlyMode(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, func(c *Config) { c.ReadOnly = true })
	if w := r.pull(t, ""); w.Code != http.StatusForbidden {
		t.Fatalf("pull returned %d, want 403", w.Code)
	}
}

// The inventory is what the panel reads, and the gone filter is the archive's
// reason to exist rather than a query trick.
func TestArchiveInventoryAndWatchlist(t *testing.T) {
	r := newArchiveRig(t, &fakeProvider{}, nil)
	if w := r.pull(t, `{"provider":"civitai","model_id":"999","watch":true,"dest_root":`+
		mustJSON(t, r.root)+`}`); w.Code != http.StatusAccepted {
		t.Fatalf("pull returned %d", w.Code)
	}
	r.waitForItem(t)

	w := do(r.srv, "GET", "http://localhost/api/archive/items", "", nil)
	var items struct {
		Items []store.ArchiveItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items.Items) != 1 {
		t.Fatalf("items = %+v", items.Items)
	}

	// Watching came along with the intake, which is the ordinary intent.
	w = do(r.srv, "GET", "http://localhost/api/archive/watch", "", nil)
	var watches struct {
		Watches []store.ArchiveWatch `json:"watches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &watches); err != nil {
		t.Fatal(err)
	}
	if len(watches.Watches) != 1 || watches.Watches[0].ModelID != "999" {
		t.Fatalf("watches = %+v", watches.Watches)
	}
	// Off unless asked for: a watch subscribes to information, not to unattended
	// multi-gigabyte downloads.
	if watches.Watches[0].AutoPull {
		t.Error("auto_pull defaulted on")
	}

	w = do(r.srv, "DELETE", "http://localhost/api/archive/watch/civitai/999", "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unwatch returned %d", w.Code)
	}

	// Status answers from the database even when a run cannot be started, the
	// same rule GET /api/updates follows: hide the button, not the data.
	w = do(r.srv, "GET", "http://localhost/api/archive", "", nil)
	if !strings.Contains(w.Body.String(), "counts") {
		t.Errorf("status carried no counts: %s", w.Body.String())
	}
}

// A distinct colour per URL, so two previews are two different blobs rather
// than one content-addressed file counted twice.
func colorFor(path string) color.RGBA {
	return color.RGBA{R: uint8(len(path) * 7), G: 60, B: 120, A: 255}
}

func testutilSleep() { time.Sleep(10 * time.Millisecond) }
