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

	"github.com/socrasteeze/model-manager/internal/archivejob"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/jobrun"
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

	// ServeFiles enables GET /api/models/{sha}/file, which streams the bytes of
	// an indexed model to whoever can reach the API.
	//
	// Opt-in rather than always-on, even though the token is already documented
	// as equivalent to read access to every model file this daemon can see. That
	// documentation describes the off-loopback deployment; the default is
	// loopback with no token at all, where any local process that can open the
	// port would otherwise gain the whole library. An operator who wants other
	// machines to pull from this one is making a deliberate decision, so it is
	// spelled as one.
	ServeFiles bool

	// AllowEvict permits deleting a local copy that was pulled from an upstream.
	//
	// Separate from ReadOnly's inverse on purpose. --writable has always meant
	// "may add rows, and may create files you asked for at destinations you
	// chose", and downloads.go states the matching guarantee: this tool never
	// modifies, moves, renames or deletes an existing model file. Eviction
	// narrows that promise. Narrowing it is a new permission, and attaching it
	// to --writable would grant it retroactively to every daemon already
	// running with that flag.
	AllowEvict bool

	// FreeSpace overrides how the download preflight measures a filesystem.
	// Nil uses diskspace.Avail, so this is a no-op in production; it exists
	// because the only other way to exercise a 507 is to fill a real disk.
	FreeSpace func(dir string) (int64, error)

	// Jobs serializes the sweeps that share one provider rate limit. Nil means
	// no coordination, which is right for a daemon that has no sweeps at all.
	Jobs *jobrun.Group

	// Archives enables deliberate archive intake. Nil leaves it to the CLI.
	Archives *archivejob.Manager

	// AllowArchive permits intake, including on a timer.
	//
	// Separate from ReadOnly's inverse for a different reason than AllowEvict:
	// intake initiates outbound requests on a schedule with nobody present --
	// the daemon acting on its own. Folding that into --writable would grant it
	// retroactively to every daemon already running with the flag.
	AllowArchive bool

	// Transport overrides the round-tripper for every outbound request this
	// package makes. Nil takes net/http's default, so it is a no-op in
	// production.
	//
	// It exists because "every read path answers from local data" is a claim
	// about most of this API and was untestable: the outbound calls here each
	// built an anonymous http.Client inline, so there was no way to observe --
	// let alone forbid -- a request from any of them.
	Transport http.RoundTripper
}

// Server is the HTTP handler.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	handler http.Handler
	started time.Time

	// reachOf records, per route pattern, whether it may leave this machine.
	// Populated by register; read by the offline test, which is the only reason
	// it is kept rather than discarded after registration.
	reachOf map[string]reach

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
	s := &Server{cfg: cfg, mux: http.NewServeMux(), started: time.Now()}

	// Every completion hook is wired below the Server exists, not above it, so
	// each can call a method on s. The render hooks already had to be here for
	// that reason; the download hooks joined them when one of them needed the
	// same thing.
	//
	// A finished download is indexed through the same tier-3 path a scan uses,
	// so it appears in the library without a manual rescan and browse flips it
	// from new to have. Wired here rather than inside the download package so
	// download never imports the store. The root recorded is the job's canonical
	// DestRoot, and a failure lands on the job as IndexError instead of
	// vanishing.
	if cfg.Downloads != nil && cfg.Store != nil {
		// The identity from the transfer's own verification pass is handed
		// straight to the indexer, so a downloaded model is read once rather
		// than twice. The indexer still re-derives everything about the file's
		// location, and falls back to a full read if what it is handed does not
		// describe the file that landed.
		cfg.Downloads.OnComplete = func(j download.Job, res *hashing.Result) string {
			if _, err := scan.IndexPublished(cfg.Store, j.FinalPath, j.DestRoot, res); err != nil {
				return err.Error()
			}
			return ""
		}
		// And a file pulled from an upstream additionally carries that
		// upstream's metadata across. Separate hook, separate error field: this
		// one failing leaves a model that is in the library but thin, which is
		// not what "not indexed yet" means.
		// One hook, dispatching on what the API layer recorded about the job.
		// A transfer is either a pull from an upstream or an archive intake or
		// neither; it cannot be both, because the two set different fields from
		// different requests.
		cfg.Downloads.AfterComplete = func(j download.Job) string {
			if j.ArchiveKey != "" {
				return s.afterArchiveDownload(j)
			}
			return s.afterPull(j)
		}
	}
	// A finished render is attached as a manual preview. Wired here rather than
	// inside the comfy package so that package never imports the store or the
	// blob store: it knows how to talk to ComfyUI and nothing about a library.
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

