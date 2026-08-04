package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// A model-version response for the seeded model. Deliberately disagrees with the
// seed on base_model, which is what the precedence test turns on, and carries a
// version and description the seed leaves blank, which is what the fall-through
// test turns on.
const enrichBody = `{
  "id": 12345, "modelId": 999, "name": "v2.0", "baseModel": "Pony Diffusion V6 XL",
  "description": "<p>from the origin</p>",
  "trainedWords": ["ponystyle"],
  "model": {"name": "Origin Name", "type": "LORA", "nsfw": false, "tags": ["style"]},
  "files": [{"name": "m.safetensors", "primary": true, "hashes": {"SHA256": "AAA"}}],
  "images": [{"url": "IMAGE_URL", "width": 512, "height": 768}]
}`

// pngBytes is the smallest thing blobstore.IsImage accepts. The trailing byte
// varies so tests can mint a second image with a different content address.
func pngBytes(tail ...byte) []byte {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0}
	return append(png, tail...)
}

// enrichServer builds a writable server whose origin client points at a stub
// provider, so nothing in these tests reaches the real Civitai.
//
// delay is applied to every provider response, which is how the concurrency test
// keeps a sweep running long enough to collide with a second request.
func enrichServer(t *testing.T, delay time.Duration) (*Server, *store.Store) {
	t.Helper()

	// The provider has to hand back an image URL pointing at itself, so the
	// handler closes over the server it is about to become.
	var self *httptest.Server
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		if strings.HasSuffix(r.URL.Path, "/image.png") {
			_, _ = w.Write(pngBytes(0x01))
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(enrichBody, "IMAGE_URL", self.URL+"/image.png")))
	}))
	t.Cleanup(self.Close)

	client := origin.NewClient()
	client.MinInterval = 0 // no throttling in tests
	client.CivitaiBase = self.URL
	// NewClient reads the ambient environment; a developer with CIVITAI_API_KEY
	// set must not get different test results from one without it.
	client.APIKey, client.HFToken = "", ""

	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	st := testStore(t)

	s := New(Config{
		Store:    st,
		Blobs:    blobs,
		Version:  "test",
		Security: Security{},
		Origin:   client,
		Enrich:   enrichjob.New(st, blobs, func() *origin.Client { return client }),
	})
	return s, st
}

// The point of the button: a field nobody has filled in gets the origin's
// answer, and a preview arrives with it.
func TestEnrichModelFillsBlanksAndFetchesPreview(t *testing.T) {
	s, st := enrichServer(t, 0)

	before, err := st.PreviewImages("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("seeded model already has %d previews", len(before))
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/aaa/enrich", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enrich returned %d: %s", w.Code, w.Body.String())
	}

	rec, err := st.GetModelRecord("aaa")
	if err != nil {
		t.Fatal(err)
	}
	// The seed had no version and no description; both should now be filled.
	if rec.Version != "v2.0" {
		t.Errorf("version = %q, want v2.0 — a blank field did not fall through to the origin", rec.Version)
	}
	if rec.Description == "" {
		t.Error("description is still blank after enrichment")
	}

	after, err := st.PreviewImages("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == 0 {
		t.Error("no preview image was attached")
	}
}

// The precedence guarantee, at the endpoint rather than in the resolver: a value
// the user typed survives a fetch that disagrees, and the disagreement surfaces
// as a suggestion instead of being silently dropped.
func TestEnrichModelNeverOverwritesAManualValue(t *testing.T) {
	s, st := enrichServer(t, 0)

	if err := st.RecordObservations("aaa", provenance.SourceManual, []store.FieldObservation{
		{Field: provenance.FieldBaseModel, Value: "My Own Base"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveModel("aaa"); err != nil {
		t.Fatal(err)
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/aaa/enrich?images=false", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enrich returned %d: %s", w.Code, w.Body.String())
	}

	rec, err := st.GetModelRecord("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if rec.BaseModel != "My Own Base" {
		t.Errorf("base_model = %q, want %q — the fetch overwrote a manual value",
			rec.BaseModel, "My Own Base")
	}

	// Never overwritten must not mean never correctable: the origin's competing
	// value has to remain visible as something the user can accept.
	suggestions, err := st.PendingSuggestions("aaa", 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sg := range suggestions {
		if sg.Field == provenance.FieldBaseModel {
			found = true
		}
	}
	if !found {
		t.Error("no suggestion was raised for the contradicted manual base_model")
	}
}

// A manual preview outranks a fetched one and must still be the thumbnail after
// enrichment attaches more images.
func TestEnrichModelKeepsAChosenThumbnailFirst(t *testing.T) {
	s, st := enrichServer(t, 0)

	// Different bytes than the provider serves, so this lands under its own
	// content address rather than colliding with the fetched image.
	chosen, err := s.storePreview("aaa", pngBytes(0x02), store.SourceManual)
	if err != nil {
		t.Fatal(err)
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/aaa/enrich", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enrich returned %d: %s", w.Code, w.Body.String())
	}

	images, err := st.PreviewImages("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) < 2 {
		t.Fatalf("expected the fetched image alongside the chosen one, got %d", len(images))
	}
	if images[0].ImageSHA256 != chosen.ImageSHA256 {
		t.Errorf("primary preview is %s, want the manually chosen %s",
			images[0].ImageSHA256, chosen.ImageSHA256)
	}
}

// A hash bound by sampled probe could archive another file's metadata here, so
// the run must refuse rather than quietly report "not found at the origin".
func TestEnrichModelRefusesAProvisionalHash(t *testing.T) {
	s, st := enrichServer(t, 0)

	run, err := st.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: "bbb", ProbeSHA256: "q", Size: 4096, Format: "safetensors"},
		store.FilePath{SHA256: "bbb", Path: "/models/loras/prov.safetensors", Root: "/models",
			Device: 1, Inode: 2, Size: 4096, MtimeNs: 1, ScanRunID: run, Provisional: true},
	); err != nil {
		t.Fatal(err)
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/bbb/enrich", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("provisional model enriched with %d, want 409: %s", w.Code, w.Body.String())
	}
	var body apiError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Detail, "sampled probe") {
		t.Errorf("detail = %q, want it to name the provisional hash as the cause", body.Detail)
	}
}

