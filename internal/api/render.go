package api

// Rendering a thumbnail with ComfyUI.
//
// This is the only feature that makes ComfyUI a *running dependency* rather
// than a file format this app reads. Everything else -- the folder vocabulary,
// the workflow chunk inside a PNG, the output-folder picker -- works whether
// ComfyUI is running or not.
//
// Two consequences shape the endpoints. A render is tens of seconds of someone
// else's GPU, so it is a job rather than a request; and it fails for a reason
// the user can act on ("nothing is listening on that address", "that graph is
// the editor format"), so the errors are sentences rather than status codes.
//
// On --no-remote: rendering stays available. That flag exists to stop the
// daemon talking to *third parties* -- Civitai, HuggingFace, CivArchive -- and
// a ComfyUI address the operator typed into their own settings is not one. It
// is the same class of local resource as the output folder the picker already
// reads. Rendering is instead gated on --writable, because it creates a
// preview.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/basemodel"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/store"
)

// comfyClient builds a client from the configured address.
func (s *Server) comfyClient() (*comfy.Client, error) {
	var base string
	ok, err := s.cfg.Store.GetSettingInto(store.SettingComfyURL, &base)
	if err != nil || !ok || strings.TrimSpace(base) == "" {
		return nil, comfy.ErrNotConfigured
	}
	return comfy.NewClient(base)
}

// workflowTemplate returns the workflow to use for a model of this base-model
// family, falling back to the default and then to the built-in one.
//
// Per family, not one global graph, for the same reason download folders are
// per (root, type) rather than global: an SDXL/Illustrious lora and a FLUX.2
// lora are not two spellings of one thing. They need different loaders, a
// different text encoder, a different VAE. A single graph would silently render
// the wrong thing -- or more likely fail in ComfyUI with a node error about a
// model it cannot load.
//
// Stored as {"<family>": <workflow>, "": <default>}. A bare string or object,
// as earlier versions stored, is read as the default for everything.
func (s *Server) workflowTemplate(family string) json.RawMessage {
	byFamily, fallback := s.workflowSettings()
	if wf, ok := byFamily[family]; ok && len(wf) > 0 {
		return wf
	}
	if len(fallback) > 0 {
		return fallback
	}
	return comfy.DefaultWorkflow
}

// workflowSettings decodes the stored workflow setting into a per-family map
// and a default.
func (s *Server) workflowSettings() (map[string]json.RawMessage, json.RawMessage) {
	var raw json.RawMessage
	ok, _ := s.cfg.Store.GetSettingInto(store.SettingComfyWorkflow, &raw)
	if !ok || len(raw) == 0 {
		return nil, nil
	}

	// A JSON string is a whole workflow pasted into a textarea, which earlier
	// versions stored directly. Kept working rather than migrated: it is the
	// same thing as "the default for every family".
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil, nil
		}
		return nil, json.RawMessage(asString)
	}

	var byFamily map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byFamily); err != nil {
		return nil, nil
	}
	// Distinguishing a per-family map from a bare graph: a graph's values are
	// node objects with a class_type, a map's values are workflows. The marker
	// is the key -- a family map is keyed by family names, never by node ids.
	if looksLikeGraph(byFamily) {
		return nil, raw
	}

	out := make(map[string]json.RawMessage, len(byFamily))
	var fallback json.RawMessage
	for family, wf := range byFamily {
		// Each value may itself be a JSON string, for the same textarea reason.
		decoded := wf
		var inner string
		if err := json.Unmarshal(wf, &inner); err == nil {
			if strings.TrimSpace(inner) == "" {
				continue
			}
			decoded = json.RawMessage(inner)
		}
		if family == "" {
			fallback = decoded
			continue
		}
		out[family] = decoded
	}
	return out, fallback
}

// looksLikeGraph reports whether a decoded object is a ComfyUI graph rather
// than a family map, by asking whether its values are nodes.
func looksLikeGraph(obj map[string]json.RawMessage) bool {
	for _, raw := range obj {
		var node struct {
			ClassType string `json:"class_type"`
		}
		if err := json.Unmarshal(raw, &node); err == nil && node.ClassType != "" {
			return true
		}
	}
	return false
}

// checkpointFor picks the base checkpoint for a family.
//
// Also per family, and for a blunter reason than the workflow: rendering an
// Illustrious lora on a FLUX.2 checkpoint does not produce a worse picture, it
// produces a ComfyUI error. Stored the same way -- a map, or a bare string
// meaning "for everything".
func (s *Server) checkpointFor(family string) string {
	var raw json.RawMessage
	ok, _ := s.cfg.Store.GetSettingInto(store.SettingComfyCheckpoint, &raw)
	if !ok || len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var byFamily map[string]string
	if err := json.Unmarshal(raw, &byFamily); err != nil {
		return ""
	}
	if c, ok := byFamily[family]; ok && strings.TrimSpace(c) != "" {
		return strings.TrimSpace(c)
	}
	return strings.TrimSpace(byFamily[""])
}

type renderRequest struct {
	// Prompt and Negative override what the template would otherwise fill in
	// from the model's trigger words.
	Prompt   string `json:"prompt,omitempty"`
	Negative string `json:"negative,omitempty"`

	// Checkpoint overrides the configured base checkpoint for this one render.
	Checkpoint string `json:"checkpoint,omitempty"`

	// Seed overrides the derived per-model seed. Zero means "use the derived
	// one", which keeps a re-render of the same model reproducible.
	Seed int64 `json:"seed,omitempty"`

	// Workflow, when given, is used instead of the configured template. This is
	// how "re-run the workflow this image came with" works.
	Workflow json.RawMessage `json:"workflow,omitempty"`
}