// reach says whether a route can leave this machine.
//
// The offline contract -- with no route to the internet, every read path answers
// from local data, and every failure names itself as an acquisition failure --
// is a claim about the whole API surface. A claim about the whole surface has to
// be checked against the whole surface, so the classification is an argument to
// register rather than a table kept somewhere else: a table in a comment rots,
// and a route added without deciding is exactly how the contract quietly stops
// being true.
type reach int

const (
	// reachLocal answers entirely from the database, the blob store and the
	// filesystem. Unplugged, it must still succeed, and it must not dial at all.
	reachLocal reach = iota

	// reachOutbound contacts a third party or the upstream while the caller
	// waits. Unplugged, it must fail in a way that names itself: a non-2xx, or a
	// 200 whose body carries the reason in a named field.
	reachOutbound

	// reachDeferred registers work that will dial, and returns before it does.
	// The handler itself touches nothing remote, but the route causes traffic,
	// so calling it local would be a lie -- and the failure surfaces on the job
	// rather than in the response, which is why it cannot be asserted the way an
	// outbound route is.
	reachDeferred
)

// register wires a route and records what it reaches.
func (s *Server) register(pattern string, h http.HandlerFunc, r reach) {
	s.reachOf[pattern] = r
	s.mux.HandleFunc(pattern, h)
}