// A confirmed hash whose file is simply not on disk right now (moved, deleted,
// an unmounted drive) hits the same stats.Considered == 0 path a provisional
// hash does, but "run mm verify --provisional" is the wrong advice for it --
// the hash was never in question, the file just is not there.
func TestEnrichModelDistinguishesAbsentFromProvisional(t *testing.T) {
	s, st := enrichServer(t, 0)

	run, err := st.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: "ccc", ProbeSHA256: "r", Size: 4096, Format: "safetensors"},
		store.FilePath{SHA256: "ccc", Path: "/models/loras/gone.safetensors", Root: "/models",
			Device: 1, Inode: 3, Size: 4096, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`UPDATE model_file_path SET present = 0 WHERE sha256 = ?`, "ccc"); err != nil {
		t.Fatal(err)
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/ccc/enrich", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("absent model enriched with %d, want 409: %s", w.Code, w.Body.String())
	}
	var body apiError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Detail, "sampled probe") || strings.Contains(body.Detail, "verify --provisional") {
		t.Errorf("detail = %q, wrongly blames a provisional hash for a file that is simply absent", body.Detail)
	}
	if !strings.Contains(body.Error, "not present") {
		t.Errorf("error = %q, want it to say the model is not present on disk", body.Error)
	}
}

// A path that is BOTH absent and provisional -- a sampled-probe binding for a
// file that has since gone missing -- must still report "not present", not
// "not confirmed". "Run mm verify --provisional" cannot hash a file that
// is not there, so blaming the hash would send the user to a command that
// cannot help. This is also the fixture that actually exercises the `&&`:
// TestEnrichModelRefusesAProvisionalHash only covers (present, provisional)
// and TestEnrichModelDistinguishesAbsentFromProvisional only covers (absent,
// non-provisional) -- neither alone can tell `p.Present && p.Provisional`
// apart from `p.Present || p.Provisional` or from `p.Provisional` alone.
func TestEnrichModelReportsAbsentEvenWhenTheAbsentPathIsProvisional(t *testing.T) {
	s, st := enrichServer(t, 0)

	run, err := st.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		store.ModelFile{SHA256: "ddd", ProbeSHA256: "s", Size: 4096, Format: "safetensors"},
		store.FilePath{SHA256: "ddd", Path: "/models/loras/gone-prov.safetensors", Root: "/models",
			Device: 1, Inode: 4, Size: 4096, MtimeNs: 1, ScanRunID: run, Provisional: true},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`UPDATE model_file_path SET present = 0 WHERE sha256 = ?`, "ddd"); err != nil {
		t.Fatal(err)
	}

	w := do(s, "POST", "http://127.0.0.1/api/models/ddd/enrich", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("absent+provisional model enriched with %d, want 409: %s", w.Code, w.Body.String())
	}
	var body apiError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Detail, "sampled probe") || strings.Contains(body.Detail, "verify --provisional") {
		t.Errorf("detail = %q, blamed the provisional flag on a path that is not even present", body.Detail)
	}
	if !strings.Contains(body.Error, "not present") {
		t.Errorf("error = %q, want it to say the model is not present on disk", body.Error)
	}
}

// Read-only is the boundary that stops anything acting on an unproven index, and
// enrichment writes.
func TestEnrichRefusedWhenReadOnly(t *testing.T) {
	s, _ := enrichServer(t, 0)
	s.cfg.ReadOnly = true

	for _, target := range []string{
		"http://127.0.0.1/api/models/aaa/enrich",
		"http://127.0.0.1/api/enrich",
	} {
		if w := do(s, "POST", target, "", nil); w.Code != http.StatusForbidden {
			t.Errorf("%s returned %d on a read-only server, want 403", target, w.Code)
		}
	}
}

