package api

// Managed roots and background scans over HTTP.
//
// Adding a directory is a write, and one with reach: every root is also a legal
// download destination, so the endpoint that adds roots is the endpoint that
// widens where this daemon may write. It is guarded like the download endpoint
// is -- read-only refuses it, the path is canonicalized before it is stored,
// and overlap with an existing root is refused rather than merged.
//
// Removing a root never touches the disk. It forgets the root and marks its
// paths absent; the model records and their metadata stay, so re-adding the
// folder later restores the library instead of re-deriving it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/socrasteeze/model-manager/internal/modeltype"
	"github.com/socrasteeze/model-manager/internal/scanjob"
	"github.com/socrasteeze/model-manager/internal/store"
)

type addRootRequest struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
	Tool  string `json:"tool,omitempty"`

	// Scan requests an immediate scan of the new root. Defaults to true: a root
	// nobody scanned holds nothing as far as the library is concerned, and
	// making the user press a second button to make their folder appear is a
	// step with no decision in it.
	Scan *bool `json:"scan,omitempty"`
}

func (s *Server) handleListRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.cfg.Store.ListRoots()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

func (s *Server) handleAddRoot(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	var req addRootRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	// A root with no tool named gets one inferred from its own contents, so
	// downloads land in the folder layout the directory already uses. Guessing
	// here is safe because the fallback is the root itself, never an invented
	// directory.
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		if canonical, err := store.CanonicalRoot(req.Path); err == nil {
			tool = modeltype.InferTool(canonical, modeltype.DirExists)
		}
	} else if !modeltype.KnownTool(tool) {
		writeError(w, http.StatusBadRequest, "unknown tool", tool)
		return
	}

	root, err := s.cfg.Store.AddRoot(req.Path, req.Label, tool)
	switch {
	case errors.Is(err, store.ErrRootExists):
		writeError(w, http.StatusConflict, "already a model root", err.Error())
		return
	case errors.Is(err, store.ErrRootNested):
		// Not a formatting complaint: overlapping roots make the per-root
		// present-sweep ambiguous, so a file under both flaps between present
		// and absent on every scan. Explaining that is more useful than "400".
		writeError(w, http.StatusConflict, "overlapping model root", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "could not add root", err.Error())
		return
	}

	resp := map[string]any{"root": root}
	if (req.Scan == nil || *req.Scan) && s.cfg.Scans != nil {
		if job, err := s.cfg.Scans.Start([]string{root.Path}); err == nil {
			resp["scan"] = job
		} else if errors.Is(err, scanjob.ErrInFlight) {
			// The root is registered either way; the scan simply waits. Saying
			// so beats failing an add that already succeeded.
			resp["scan_deferred"] = "another scan is already running"
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleRemoveRoot(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid root id")
		return
	}
	if err := s.cfg.Store.RemoveRoot(id); err != nil {
		writeError(w, http.StatusNotFound, "could not remove root", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

type patchRootRequest struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Label   *string `json:"label,omitempty"`
	Tool    *string `json:"tool,omitempty"`
}

func (s *Server) handlePatchRoot(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid root id")
		return
	}
	var req patchRootRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if req.Enabled != nil {
		if err := s.cfg.Store.SetRootEnabled(id, *req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, "update failed", err.Error())
			return
		}
	}
	if req.Tool != nil && *req.Tool != "" && !modeltype.KnownTool(*req.Tool) {
		writeError(w, http.StatusBadRequest, "unknown tool", *req.Tool)
		return
	}
	if req.Label != nil || req.Tool != nil {
		if err := s.cfg.Store.UpdateRootMeta(id, req.Label, req.Tool); err != nil {
			writeError(w, http.StatusInternalServerError, "update failed", err.Error())
			return
		}
	}
	root, err := s.cfg.Store.RootByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such root", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root})
}

// --- scans ------------------------------------------------------------------

type startScanRequest struct {
	// Roots to scan. Empty means every enabled managed root.
	Roots []string `json:"roots,omitempty"`
}

func (s *Server) handleStartScan(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Scans == nil {
		writeError(w, http.StatusServiceUnavailable, "scanning is disabled",
			"the server was started without a scan manager")
		return
	}
	var req startScanRequest
	if r.Body != nil {
		// An empty body is the common case -- "rescan everything" -- so a
		// decode failure on no content is not an error.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}

	// Every root named must already be managed. Otherwise this endpoint would
	// be a way to make the daemon walk and hash any directory on the machine.
	var roots []string
	for _, raw := range req.Roots {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		canonical, err := store.CanonicalRoot(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid root", err.Error())
			return
		}
		if _, err := s.cfg.Store.RootByPath(canonical); err != nil {
			writeError(w, http.StatusForbidden, "not a managed model root", raw)
			return
		}
		roots = append(roots, canonical)
	}

	job, err := s.cfg.Scans.Start(roots)
	if errors.Is(err, scanjob.ErrInFlight) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a scan is already running",
			"scan":  job,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not start scan", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"scan": job})
}

func (s *Server) handleActiveScan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Scans == nil {
		writeJSON(w, http.StatusOK, map[string]any{"scan": nil})
		return
	}
	job, ok := s.cfg.Scans.Current()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"scan": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scan": job})
}

func (s *Server) handleCancelScan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Scans == nil {
		writeError(w, http.StatusServiceUnavailable, "scanning is disabled")
		return
	}
	if s.cfg.Scans.Cancel(r.PathValue("id")) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
		return
	}
	writeError(w, http.StatusNotFound, "no scan running with that id")
}
