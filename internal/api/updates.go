package api

// Update checking over HTTP.
//
// GET reads what the last sweep recorded. POST starts a new one. That split is
// the point of the feature: the old GET performed the whole check inline --
// one upstream request per owned model, up to a 30-minute budget, with the
// result thrown away when the tab closed -- so the answer to "what needs
// updating" existed only for as long as you were looking at it, and could not
// be filtered on or badged in the library.

import (
	"errors"
	"net/http"
	"time"

	"github.com/socrasteeze/model-manager/internal/updatejob"
)

// updatePrereq reports why starting a sweep is unavailable, or 0 if it is not.
//
// Deliberately not applied to the GET. A --no-remote daemon contacts nobody,
// but it can still show what a previous sweep recorded -- that is the entire
// point of persisting it. The GET reports availability in its payload instead,
// so the UI can hide the button without hiding the data.
func (s *Server) updatePrereq() (int, string, string) {
	if s.cfg.Origin == nil {
		return http.StatusServiceUnavailable, "remote lookups are disabled",
			"this daemon was started with --no-remote, so it does not contact Civitai or any other provider"
	}
	if s.cfg.Updates == nil {
		return http.StatusServiceUnavailable, "update checking is not available",
			"an update check writes to the library, so it is offered only on a daemon started with --writable"
	}
	// Both sweeps spend the same shared origin.Client throttle. jobrun only
	// enforces at-most-one within a Runner, so without this they would each be
	// politely paced and together double the request rate against the very API
	// the throttle exists to placate -- the failure enrichjob's package comment
	// warns about.
	if s.cfg.Enrich != nil {
		if _, running := s.cfg.Enrich.InFlight(); running {
			return http.StatusConflict, "an enrichment run is already in progress",
				"both talk to the same provider on one shared rate limit; wait for it to finish"
		}
	}
	return 0, "", ""
}

// handleUpdates handles GET /api/updates.
//
// Reads stored data. No network, no timeout, no single-flight latch: the old
// handler needed all three because a GET performed the sweep.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	updates, err := s.cfg.Store.PendingUpdates(queryInt(r, "limit", 0))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}

	body := map[string]any{
		"updates": updates,
		// Whether a sweep could be started now, as opposed to whether there is
		// anything to show.
		"available": func() bool { status, _, _ := s.updatePrereq(); return status == 0 }(),
	}
	if s.cfg.Updates != nil {
		if job, ok := s.cfg.Updates.Current(); ok {
			body["job"] = job
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// handleStartUpdates handles POST /api/updates.
func (s *Server) handleStartUpdates(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if status, msg, detail := s.updatePrereq(); status != 0 {
		writeError(w, status, msg, detail)
		return
	}

	opts := updatejob.Options{Limit: queryInt(r, "limit", 0)}
	// Hours, not a duration string: the only caller is a button, and the one
	// question it needs to answer is "skip what was checked recently".
	if h := queryInt(r, "max_age_hours", 0); h > 0 {
		opts.MaxAge = time.Duration(h) * time.Hour
	}

	job, err := s.cfg.Updates.Start(opts)
	if errors.Is(err, updatejob.ErrInFlight) {
		// The running job is returned rather than just refused, so a client
		// that lost track of it can pick it back up instead of being told no
		// with nothing to poll.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "an update check is already in progress", "job": job,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the update check", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleCancelUpdates handles DELETE /api/updates/{id}.
func (s *Server) handleCancelUpdates(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Updates == nil || !s.cfg.Updates.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "no update check to cancel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
