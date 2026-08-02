package comfy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeComfy is a ComfyUI stand-in speaking the three calls this client makes.
type fakeComfy struct {
	mu sync.Mutex

	// pollsBeforeDone is how many /history requests return "still working"
	// before the outputs appear.
	pollsBeforeDone int
	polls           int

	queued json.RawMessage
	image  []byte

	// refuse makes /prompt answer 400 the way ComfyUI does for a bad graph.
	refuse string
	// completeWithNoImages reproduces a graph that runs but saves nothing.
	completeWithNoImages bool
}

func (f *fakeComfy) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"system":{"comfyui_version":"0.3.99"}}`))
	})

	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(f.refuse))
			return
		}
		var body struct {
			Prompt   json.RawMessage `json:"prompt"`
			ClientID string          `json:"client_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.queued = body.Prompt
		_, _ = w.Write([]byte(`{"prompt_id":"p-1","number":1,"node_errors":{}}`))
	})

	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		if f.polls <= f.pollsBeforeDone {
			_, _ = w.Write([]byte(`{"p-1":{"status":{"completed":false},"outputs":{}}}`))
			return
		}
		if f.completeWithNoImages {
			_, _ = w.Write([]byte(`{"p-1":{"status":{"completed":true},"outputs":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"p-1":{"status":{"completed":true},"outputs":{"8":{"images":[
            {"filename":"model-manager_00001_.png","subfolder":"","type":"output"}]}}}}`))
	})

	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "model-manager_00001_.png" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(f.image)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testClient(t *testing.T, f *fakeComfy) *Client {
	t.Helper()
	srv := f.server(t)
	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestQueueWaitAndFetch(t *testing.T) {
	f := &fakeComfy{pollsBeforeDone: 2, image: []byte("\x89PNG\r\n\x1a\npretend")}
	c := testClient(t, f)
	ctx := context.Background()

	if v, err := c.Ping(ctx); err != nil || v != "0.3.99" {
		t.Fatalf("ping = %q, %v", v, err)
	}

	id, err := c.Queue(ctx, json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	if id != "p-1" {
		t.Errorf("prompt id = %q", id)
	}

	res, err := c.Wait(ctx, id, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("got %d images", len(res.Images))
	}

	data, err := c.Fetch(ctx, res.Images[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(f.image) {
		t.Errorf("fetched %q", data)
	}
}

// ComfyUI accepts the API form of a graph, not the editor form. Catching that
// here turns a confusing 400 from ComfyUI into a sentence saying what to do --
// and it is the reason "re-run the workflow this image came with" only works
// for images carrying the `prompt` chunk.
func TestEditorFormatWorkflowIsRefusedLocally(t *testing.T) {
	c := testClient(t, &fakeComfy{})

	editor := json.RawMessage(`{"nodes":[{"id":1,"type":"KSampler"}],"links":[],"version":0.4}`)
	_, err := c.Queue(context.Background(), editor, "test")
	if !errors.Is(err, ErrEditorFormat) {
		t.Fatalf("got %v, want ErrEditorFormat", err)
	}
	if !strings.Contains(err.Error(), "Save (API Format)") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// ComfyUI puts the useful part -- which node rejected what -- in the body.
func TestQueueSurfacesComfyUIsOwnComplaint(t *testing.T) {
	c := testClient(t, &fakeComfy{refuse: `{"error":{"message":"ckpt_name not in list"}}`})

	_, err := c.Queue(context.Background(),
		json.RawMessage(`{"1":{"class_type":"CheckpointLoaderSimple","inputs":{}}}`), "test")
	if err == nil {
		t.Fatal("a refused workflow looked like a success")
	}
	if !strings.Contains(err.Error(), "ckpt_name not in list") {
		t.Errorf("ComfyUI's reason was swallowed: %v", err)
	}
}

// A graph with no SaveImage runs fine and produces nothing. Waiting forever for
// an image that is never coming is the wrong behaviour.
func TestCompletedWithNoImagesFailsRatherThanHanging(t *testing.T) {
	c := testClient(t, &fakeComfy{completeWithNoImages: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Wait(ctx, "p-1", time.Millisecond)
	if err == nil {
		t.Fatal("no error for a workflow that saved nothing")
	}
	if !strings.Contains(err.Error(), "SaveImage") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func TestUnreachableComfyIsNamedAsSuch(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP.Timeout = 500 * time.Millisecond
	if _, err := c.Ping(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
}

func TestNewClientRejectsNonHTTPAddresses(t *testing.T) {
	for _, bad := range []string{"", "   ", "file:///etc/passwd", "not a url at all", "ftp://x"} {
		if _, err := NewClient(bad); err == nil {
			t.Errorf("accepted %q as a ComfyUI address", bad)
		}
	}
}

// --- templates ---------------------------------------------------------------

func TestFillSubstitutesAndEscapes(t *testing.T) {
	tmpl := json.RawMessage(`{
      "1": {"class_type":"LoraLoader","inputs":{"lora_name":"{{model}}"}},
      "2": {"class_type":"CLIPTextEncode","inputs":{"text":"a photo, {{prompt}}"}},
      "3": {"class_type":"KSampler","inputs":{"seed": {{seed}} }}
    }`)

	got, err := Fill(tmpl, Vars{
		Model:        `weird "name".safetensors`,
		TriggerWords: []string{"trig a", `trig "b"`},
		Seed:         12345,
	})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("filled workflow is not valid JSON: %v\n%s", err, got)
	}
	// A quote in a value must survive as a quote in a value, not break the
	// graph or become an extra field.
	if parsed["1"].Inputs["lora_name"] != `weird "name".safetensors` {
		t.Errorf("lora_name = %#v", parsed["1"].Inputs["lora_name"])
	}
	if parsed["2"].Inputs["text"] != `a photo, trig a, trig "b"` {
		t.Errorf("text = %#v", parsed["2"].Inputs["text"])
	}
	// A seed lands in a numeric field; quoting it would make ComfyUI reject it.
	if parsed["3"].Inputs["seed"] != float64(12345) {
		t.Errorf("seed = %#v, want a number", parsed["3"].Inputs["seed"])
	}
}

// A value that closes the string and adds a node would be the injection this
// escaping exists to prevent.
func TestFillCannotInjectExtraNodes(t *testing.T) {
	tmpl := json.RawMessage(`{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"{{prompt}}"}}}`)

	got, err := Fill(tmpl, Vars{
		Prompt: `x"},"inputs":{"text":"y"}},"99":{"class_type":"EvilNode","inputs":{"a":"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 {
		t.Fatalf("substitution added nodes: %v", parsed)
	}
	if _, ok := parsed["99"]; ok {
		t.Error("an injected node made it into the graph")
	}
}

func TestUnknownPlaceholderIsLeftAlone(t *testing.T) {
	tmpl := json.RawMessage(`{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"{{mystery}}"}}}`)
	got, err := Fill(tmpl, Vars{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "{{mystery}}") {
		t.Errorf("an unrecognised placeholder was eaten: %s", got)
	}
}

// A thumbnail that changes every time it is regenerated makes "did my edit
// help?" unanswerable.
func TestSeedIsStablePerModel(t *testing.T) {
	a := SeedFor("abc123")
	if a != SeedFor("abc123") {
		t.Error("seed is not reproducible for one model")
	}
	if a == SeedFor("def456") {
		t.Error("two models got the same seed")
	}
	if a < 0 {
		t.Errorf("seed %d is negative; ComfyUI seed widgets will refuse it", a)
	}
}

func TestDefaultWorkflowIsQueueableAPIFormat(t *testing.T) {
	filled, err := Fill(DefaultWorkflow, Vars{
		Model: "style.safetensors", Checkpoint: "base.safetensors",
		Prompt: "a portrait", Negative: DefaultNegative, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAPIFormat(filled); err != nil {
		t.Fatalf("the shipped default is not queueable: %v", err)
	}

	f := &fakeComfy{image: []byte("\x89PNG\r\n\x1a\npretend")}
	c := testClient(t, f)
	if _, err := c.Queue(context.Background(), filled, "test"); err != nil {
		t.Fatal(err)
	}

	// And the placeholders really did become values.
	f.mu.Lock()
	queued := string(f.queued)
	f.mu.Unlock()
	for _, want := range []string{"style.safetensors", "base.safetensors", "a portrait"} {
		if !strings.Contains(queued, want) {
			t.Errorf("queued graph is missing %q:\n%s", want, queued)
		}
	}
	if strings.Contains(queued, "{{") {
		t.Errorf("an unfilled placeholder was queued:\n%s", queued)
	}
}

// --- render manager ----------------------------------------------------------

func testManager(t *testing.T, f *fakeComfy) (*Manager, *[]byte) {
	t.Helper()
	c := testClient(t, f)
	var attached []byte
	m := NewManager(
		func() (*Client, error) { return c, nil },
		func(sha string, image []byte) (string, error) {
			attached = append([]byte(nil), image...)
			return "image-sha", nil
		},
	)
	m.PollInterval = time.Millisecond
	m.Timeout = 10 * time.Second
	return m, &attached
}

func waitForState(t *testing.T, m *Manager, id string, want State) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := m.Job(id); ok && j.State == want {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	j, _ := m.Job(id)
	t.Fatalf("render never reached %s (state %s, error %q)", want, j.State, j.Error)
	return Job{}
}

func TestRenderAttachesTheResult(t *testing.T) {
	m, attached := testManager(t, &fakeComfy{pollsBeforeDone: 1, image: []byte("\x89PNG\r\n\x1a\nx")})

	job, err := m.Start("model-sha", json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateQueued {
		t.Errorf("state = %q on return; registration must be synchronous", job.State)
	}

	done := waitForState(t, m, job.ID, StateComplete)
	if done.ImageSHA256 != "image-sha" {
		t.Errorf("image = %q", done.ImageSHA256)
	}
	if string(*attached) != "\x89PNG\r\n\x1a\nx" {
		t.Errorf("attached %q", *attached)
	}
}

// Per model, not globally: two renders of one model race to attach a preview
// and one is wasted GPU time, but renders of different models are what a queue
// is for.
func TestOneRenderPerModelButManyAcrossModels(t *testing.T) {
	m, _ := testManager(t, &fakeComfy{pollsBeforeDone: 500, image: []byte("x")})
	graph := json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`)

	first, err := m.Start("model-a", graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("model-a", graph); !errors.Is(err, ErrInFlight) {
		t.Fatalf("a second render of the same model started: %v", err)
	}
	if _, err := m.Start("model-b", graph); err != nil {
		t.Fatalf("a render of a different model was blocked: %v", err)
	}
	m.Cancel(first.ID)
}

func TestCancelStopsWaiting(t *testing.T) {
	m, _ := testManager(t, &fakeComfy{pollsBeforeDone: 100000, image: []byte("x")})

	job, err := m.Start("model-sha", json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, m, job.ID, StateRunning)
	if !m.Cancel(job.ID) {
		t.Fatal("cancel reported nothing to stop")
	}
	waitForState(t, m, job.ID, StateCanceled)
}

func TestRenderFailureCarriesTheReason(t *testing.T) {
	m, _ := testManager(t, &fakeComfy{refuse: `{"error":{"message":"ckpt_name not in list"}}`})

	job, err := m.Start("model-sha", json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForState(t, m, job.ID, StateFailed)
	if !strings.Contains(failed.Error, "ckpt_name not in list") {
		t.Errorf("the job did not carry ComfyUI's reason: %q", failed.Error)
	}
}

// With no address configured, Start must fail before registering a job -- the
// user needs "set the address", not a job that dies a moment later.
func TestStartFailsFastWhenComfyIsNotConfigured(t *testing.T) {
	m := NewManager(
		func() (*Client, error) { return nil, ErrNotConfigured },
		func(string, []byte) (string, error) { return "", nil },
	)
	graph := json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{}}}`)
	if _, err := m.Start("sha", graph); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v, want ErrNotConfigured", err)
	}
	if len(m.Jobs()) != 0 {
		t.Error("a job was registered for a render that could never run")
	}
}

// The format check has to happen before a job is registered. Catching it inside
// the goroutine would hand the caller a 202 and an ID, and an editor-format
// workflow would look like a render that started and then quietly died -- when
// it is a one-step fix, if the user is told immediately.
func TestEditorFormatIsRejectedBeforeAJobExists(t *testing.T) {
	m, _ := testManager(t, &fakeComfy{image: []byte("x")})

	editor := json.RawMessage(`{"nodes":[{"id":1,"type":"KSampler"}],"links":[]}`)
	if _, err := m.Start("sha", editor); !errors.Is(err, ErrEditorFormat) {
		t.Fatalf("got %v, want ErrEditorFormat", err)
	}
	if len(m.Jobs()) != 0 {
		t.Error("a job was registered for a workflow that could never be queued")
	}
}
