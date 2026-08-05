// Package api serves the HTTP interface.
//
// The API is the third interop tier, alongside the filesystem view and sidecar
// projection, and the thing that makes the library readable from any SD
// front-end (spec §11). It also serves the bundled UI, so the same binary covers
// localhost, the tailnet PWA, and third-party clients.
package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/scanjob"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/updatejob"
)

// Config configures a Server.
type Config struct {
	Store *store.Store
	Blobs *blobstore.Store

	// UI is the embedded front-end. Optional: without it the API still serves.
	UI fs.FS

	Security Security

	// Version is reported by /api/health.
	Version string

	// Downloads enables fetching models. Nil disables the download endpoints
	// entirely, which is the default for a server that has no business writing
	// new files onto the array.
	Downloads *download.Manager

	// Scans enables starting a scan from the UI. Nil leaves scanning to the
	// CLI, which is the right default for a daemon that is only meant to serve
	// an index somebody else built.
	Scans *scanjob.Manager

	// Renders enables rendering a thumbnail with ComfyUI. Nil disables the
	// render endpoints. Not gated on the remote-browsing switch: a ComfyUI
	// address the operator configured is a local service, not a third party.
	Renders *comfy.Manager

	// Enrich enables background metadata/preview sweeps from the UI. Nil leaves
	// enrichment to `mm enrich`. A sweep writes to the library, so it follows the
	// same --writable rule as scanning; it additionally needs Origin, since with
	// no client there is nobody to ask.
	Enrich *enrichjob.Manager

	// Updates enables background update sweeps. Nil leaves update checking to
	// `mm updates`. A sweep writes to the library, so it follows the same
	// --writable rule as enrichment, and needs Origin for the same reason.
	Updates *updatejob.Manager

	// Origin enables remote browsing and update checking. Nil disables both
	// endpoints: those are the only ones that contact a *third party*, so an
	// operator who does not want the daemon talking to Civitai, HuggingFace or
	// CivArchive gets that by leaving this unset rather than by firewall.
	//
	// Renders also makes outbound requests, but to a local ComfyUI the operator
	// configured, which is a different thing and is gated separately.
	Origin *origin.Client

	// ReadOnly refuses every mutating request. Phase 1's contract is that the
	// index is proven before anything acts on it, and this is how that is
	// enforced at the boundary rather than by convention.
	ReadOnly bool
}

// Server is the HTTP handler.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	handler http.Handler
	started time.Time

	// comfyClientMu guards a cached comfy.Client, so a status probe or a
	// render does not build a fresh http.Client -- and abandon the previous
	// one's pooled connections -- on every single call. Rebuilt only when the
	// configured address actually changes.
	comfyClientMu    sync.Mutex
	comfyClientCache *comfy.Client
	comfyClientURL   string
}