func (s *Server) routes() {
	// Go 1.22+ pattern routing: method and path variables without a router
	// dependency.
	s.reachOf = map[string]reach{}

	s.register("GET /api/health", s.handleHealth, reachLocal)
	s.register("GET /api/models", s.handleSearch, reachLocal)
	s.register("GET /api/models/{sha}", s.handleModel, reachLocal)
	s.register("PATCH /api/models/{sha}", s.handleUpdateModel, reachLocal)
	s.register("DELETE /api/models/{sha}/fields/{field}", s.handleClearField, reachLocal)
	s.register("GET /api/models/{sha}/candidates", s.handleCandidates, reachLocal)
	// Registered as GET; Go's pattern routing matches HEAD against it too, which
	// is what lets a client probe for size and availability without a transfer.
	s.register("GET /api/models/{sha}/file", s.handleModelFile, reachLocal)
	s.register("POST /api/models/{sha}/evict", s.handleEvict, reachLocal)
	// Outbound: it asks the upstream whether it is up, which is the question.
	s.register("GET /api/upstream", s.handleUpstream, reachOutbound)
	s.register("GET /api/pulls", s.handlePulls, reachLocal)
	s.register("PUT /api/models/{sha}/tags", s.handleSetTags, reachLocal)
	s.register("POST /api/models/{sha}/previews", s.handleUploadPreview, reachLocal)
	s.register("POST /api/models/{sha}/previews/generated", s.handleAttachGenerated, reachLocal)
	s.register("PUT /api/models/{sha}/previews/order", s.handleReorderPreviews, reachLocal)
	s.register("DELETE /api/models/{sha}/previews/{image}", s.handleDeletePreview, reachLocal)
	s.register("GET /api/models/{sha}/previews/{image}/workflow", s.handlePreviewWorkflow, reachLocal)
	s.register("GET /api/generated", s.handleGeneratedImages, reachLocal)
	s.register("POST /api/models/{sha}/previews/render", s.handleRenderPreview, reachDeferred)
	s.register("POST /api/models/{sha}/previews/render/plan", s.handleRenderPlan, reachLocal)
	s.register("GET /api/renders", s.handleListRenders, reachLocal)
	s.register("DELETE /api/renders/{id}", s.handleCancelRender, reachLocal)
	// Outbound: this one pings ComfyUI synchronously, unlike its neighbours.
	s.register("GET /api/comfy", s.handleComfyStatus, reachOutbound)
	s.register("GET /api/comfy/workflows", s.handleListWorkflows, reachLocal)
	s.register("GET /api/comfy/status", s.handleWorkflowStatus, reachLocal)
	s.register("POST /api/comfy/adopt", s.handleAdoptWorkflow, reachLocal)

	s.register("GET /api/models/{sha}/training", s.handleGetTraining, reachLocal)
	s.register("PUT /api/models/{sha}/training", s.handlePutTraining, reachLocal)

	s.register("GET /api/suggestions", s.handleSuggestions, reachLocal)
	s.register("POST /api/suggestions/{id}/accept", s.handleAcceptSuggestion, reachLocal)
	s.register("POST /api/suggestions/{id}/dismiss", s.handleDismissSuggestion, reachLocal)

	// Outbound, and the only mutating one: it asks the provider inline rather
	// than registering a job, because it is one model and the caller is waiting.
	s.register("POST /api/models/{sha}/enrich", s.handleEnrichModel, reachOutbound)
	s.register("POST /api/enrich", s.handleStartEnrich, reachDeferred)
	s.register("GET /api/enrich", s.handleEnrichStatus, reachLocal)
	s.register("DELETE /api/enrich/{id}", s.handleCancelEnrich, reachLocal)

	// Register-and-return, so the route itself dials nothing.
	s.register("POST /api/archive/pull", s.handleArchivePull, reachDeferred)
	s.register("GET /api/archive", s.handleArchiveStatus, reachLocal)
	s.register("DELETE /api/archive/{id}", s.handleCancelArchive, reachLocal)
	s.register("GET /api/archive/items", s.handleArchiveItems, reachLocal)
	s.register("GET /api/archive/watch", s.handleListWatches, reachLocal)
	s.register("POST /api/archive/watch", s.handleAddWatch, reachLocal)
	s.register("DELETE /api/archive/watch/{provider}/{model}", s.handleRemoveWatch, reachLocal)

	s.register("GET /api/browse", s.handleBrowse, reachOutbound)
	s.register("GET /api/updates", s.handleUpdates, reachLocal)
	s.register("POST /api/updates", s.handleStartUpdates, reachDeferred)
	s.register("DELETE /api/updates/{id}", s.handleCancelUpdates, reachLocal)
	s.register("GET /api/remote-image", s.handleRemoteImage, reachOutbound)

	s.register("POST /api/downloads", s.handleCreateDownload, reachDeferred)
	s.register("GET /api/downloads", s.handleListDownloads, reachLocal)
	s.register("DELETE /api/downloads/{id}", s.handleCancelDownload, reachLocal)
	s.register("GET /api/downloads/roots", s.handleDownloadRoots, reachLocal)
	s.register("GET /api/downloads/destination", s.handleResolveDestination, reachLocal)
	s.register("GET /api/downloads/folder-defaults", s.handleFolderDefaults, reachLocal)

	s.register("GET /api/facets", s.handleFacets, reachLocal)
	s.register("GET /api/tags", s.handleTags, reachLocal)
	s.register("GET /api/stats", s.handleStats, reachLocal)
	s.register("GET /api/roots", s.handleListRoots, reachLocal)
	s.register("POST /api/roots", s.handleAddRoot, reachLocal)
	s.register("PATCH /api/roots/{id}", s.handlePatchRoot, reachLocal)
	s.register("DELETE /api/roots/{id}", s.handleRemoveRoot, reachLocal)

	s.register("GET /api/settings", s.handleGetSettings, reachLocal)
	s.register("PUT /api/settings/{key}", s.handlePutSetting, reachLocal)
	s.register("DELETE /api/settings/{key}", s.handleDeleteSetting, reachLocal)

	s.register("GET /api/scans", s.handleScans, reachLocal)
	s.register("POST /api/scans", s.handleStartScan, reachLocal)
	s.register("GET /api/scans/active", s.handleActiveScan, reachLocal)
	s.register("DELETE /api/scans/{id}", s.handleCancelScan, reachLocal)
	s.register("GET /api/detect", s.handleDetect, reachLocal)
	s.register("GET /api/previews/{image}", s.handlePreview, reachLocal)
	s.register("GET /openapi.json", s.handleOpenAPI, reachLocal)

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

// outboundClient builds a client for a request that leaves this machine.
//
// One constructor for every such site in this package, so the test seam cannot
// be half-applied and a new call site cannot quietly escape it.
func (s *Server) outboundClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: s.cfg.Transport}
}

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
