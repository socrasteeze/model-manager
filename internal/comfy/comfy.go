// Package comfy submits workflows to a ComfyUI instance and collects the images
// it produces.
//
// This is the one place the app treats ComfyUI as a *running service* rather
// than as a file format it can read. Everything else about ComfyUI in this
// project -- the folder vocabulary, the workflow chunk inside a PNG, the output
// directory the picker reads -- works whether ComfyUI is running or not. This
// does not, and the API is shaped around that: a render is a job that can fail
// because nothing is listening, and saying so plainly is more useful than a
// timeout.
//
// The protocol is three calls:
//
//	POST /prompt                    queue a graph        -> prompt_id
//	GET  /history/{prompt_id}       poll until outputs appear
//	GET  /view?filename=&subfolder=&type=   fetch the bytes
//
// One constraint worth stating up front, because it decides what can be
// re-run: ComfyUI accepts the *API* form of a graph, not the editor form. A PNG
// exported by ComfyUI carries both -- `prompt` is the API form, `workflow` is
// the editor form -- and only the former can be submitted back. Converting
// editor to API needs the node definitions of whatever custom nodes were
// installed, which this app does not have and should not pretend to.
package comfy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotConfigured means no ComfyUI address has been set.
var ErrNotConfigured = errors.New("comfy: no ComfyUI address configured")

// ErrUnreachable means ComfyUI is configured but nothing answered.
var ErrUnreachable = errors.New("comfy: ComfyUI is not reachable")

// ErrEditorFormat means the graph is the editor form, which cannot be queued.
var ErrEditorFormat = errors.New(
	"comfy: this is a ComfyUI editor workflow, not the API form it accepts. " +
		"In ComfyUI, enable Settings > Enable Dev mode Options and use " +
		"\"Save (API Format)\", or attach an image whose `prompt` chunk is present")

// MaxImageBytes caps a fetched render. A preview is a picture, not a dataset.
const MaxImageBytes = 32 << 20

// Client talks to one ComfyUI instance.
type Client struct {
	// BaseURL is the root of the ComfyUI HTTP API, e.g. http://127.0.0.1:8188.
	BaseURL string

	HTTP *http.Client
}

// NewClient builds a client for base.
//
// The timeout is per request, not per render: queueing returns immediately and
// the wait happens across many short polls, so a long render never sits inside
// one long-held connection.
func NewClient(base string) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, ErrNotConfigured
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("comfy: %q is not an http(s) address", base)
	}
	return &Client{
		BaseURL: base,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Ping reports whether ComfyUI is answering, and its reported version.
func (c *Client) Ping(ctx context.Context) (string, error) {
	var stats struct {
		System struct {
			ComfyUIVersion string `json:"comfyui_version"`
			PythonVersion  string `json:"python_version"`
		} `json:"system"`
	}
	if err := c.getJSON(ctx, "/system_stats", &stats); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return stats.System.ComfyUIVersion, nil
}

// Queue submits an API-format graph and returns its prompt id.
func (c *Client) Queue(ctx context.Context, graph json.RawMessage, clientID string) (string, error) {
	if err := checkAPIFormat(graph); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{"prompt": graph, "client_id": clientID})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/prompt",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		// ComfyUI puts the useful part -- which node rejected what -- in the
		// body. Passing the status alone would turn a fixable graph error into
		// an opaque 400.
		return "", fmt.Errorf("comfy: ComfyUI refused the workflow (%s): %s",
			res.Status, strings.TrimSpace(string(raw)))
	}

	var out struct {
		PromptID   string          `json:"prompt_id"`
		NodeErrors json.RawMessage `json:"node_errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("comfy: unrecognised /prompt response: %w", err)
	}
	if out.PromptID == "" {
		return "", fmt.Errorf("comfy: ComfyUI accepted nothing: %s", strings.TrimSpace(string(raw)))
	}
	return out.PromptID, nil
}

// ImageRef names one output image.
type ImageRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// history is the shape of GET /history/{id}.
type history struct {
	Status struct {
		Completed bool   `json:"completed"`
		StatusStr string `json:"status_str"`
		Messages  []any  `json:"messages"`
	} `json:"status"`
	Outputs map[string]struct {
		Images []ImageRef `json:"images"`
	} `json:"outputs"`
}

// Result is a finished render.
type Result struct {
	Images []ImageRef
}

// Wait polls until the prompt finishes, and returns the images it produced.
//
// Polling rather than the websocket ComfyUI also offers: a render is tens of
// seconds, a poll is one cheap GET, and a websocket would add a reconnect state
// machine to a path whose failure mode should be "ComfyUI is not running".
func (c *Client) Wait(ctx context.Context, promptID string, interval time.Duration) (*Result, error) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		var h map[string]history
		if err := c.getJSON(ctx, "/history/"+url.PathEscape(promptID), &h); err == nil {
			if entry, ok := h[promptID]; ok {
				var images []ImageRef
				for _, out := range entry.Outputs {
					images = append(images, out.Images...)
				}
				if len(images) > 0 {
					return &Result{Images: images}, nil
				}
				// Completed with no images means the graph ran but saves
				// nothing -- a SaveImage node is missing, or its output went
				// somewhere this API does not report. Waiting longer will not
				// change that.
				if entry.Status.Completed {
					return nil, errors.New(
						"comfy: the workflow finished but produced no image. It needs a " +
							"SaveImage (or PreviewImage) node for the render to come back")
				}
				if s := entry.Status.StatusStr; s != "" && s != "success" {
					return nil, fmt.Errorf("comfy: ComfyUI reported %q", s)
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Fetch downloads one output image.
func (c *Client) Fetch(ctx context.Context, ref ImageRef) ([]byte, error) {
	params := url.Values{}
	params.Set("filename", ref.Filename)
	params.Set("subfolder", ref.Subfolder)
	params.Set("type", orDefault(ref.Type, "output"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/view?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comfy: fetching %s: %s", ref.Filename, res.Status)
	}
	// Size-capped, and the caller sniffs the bytes before storing them. Same
	// rule as every other image this app admits: what a server says it sent is
	// a claim, not a fact.
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("comfy: %s is over the %d-byte limit", ref.Filename, MaxImageBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("comfy: %s came back empty", ref.Filename)
	}
	return data, nil
}

func (c *Client) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(into)
}

// CheckAPIFormat reports whether a graph is the API form ComfyUI accepts.
// Exported so callers importing a graph from elsewhere can say what is wrong
// with it before storing it.
func CheckAPIFormat(graph json.RawMessage) error { return checkAPIFormat(graph) }

// checkAPIFormat rejects an editor-format graph before it is sent.
//
// The two are easy to tell apart and impossible to convert between here: the
// editor form is {"nodes": [...], "links": [...]}, the API form is a flat object
// keyed by node id where each value has a `class_type`. Catching it locally
// turns a confusing ComfyUI 400 into a sentence that says what to do.
func checkAPIFormat(graph json.RawMessage) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(graph, &probe); err != nil {
		return fmt.Errorf("comfy: workflow is not a JSON object: %w", err)
	}
	if len(probe) == 0 {
		return errors.New("comfy: workflow is empty")
	}
	if _, hasNodes := probe["nodes"]; hasNodes {
		if _, hasLinks := probe["links"]; hasLinks {
			return ErrEditorFormat
		}
	}
	for _, raw := range probe {
		var node struct {
			ClassType string `json:"class_type"`
		}
		if err := json.Unmarshal(raw, &node); err == nil && node.ClassType != "" {
			return nil
		}
	}
	return ErrEditorFormat
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