// New builds a Server.
func New(cfg Config) *Server {
	// A finished download is indexed through the same tier-3 path a scan
	// uses, so it appears in the library without a manual rescan and browse
	// flips it from new to have. Wired here rather than inside the download
	// package so download never imports the store. The root recorded is the
	// job's canonical DestRoot, and a failure lands on the job as IndexError
	// instead of vanishing.
	if cfg.Downloads != nil && cfg.Store != nil {
		cfg.Downloads.OnComplete = func(j download.Job) string {
			if _, err := scan.IndexFile(cfg.Store, j.FinalPath, j.DestRoot); err != nil {
				return err.Error()
			}
			return ""
		}
	}
	// A finished render is attached as a manual preview. Wired here rather than
	// inside the comfy package so that package never imports the store or the
	// blob store: it knows how to talk to ComfyUI and nothing about a library.
	s := &Server{cfg: cfg, mux: http.NewServeMux(), started: time.Now()}
	if cfg.Renders != nil {
		cfg.Renders.Attach = s.attachRendered
		cfg.Renders.Dial = s.comfyClient
	}
	s.routes()
	s.handler = cfg.Security.middleware(s.mux)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Go 1.22+ pattern routing: method and path variables without a router
	// dependency.
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/models", s.handleSearch)
	s.mux.HandleFunc("GET /api/models/{sha}", s.handleModel)
	s.mux.HandleFunc("PATCH /api/models/{sha}", s.handleUpdateModel)
	s.mux.HandleFunc("DELETE /api/models/{sha}/fields/{field}", s.handleClearField)
	s.mux.HandleFunc("GET /api/models/{sha}/candidates", s.handleCandidates)
	s.mux.HandleFunc("PUT /api/models/{sha}/tags", s.handleSetTags)
	s.mux.HandleFunc("POST /api/models/{sha}/previews", s.handleUploadPreview)
	s.mux.HandleFunc("POST /api/models/{sha}/previews/generated", s.handleAttachGenerated)
	s.mux.HandleFunc("PUT /api/models/{sha}/previews/order", s.handleReorderPreviews)
	s.mux.HandleFunc("DELETE /api/models/{sha}/previews/{image}", s.handleDeletePreview)
	s.mux.HandleFunc("GET /api/models/{sha}/previews/{image}/workflow", s.handlePreviewWorkflow)
	s.mux.HandleFunc("GET /api/generated", s.handleGeneratedImages)
	s.mux.HandleFunc("POST /api/models/{sha}/previews/render", s.handleRenderPreview)
	s.mux.HandleFunc("POST /api/models/{sha}/previews/render/plan", s.handleRenderPlan)
	s.mux.HandleFunc("GET /api/renders", s.handleListRenders)
	s.mux.HandleFunc("DELETE /api/renders/{id}", s.handleCancelRender)
	s.mux.HandleFunc("GET /api/comfy", s.handleComfyStatus)
	s.mux.HandleFunc("GET /api/comfy/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /api/comfy/status", s.handleWorkflowStatus)
	s.mux.HandleFunc("POST /api/comfy/adopt", s.handleAdoptWorkflow)

	s.mux.HandleFunc("GET /api/models/{sha}/training", s.handleGetTraining)
	s.mux.HandleFunc("PUT /api/models/{sha}/training", s.handlePutTraining)

	s.mux.HandleFunc("GET /api/suggestions", s.handleSuggestions)
	s.mux.HandleFunc("POST /api/suggestions/{id}/accept", s.handleAcceptSuggestion)
	s.mux.HandleFunc("POST /api/suggestions/{id}/dismiss", s.handleDismissSuggestion)

	s.mux.HandleFunc("POST /api/models/{sha}/enrich", s.handleEnrichModel)
	s.mux.HandleFunc("POST /api/enrich", s.handleStartEnrich)
	s.mux.HandleFunc("GET /api/enrich", s.handleEnrichStatus)
	s.mux.HandleFunc("DELETE /api/enrich/{id}", s.handleCancelEnrich)

	s.mux.HandleFunc("GET /api/browse", s.handleBrowse)
	s.mux.HandleFunc("GET /api/updates", s.handleUpdates)
	s.mux.HandleFunc("POST /api/updates", s.handleStartUpdates)
	s.mux.HandleFunc("DELETE /api/updates/{id}", s.handleCancelUpdates)
	s.mux.HandleFunc("GET /api/remote-image", s.handleRemoteImage)

	s.mux.HandleFunc("POST /api/downloads", s.handleCreateDownload)
	s.mux.HandleFunc("GET /api/downloads", s.handleListDownloads)
	s.mux.HandleFunc("DELETE /api/downloads/{id}", s.handleCancelDownload)
	s.mux.HandleFunc("GET /api/downloads/roots", s.handleDownloadRoots)
	s.mux.HandleFunc("GET /api/downloads/destination", s.handleResolveDestination)
	s.mux.HandleFunc("GET /api/downloads/folder-defaults", s.handleFolderDefaults)

	s.mux.HandleFunc("GET /api/facets", s.handleFacets)
	s.mux.HandleFunc("GET /api/tags", s.handleTags)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/roots", s.handleListRoots)
	s.mux.HandleFunc("POST /api/roots", s.handleAddRoot)
	s.mux.HandleFunc("PATCH /api/roots/{id}", s.handlePatchRoot)
	s.mux.HandleFunc("DELETE /api/roots/{id}", s.handleRemoveRoot)

	s.mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	s.mux.HandleFunc("PUT /api/settings/{key}", s.handlePutSetting)
	s.mux.HandleFunc("DELETE /api/settings/{key}", s.handleDeleteSetting)

	s.mux.HandleFunc("GET /api/scans", s.handleScans)
	s.mux.HandleFunc("POST /api/scans", s.handleStartScan)
	s.mux.HandleFunc("GET /api/scans/active", s.handleActiveScan)
	s.mux.HandleFunc("DELETE /api/scans/{id}", s.handleCancelScan)
	s.mux.HandleFunc("GET /api/detect", s.handleDetect)
	s.mux.HandleFunc("GET /api/previews/{image}", s.handlePreview)
	s.mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	if s.cfg.UI != nil {
		s.mux.Handle("GET /", s.uiHandler())
	} else {
		s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"service": "model-manager",
				"api":     "/api/health",
				"openapi": "/openapi.json",
				"note":    "no UI is embedded in this build",
			})
		})
	}
}

// --- helpers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(body); err != nil {
		// The status line is already written, so this can only be logged, not
		// turned into an error response.
		_ = err
	}
}

type apiError struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg string, detail ...string) {
	e := apiError{Error: msg}
	if len(detail) > 0 {
		e.Detail = detail[0]
	}
	writeJSON(w, status, e)
}

// readOnlyGuard rejects mutations when the server is in read-only mode.
func (s *Server) readOnlyGuard(w http.ResponseWriter) bool {
	if !s.cfg.ReadOnly {
		return false
	}
	writeError(w, http.StatusForbidden, "server is read-only",
		"Phase 1 proves the index before anything is allowed to act on it. "+
			"Start with --writable to enable editing.")
	return true
}

func queryBool(r *http.Request, key string) *bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil
	}
	b := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	return &b
}

func queryList(r *http.Request, key string) []string {
	var out []string
	for _, raw := range r.URL.Query()[key] {
		for _, part := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(part); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

// ListenAddr describes where a Server should bind.
type ListenAddr struct {
	Host string
	Port int
}

func (l ListenAddr) String() string {
	return net.JoinHostPort(l.Host, fmt.Sprintf("%d", l.Port))
}

// IsLoopback reports whether the bind address is loopback-only, which is what
// decides whether a token is mandatory.
func (l ListenAddr) IsLoopback() bool {
	if l.Host == "" {
		// An empty host means every interface.
		return false
	}
	if l.Host == "localhost" {
		return true
	}
	ip := net.ParseIP(l.Host)
	return ip != nil && ip.IsLoopback()
}
