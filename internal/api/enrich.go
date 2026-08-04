package api

// Pulling metadata and previews from the origin, on demand.
//
// The merge these endpoints trigger is not new behaviour and deliberately does
// not introduce any: everything fetched is recorded as an ordinary observation
// at the origin tier and resolved by the same rules every other ingest goes
// through (internal/provenance). So a value the user typed still wins and is
// still never overwritten -- a contradicting origin becomes a suggestion instead
// -- a field nobody has filled in gets the best answer available, and a preview
// the user chose cannot be displaced by a fetched one.
//
// What was missing was only the trigger. Enrichment existed solely as `mm
// enrich`, which means the one thing you cannot do from the UI is ask "go and
// see if there is anything better for this model", and that is precisely the
// question you have when looking at a model with a blank base model and no
// thumbnail.
//
// Two shapes, because the two uses are genuinely different:
//
//   - One model, synchronously. A single throttled lookup is under a second, the
//     user is looking straight at the panel, and a job to poll for would be
//     ceremony around a request that has already finished.
//   - Many models, as a background job. A sweep is one request per model against
//     a rate-limited API; holding the connection open for that would tie it to a
//     browser tab.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/origin"
)

// singleEnrichTimeout bounds the synchronous per-model path.
//
// Generous enough for a throttled lookup plus a handful of preview images on a
// slow link, short enough that a provider which accepts the connection and then
// says nothing cannot pin the request open indefinitely.
const singleEnrichTimeout = 90 * time.Second

// enrichPrereqs reports why enrichment is unavailable, or "" if it is.
//
// Checked in one place because three endpoints need the same three answers, and
// a client that gets 404 from one and 503 from another for the same missing
// dependency cannot tell what to fix.
func (s *Server) enrichPrereq() (int, string, string) {
	if s.cfg.Origin == nil {
		return http.StatusServiceUnavailable, "remote lookups are disabled",
			"this daemon was started with --no-remote, so it does not contact Civitai or any other provider"
	}
	if s.cfg.Enrich == nil {
		return http.StatusServiceUnavailable, "enrichment is not available",
			"enrichment writes to the library, so it is offered only on a daemon started with --writable"
	}
	return 0, "", ""
}

// handleEnrichModel handles POST /api/models/{sha}/enrich.
//
// Refresh defaults to true here, unlike a sweep. Pressing a button on one model
// means "go and ask now"; answering it from an archived response recorded weeks
// ago would look like the button did nothing.
func (s *Server) handleEnrichModel(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if status, msg, detail := s.enrichPrereq(); status != 0 {
		writeError(w, status, msg, detail)
		return
	}

	sha := strings.ToLower(r.PathValue("sha"))

	// Existence plus a preview count is all this handler needs before the
	// lookup runs. The full ModelDetail -- seven queries across paths,
	// previews, tags, training and suggestions -- is only genuinely needed
	// once, for the response body after enrichment has actually changed
	// something.
	exists, err := s.cfg.Store.ModelExists(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "no model with that hash", sha)
		return
	}
	previewsBefore, err := s.cfg.Store.PreviewImages(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}

	opts := origin.EnrichOptions{
		Client:     s.cfg.Origin,
		Blobs:      s.cfg.Blobs,
		Targets:    []string{sha},
		Refresh:    queryBoolDefault(r, "refresh", true),
		SkipImages: !queryBoolDefault(r, "images", true),
		MaxImages:  queryInt(r, "max_images", 4),
	}
	if opts.SkipImages {
		opts.Blobs = nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), singleEnrichTimeout)
	defer cancel()

	stats, err := origin.Enrich(ctx, s.cfg.Store, opts)
	if err != nil {
		writeError(w, http.StatusBadGateway, "lookup failed", err.Error())
		return
	}

	// Enrich's eligibility rule skips a model rather than looking it up when
	// Considered comes back 0, and there are two different reasons that can
	// happen: a provisional path (bound by sampled probe, so a lookup could
	// archive another file's metadata here) or a path that simply is not
	// present on disk right now (moved, deleted, an unmounted drive). They
	// need different messages -- "run mm verify" fixes the first and does
	// nothing for the second.
	if stats.Considered == 0 {
		paths, err := s.cfg.Store.PathsFor(sha)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
			return
		}
		var provisional bool
		for _, p := range paths {
			if p.Present && p.Provisional {
				provisional = true
				break
			}
		}
		if provisional {
			writeError(w, http.StatusConflict, "this model's hash is not confirmed",
				"its path was bound by a sampled probe. Run `mm verify --provisional` to "+
					"confirm the hash, then try again.")
		} else {
			writeError(w, http.StatusConflict, "this model is not present on disk",
				"every known path for it is marked absent, so there is nothing to look up right now.")
		}
		return
	}

	after, err := s.modelDetail(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"detail": after,
		"result": enrichResult{
			Found:          stats.Found > 0,
			FromArchive:    stats.CacheHits > 0,
			ImagesAdded:    stats.Images,
			PreviewsBefore: len(previewsBefore),
			PreviewsAfter:  len(after.Previews),
			Errors:         stats.Errors,
		},
	})
}