// --no-remote means this daemon does not contact third parties, and that has to
// hold for the button as much as for browsing.
func TestEnrichUnavailableWithoutAnOriginClient(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = nil })

	w := do(s, "POST", "http://127.0.0.1/api/models/aaa/enrich", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("enrich returned %d without an origin client, want 503", w.Code)
	}
}

// GET /api/enrich's "available" field has to agree with what the two POST
// endpoints actually enforce. serve.go currently only ever constructs an
// enrichjob.Manager alongside an origin.Client (never one without the
// other), which would make checking Enrich alone happen to work -- but nothing
// enforces that pairing at the type level, so the status endpoint checks both
// explicitly via the same enrichPrereq the POST endpoints use, rather than
// relying on how one particular caller happens to wire the two together.
func TestEnrichStatusUnavailableWithoutAnOriginClientEvenIfEnrichIsSet(t *testing.T) {
	s := newServer(t, func(c *Config) {
		c.Origin = nil
		c.Enrich = &enrichjob.Manager{}
	})

	w := do(s, "GET", "http://127.0.0.1/api/enrich", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Available {
		t.Error("available = true with no origin client, want false regardless of whether an Enrich manager exists")
	}
}

// scope=search must cover everything matching the filters, not the 50-row page
// the grid happens to be showing.
func TestBulkEnrichCoversEveryMatchNotJustThePage(t *testing.T) {
	s, st := enrichServer(t, 0)

	run, err := st.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(sha, typ string, inode uint64) {
		t.Helper()
		if err := st.UpsertFileAndPath(
			store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 4096, Format: "safetensors"},
			store.FilePath{SHA256: sha, Path: "/models/x/" + sha + ".safetensors", Root: "/models",
				Device: 1, Inode: inode, Size: 4096, MtimeNs: 1, ScanRunID: run},
		); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordObservations(sha, provenance.SourceCivitai, []store.FieldObservation{
			{Field: provenance.FieldType, Value: typ},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResolveModel(sha); err != nil {
			t.Fatal(err)
		}
	}
	// One extra lora beyond the seeded "aaa", plus a checkpoint the filter must
	// exclude.
	seed("ccc", "lora", 3)
	seed("ddd", "checkpoint", 4)

	shas, err := st.SearchSHAs(store.SearchQuery{Types: []string{"lora"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(shas) != 2 {
		t.Fatalf("SearchSHAs(type=lora) returned %d models (%v), want 2", len(shas), shas)
	}

	w := do(s, "POST", "http://127.0.0.1/api/enrich?scope=search&type=lora&images=false", "", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("bulk enrich returned %d, want 202: %s", w.Code, w.Body.String())
	}

	var job enrichjob.Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Scope != "search" {
		t.Errorf("scope = %q, want search", job.Scope)
	}

	final := awaitEnrich(t, s)
	if final.State != enrichjob.StateComplete {
		t.Fatalf("run ended %s: %s", final.State, final.Error)
	}
	if final.ModelsTotal != 2 {
		t.Errorf("swept %d models, want the 2 matching loras", final.ModelsTotal)
	}
	// The checkpoint must not have been touched.
	if final.Found > 2 {
		t.Errorf("found %d records from a 2-model sweep", final.Found)
	}
}

// A second sweep must not start alongside the first: two would each honour their
// own throttle and together double the request rate.
func TestBulkEnrichRefusesASecondConcurrentRun(t *testing.T) {
	s, _ := enrichServer(t, 300*time.Millisecond)

	if w := do(s, "POST", "http://127.0.0.1/api/enrich?images=false", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("first run returned %d, want 202: %s", w.Code, w.Body.String())
	}
	w := do(s, "POST", "http://127.0.0.1/api/enrich?images=false", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second concurrent run returned %d, want 409", w.Code)
	}
	// The running job comes back with the refusal, so a client that lost track
	// of it has something to poll rather than just being told no.
	var body struct {
		Job *enrichjob.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Job == nil || body.Job.ID == "" {
		t.Error("the 409 did not carry the job already running")
	}

	s.cfg.Enrich.Cancel("")
	awaitEnrich(t, s)
}

func TestBulkEnrichRejectsAnUnknownScope(t *testing.T) {
	s, _ := enrichServer(t, 0)
	if w := do(s, "POST", "http://127.0.0.1/api/enrich?scope=everything", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("unknown scope returned %d, want 400", w.Code)
	}
}

// awaitEnrich polls until the run leaves the running state.
func awaitEnrich(t *testing.T, s *Server) enrichjob.Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := s.cfg.Enrich.Current()
		if ok && job.State != enrichjob.StateRunning {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enrichment run did not finish within 20s")
	return enrichjob.Job{}
}
