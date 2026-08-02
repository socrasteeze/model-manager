package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/store"
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
	return serverWithRoot(t, t.TempDir(), func(c *Config) {
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
	s := serverWithRoot(t, t.TempDir(), func(c *Config) {
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