// enrichResult tells the UI what actually changed, so the button can say so
// rather than leaving the user to diff the panel by eye.
type enrichResult struct {
	Found          bool `json:"found"`
	FromArchive    bool `json:"from_archive"`
	ImagesAdded    int  `json:"images_added"`
	PreviewsBefore int  `json:"previews_before"`
	PreviewsAfter  int  `json:"previews_after"`
	Errors         int  `json:"errors"`
}

// handleStartEnrich handles POST /api/enrich.
//
// Scope is either "all" (the whole library) or "search", in which case the
// request carries the same filter parameters /api/models takes and the sweep
// covers every model matching them -- not just the page on screen. The filters
// are read with searchQueryFrom, the identical function the model list and the
// facet counts use, so "refresh what I am looking at" cannot come to mean a
// different set than what is being looked at.
func (s *Server) handleStartEnrich(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if status, msg, detail := s.enrichPrereq(); status != 0 {
		writeError(w, status, msg, detail)
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}

	opts := enrichjob.Options{
		Refresh:    queryBoolDefault(r, "refresh", false),
		SkipImages: !queryBoolDefault(r, "images", true),
		MaxImages:  queryInt(r, "max_images", 4),
		Limit:      queryInt(r, "limit", 0),
	}

	switch scope {
	case "all":
		// No targets: Enrich walks every model with a confirmed hash.
	case "search":
		q := searchQueryFrom(r)
		// Limit and Offset describe the grid's page, not the intended sweep.
		q.Limit, q.Offset = 0, 0
		targets, err := s.cfg.Store.SearchSHAs(q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search failed", err.Error())
			return
		}
		if len(targets) == 0 {
			writeError(w, http.StatusBadRequest, "no models match those filters")
			return
		}
		opts.Targets = targets
	default:
		writeError(w, http.StatusBadRequest, "unknown scope: "+scope,
			`scope must be "all" or "search"`)
		return
	}

	job, err := s.cfg.Enrich.Start(scope, opts)
	if errors.Is(err, enrichjob.ErrInFlight) {
		// The running job is returned rather than just refused, so a client that
		// lost track of it can pick the existing one back up instead of being
		// told no with nothing to poll.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "an enrichment run is already in progress", "job": job,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start enrichment", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleEnrichStatus handles GET /api/enrich.
func (s *Server) handleEnrichStatus(w http.ResponseWriter, r *http.Request) {
	// Goes through enrichPrereq rather than restating its condition, same
	// reasoning as injectToken's enrichAvailable: the two POST endpoints
	// already enforce both halves of it, and a poller's idea of "available"
	// has to agree with what they will actually do, not just with whether a
	// Manager happens to exist.
	if status, _, _ := s.enrichPrereq(); status != 0 {
		writeJSON(w, http.StatusOK, map[string]any{"job": nil, "available": false})
		return
	}
	job, ok := s.cfg.Enrich.Current()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"job": nil, "available": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job, "available": true})
}

// handleCancelEnrich handles DELETE /api/enrich/{id}.
func (s *Server) handleCancelEnrich(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Enrich == nil || !s.cfg.Enrich.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "no enrichment run to cancel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// queryBoolDefault reads a tri-state query flag with a default for "absent".
//
// queryBool returns nil for absent, which suits a search filter where absent
// means "do not filter". Here absent means "use the default", and the defaults
// differ per endpoint, so the choice has to be made at the call site.
func queryBoolDefault(r *http.Request, key string, fallback bool) bool {
	if v := queryBool(r, key); v != nil {
		return *v
	}
	return fallback
}