// handleRenderPreview handles POST /api/models/{sha}/previews/render.
func (s *Server) handleRenderPreview(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Renders == nil || s.cfg.Blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "rendering is disabled",
			"the server was started without a render manager")
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))
	detail, err := s.modelDetail(sha)
	if err != nil || detail == nil {
		writeError(w, http.StatusNotFound, "no such model", sha)
		return
	}

	var req renderRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}

	vars := s.renderVars(sha, detail, req)

	template := req.Workflow
	if len(template) == 0 {
		template = s.workflowTemplate(vars.BaseModel)
	}

	graph, err := comfy.Fill(template, vars)
	if err != nil {
		writeError(w, http.StatusBadRequest, "workflow could not be prepared", err.Error())
		return
	}

	job, err := s.cfg.Renders.Start(sha, graph)
	switch {
	case errors.Is(err, comfy.ErrInFlight):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "a render for this model is already running",
			"render": job,
		})
		return
	case errors.Is(err, comfy.ErrEditorFormat):
		writeError(w, http.StatusBadRequest, "that workflow is the ComfyUI editor format",
			err.Error())
		return
	case errors.Is(err, comfy.ErrNotConfigured):
		writeError(w, http.StatusBadRequest, "no ComfyUI address configured",
			"set "+store.SettingComfyURL+" to where ComfyUI is listening, e.g. http://127.0.0.1:8188")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "could not start the render", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"render": job})
}

// renderVars gathers what a template can substitute for this model.
func (s *Server) renderVars(sha string, detail *ModelDetail, req renderRequest) comfy.Vars {
	v := comfy.Vars{
		Prompt:     req.Prompt,
		Negative:   req.Negative,
		Checkpoint: strings.TrimSpace(req.Checkpoint),
		Seed:       req.Seed,
	}
	if v.Seed == 0 {
		// Derived from the hash, so re-rendering the same model gives the same
		// picture. A thumbnail that changes on every regeneration makes "did my
		// edit help?" unanswerable.
		v.Seed = comfy.SeedFor(sha)
	}
	if v.Negative == "" {
		v.Negative = comfy.DefaultNegative
	}
	// The model's own filename as ComfyUI would load it. Not the full path: a
	// ComfyUI node names a file relative to its models directory, and the
	// absolute path this library records would not resolve there.
	for _, p := range detail.Paths {
		if p.Present {
			v.Model = filenameOnly(p.Path)
			break
		}
	}
	if v.Model == "" && len(detail.Paths) > 0 {
		v.Model = filenameOnly(detail.Paths[0].Path)
	}
	if rec := detail.Record; rec != nil {
		v.Name = rec.Name
		v.BaseModel = basemodel.Normalize(rec.BaseModel)
		v.TriggerWords = rec.TriggerWords
	}
	// Resolved after the family is known, because the checkpoint is per family:
	// an Illustrious lora rendered on a FLUX.2 checkpoint is a ComfyUI error,
	// not a worse picture.
	if v.Checkpoint == "" {
		v.Checkpoint = s.checkpointFor(v.BaseModel)
	}
	return v
}

func filenameOnly(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// handleListRenders handles GET /api/renders.
func (s *Server) handleListRenders(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Renders == nil {
		writeJSON(w, http.StatusOK, map[string]any{"renders": []comfy.Job{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"renders": s.cfg.Renders.Jobs()})
}

// handleCancelRender handles DELETE /api/renders/{id}.
//
// This stops the daemon waiting. ComfyUI keeps whatever it already queued --
// this app does not get to clear someone else's queue.
func (s *Server) handleCancelRender(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Renders == nil {
		writeError(w, http.StatusServiceUnavailable, "rendering is disabled")
		return
	}
	if s.cfg.Renders.Cancel(r.PathValue("id")) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
		return
	}
	writeError(w, http.StatusNotFound, "no render running with that id")
}

// handleComfyStatus handles GET /api/comfy.
//
// Answers the question the UI actually needs before offering a Render button:
// is ComfyUI configured, and is it answering right now. A button that fails
// after thirty seconds of waiting is worse than one that is not offered.
func (s *Server) handleComfyStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"configured":   false,
		"reachable":    false,
		"placeholders": comfy.Placeholders,
		// The families the settings UI offers a workflow slot for. One graph
		// cannot serve four architectures, so the UI has to know which ones
		// exist to ask about.
		"base_models": basemodel.Known,
	}

	client, err := s.comfyClient()
	if err != nil {
		if !errors.Is(err, comfy.ErrNotConfigured) {
			resp["error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["configured"] = true
	resp["url"] = client.BaseURL

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	version, err := client.Ping(ctx)
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["reachable"] = true
	resp["version"] = version
	writeJSON(w, http.StatusOK, resp)
}

// attachRendered stores a finished render as a manual preview.
//
// `manual` because a picture the user asked this app to make is a picture they
// chose -- it must outrank anything a later enrichment fetches, exactly like an
// upload does.
func (s *Server) attachRendered(sha string, image []byte) (string, error) {
	if !blobstore.IsImage(image) {
		// ComfyUI is a service the operator configured, not a trusted one. The
		// bytes it returns are sniffed like every other image this app admits.
		return "", errors.New("what ComfyUI returned is not a recognised image")
	}
	preview, err := s.storePreview(sha, image, store.SourceManual)
	if err != nil {
		return "", err
	}
	return preview.ImageSHA256, nil
}
