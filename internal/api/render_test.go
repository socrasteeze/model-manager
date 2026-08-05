package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/basemodel"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

// fakeComfyServer answers the three calls the client makes, returning a real
// PNG so the attach path -- sniff, thumbnail, store -- runs for real.
func fakeComfyServer(t *testing.T, image []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var queued json.RawMessage

	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"system":{"comfyui_version":"0.3.99"}}`))
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		queued = body.Prompt
		_, _ = w.Write([]byte(`{"prompt_id":"p-1"}`))
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"p-1":{"status":{"completed":true},"outputs":{"8":{"images":[
            {"filename":"out.png","subfolder":"","type":"output"}]}}}}`))
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(image)
	})
	mux.HandleFunc("/queued", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(queued)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func renderServer(t *testing.T, comfyURL string) *Server {
	t.Helper()
	return serverWithRoot(t, testutil.TempDir(t), func(c *Config) {
		c.ReadOnly = false
		mgr := comfy.NewManager(nil, nil)
		mgr.PollInterval = time.Millisecond
		mgr.Timeout = 10 * time.Second
		c.Renders = mgr
	})
}

func waitForRender(t *testing.T, s *Server, id string, want comfy.State) comfy.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := s.cfg.Renders.Job(id); ok && j.State == want {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	j, _ := s.cfg.Renders.Job(id)
	t.Fatalf("render never reached %s (state %s, error %q)", want, j.State, j.Error)
	return comfy.Job{}
}

func postRender(t *testing.T, s *Server, sha, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"http://localhost/api/models/"+sha+"/previews/render", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestRenderProducesAManualPreview(t *testing.T) {
	png := testPNG(900, 900)
	comfyURL := fakeComfyServer(t, png).URL
	s := renderServer(t, comfyURL)

	if err := s.cfg.Store.PutSetting(store.SettingComfyURL, comfyURL); err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.Store.PutSetting(store.SettingComfyCheckpoint, "base.safetensors"); err != nil {
		t.Fatal(err)
	}

	rec := postRender(t, s, "aaa", `{"prompt":"a portrait"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Render comfy.Job `json:"render"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	done := waitForRender(t, s, started.Render.ID, comfy.StateComplete)
	if done.ImageSHA256 == "" {
		t.Fatal("the render completed without attaching an image")
	}

	previews, err := s.cfg.Store.PreviewImages("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 {
		t.Fatalf("got %d previews, want 1", len(previews))
	}
	// A picture the user asked this app to make is a picture they chose, so it
	// gets the same rank as an upload and enrichment cannot displace it.
	if previews[0].Source != store.SourceManual {
		t.Errorf("source = %q, want manual", previews[0].Source)
	}
	if previews[0].ThumbSHA256 == "" {
		t.Error("a rendered preview got no grid thumbnail")
	}
}

// The graph that reaches ComfyUI must name this model's file, or the thumbnail
// is a picture of something else.
func TestRenderedGraphNamesTheModelBeingPreviewed(t *testing.T) {
	comfySrv := fakeComfyServer(t, testPNG(600, 600))
	s := renderServer(t, comfySrv.URL)
	if err := s.cfg.Store.PutSetting(store.SettingComfyURL, comfySrv.URL); err != nil {
		t.Fatal(err)
	}

	rec := postRender(t, s, "aaa", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Render comfy.Job `json:"render"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	waitForRender(t, s, started.Render.ID, comfy.StateComplete)

	res, err := http.Get(comfySrv.URL + "/queued")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var graph map[string]struct {
		ClassType string         `json:"class_type"`
		Inputs    map[string]any `json:"inputs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&graph); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, node := range graph {
		if node.ClassType == "LoraLoader" && node.Inputs["lora_name"] == "a.safetensors" {
			found = true
		}
	}
	if !found {
		t.Errorf("the queued graph does not load this model's file: %+v", graph)
	}
}

func TestRenderRefusedWithoutAConfiguredComfyUI(t *testing.T) {
	s := renderServer(t, "")

	rec := postRender(t, s, "aaa", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// The message has to say what to do, because "400" is not actionable.
	if !strings.Contains(rec.Body.String(), "8188") {
		t.Errorf("the refusal does not say how to configure it: %s", rec.Body.String())
	}
}

func TestRenderRefusedInReadOnlyMode(t *testing.T) {
	s := serverWithRoot(t, testutil.TempDir(t), func(c *Config) {
		c.ReadOnly = true
		c.Renders = comfy.NewManager(nil, nil)
	})
	if rec := postRender(t, s, "aaa", `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// The UI asks before offering a Render button: a button that fails after thirty
// seconds of waiting is worse than one that is not offered.
func TestComfyStatusReportsConfiguredAndReachable(t *testing.T) {
	s := renderServer(t, "")

	get := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/comfy", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if st := get(); st["configured"] != false || st["reachable"] != false {
		t.Errorf("unconfigured status = %+v", st)
	}

	comfyURL := fakeComfyServer(t, testPNG(64, 64)).URL
	if err := s.cfg.Store.PutSetting(store.SettingComfyURL, comfyURL); err != nil {
		t.Fatal(err)
	}
	st := get()
	if st["configured"] != true || st["reachable"] != true {
		t.Errorf("configured status = %+v", st)
	}
	if st["version"] != "0.3.99" {
		t.Errorf("version = %v", st["version"])
	}
}

// A ComfyUI that answers with something that is not an image must not become a
// preview: it is a service the operator configured, not a trusted one.
func TestNonImageFromComfyIsRefused(t *testing.T) {
	comfyURL := fakeComfyServer(t, []byte("<html>login page</html>")).URL
	s := renderServer(t, comfyURL)
	if err := s.cfg.Store.PutSetting(store.SettingComfyURL, comfyURL); err != nil {
		t.Fatal(err)
	}

	rec := postRender(t, s, "aaa", `{}`)
	var started struct {
		Render comfy.Job `json:"render"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)

	failed := waitForRender(t, s, started.Render.ID, comfy.StateFailed)
	if !strings.Contains(failed.Error, "not a recognised image") {
		t.Errorf("error = %q", failed.Error)
	}
	if previews, _ := s.cfg.Store.PreviewImages("aaa"); len(previews) != 0 {
		t.Errorf("a non-image was attached anyway: %+v", previews)
	}
}

// One graph cannot serve four architectures. An SDXL/Illustrious lora and a
// FLUX.2 lora need different loaders, a different text encoder and a different
// VAE, so the workflow is chosen by base-model family the same way a download
// folder is chosen by (root, type).
func TestWorkflowIsChosenByBaseModelFamily(t *testing.T) {
	s := renderServer(t, "")

	illustrious := `{"1":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"{{checkpoint}}"}}}`
	flux2 := `{"1":{"class_type":"UNETLoader","inputs":{"unet_name":"{{checkpoint}}"}}}`
	fallback := `{"1":{"class_type":"FallbackNode","inputs":{}}}`

	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, map[string]string{
		basemodel.Illustrious: illustrious,
		basemodel.Flux2:       flux2,
		"":                    fallback,
	}); err != nil {
		t.Fatal(err)
	}

	if got := mustTemplate(t, s, basemodel.Illustrious); got != illustrious {
		t.Errorf("Illustrious got the wrong graph:\n%s", got)
	}
	if got := mustTemplate(t, s, basemodel.Flux2); got != flux2 {
		t.Errorf("Flux.2 got the wrong graph:\n%s", got)
	}
	// A family with no slot configured falls to the default rather than to a
	// graph belonging to a different architecture.
	if got := mustTemplate(t, s, basemodel.Anima); got != fallback {
		t.Errorf("Anima did not fall back to the default:\n%s", got)
	}
}

// Earlier versions stored one workflow as a plain string. That must keep
// working and mean "the default for every family" rather than breaking renders.
func TestABareWorkflowStringStillWorksAsTheDefault(t *testing.T) {
	s := renderServer(t, "")

	graph := `{"1":{"class_type":"KSampler","inputs":{}}}`
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, graph); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{basemodel.Illustrious, basemodel.Flux2, ""} {
		if got := mustTemplate(t, s, family); got != graph {
			t.Errorf("family %q got %s", family, got)
		}
	}
}

// A graph stored directly as an object must not be mistaken for a family map --
// its keys are node ids, and reading them as family names would leave every
// family with no workflow at all.
func TestAGraphObjectIsNotMistakenForAFamilyMap(t *testing.T) {
	s := renderServer(t, "")

	graph := json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{"seed":1}}}`)
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, graph); err != nil {
		t.Fatal(err)
	}
	got := mustTemplate(t, s, basemodel.Illustrious)
	if !strings.Contains(got, "KSampler") {
		t.Errorf("a directly-stored graph was lost: %s", got)
	}
}

// Rendering an Illustrious lora on a FLUX.2 checkpoint is not a worse picture,
// it is a ComfyUI error -- so the checkpoint is per family too.
func TestCheckpointIsChosenByFamily(t *testing.T) {
	s := renderServer(t, "")

	if err := s.cfg.Store.PutSetting(store.SettingComfyCheckpoint, map[string]string{
		basemodel.Illustrious: "illustrious_v2.safetensors",
		basemodel.Flux2:       "flux2-klein.safetensors",
		"":                    "sd_xl_base_1.0.safetensors",
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.checkpointFor(basemodel.Illustrious); got != "illustrious_v2.safetensors" {
		t.Errorf("Illustrious checkpoint = %q", got)
	}
	if got := s.checkpointFor(basemodel.Flux2); got != "flux2-klein.safetensors" {
		t.Errorf("Flux.2 checkpoint = %q", got)
	}
	if got := s.checkpointFor(basemodel.Anima); got != "sd_xl_base_1.0.safetensors" {
		t.Errorf("unconfigured family did not fall back: %q", got)
	}

	// And a bare string, as earlier versions stored, still means "everything".
	if err := s.cfg.Store.PutSetting(store.SettingComfyCheckpoint, "one.safetensors"); err != nil {
		t.Fatal(err)
	}
	if got := s.checkpointFor(basemodel.Flux2); got != "one.safetensors" {
		t.Errorf("bare checkpoint string = %q", got)
	}
}

func mustTemplate(t *testing.T, s *Server, family string) string {
	t.Helper()
	wf, err := s.workflowTemplate(family)
	if err != nil {
		t.Fatalf("resolving the %q workflow: %v", family, err)
	}
	return string(wf)
}

// Pointing at a file rather than pasting JSON is the better of the two: the
// workflow stays where ComfyUI saved it, stays editable there, and the next
// render picks the edit up.
func TestAFamilySlotCanNameAWorkflowFile(t *testing.T) {
	dir := testutil.TempDir(t)
	path := filepath.Join(dir, "flux2-preview.json")
	graph := `{"1":{"class_type":"UNETLoader","inputs":{"unet_name":"{{checkpoint}}"}}}`
	if err := os.WriteFile(path, []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}

	s := renderServer(t, "")
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflowDir, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, map[string]string{
		basemodel.Flux2: "flux2-preview.json",
	}); err != nil {
		t.Fatal(err)
	}

	if got := mustTemplate(t, s, basemodel.Flux2); got != graph {
		t.Errorf("named file not loaded, got %s", got)
	}

	// Edited in ComfyUI, and the next render sees it -- the whole point of
	// naming a file instead of copying its contents into the database.
	edited := `{"1":{"class_type":"UNETLoader","inputs":{"unet_name":"edited"}}}`
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustTemplate(t, s, basemodel.Flux2); got != edited {
		t.Errorf("an edit to the file was not picked up: %s", got)
	}
}

// A stored relative name must not walk out of the workflow folder.
func TestWorkflowNameCannotEscapeTheFolder(t *testing.T) {
	outside := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(testutil.TempDir(t), "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := renderServer(t, "")
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflowDir, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.json", "../../secret.json"} {
		got, err := s.resolveWorkflowPath(name)
		if err == nil && !withinRoot(dir, got) {
			t.Errorf("%q escaped the workflow folder: %q", name, got)
		}
	}
}

// A file that has gone missing must be reported where it is configured, not
// discovered when a render fails at 2am. That is the cost of pointing at files
// rather than copying them, and it is worth paying only if it is visible.
func TestMissingWorkflowFileIsReportedInStatus(t *testing.T) {
	dir := testutil.TempDir(t)
	s := renderServer(t, "")
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflowDir, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, map[string]string{
		basemodel.Flux2: "gone.json",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/comfy/status", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	var out struct {
		Families []familyStatus `json:"families"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var flux2 *familyStatus
	for i := range out.Families {
		if out.Families[i].Family == basemodel.Flux2 {
			flux2 = &out.Families[i]
		}
	}
	if flux2 == nil {
		t.Fatal("Flux.2 missing from the status report")
	}
	if flux2.OK || flux2.Error == "" {
		t.Errorf("a missing workflow file was reported as fine: %+v", flux2)
	}
	if flux2.Source != "file" || flux2.File != "gone.json" {
		t.Errorf("status did not say which file: %+v", flux2)
	}
}

// A graph that never loads the model renders the same picture for every model.
// Worth saying out loud, but not worth refusing: previewing checkpoints is a
// legitimate case with no separate model to load.
func TestLintWarnsAboutAWorkflowThatIgnoresTheModel(t *testing.T) {
	s := renderServer(t, "")
	if err := s.cfg.Store.PutSetting(store.SettingComfyWorkflow, map[string]string{
		basemodel.Anima: `{"1":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"x"}},` +
			`"8":{"class_type":"SaveImage","inputs":{}}}`,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/comfy/status", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	var out struct {
		Families []familyStatus `json:"families"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, f := range out.Families {
		if f.Family != basemodel.Anima {
			continue
		}
		if !f.OK {
			t.Errorf("a usable workflow was rejected: %+v", f)
		}
		var codes []string
		for _, w := range f.Warnings {
			codes = append(codes, w.Code)
		}
		if !slicesContains(codes, comfy.WarnNoLoraInput) {
			t.Errorf("no warning about ignoring the model: %v", codes)
		}
		return
	}
	t.Fatal("Anima missing from the status report")
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
