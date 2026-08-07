package api

// Removing a local copy that can be fetched again.
//
// This is the first operation in this program that deletes a model file, and
// everything about its shape is an argument for why that is defensible here and
// nowhere else.
//
// The standing guarantee is that this tool never modifies, moves, renames or
// deletes an existing model file. RemoveRoot goes out of its way to honour it,
// marking paths absent and leaving the disk alone. Eviction narrows the promise
// rather than abandoning it: it deletes a file this daemon wrote, at a
// destination the user chose, which a recorded upstream can hand back. Take away
// any one of those three and the operation is refused.
//
// What survives is the whole point. The model stays in the library with its
// name, tags, previews, provenance and the user's own edits intact; only the
// claim "these bytes are on this disk" goes away. That is the same thing
// RemoveRoot does, arrived at from the other direction.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/socrasteeze/model-manager/internal/evict"
	"github.com/socrasteeze/model-manager/internal/scan"
)

type evictRequest struct {
	// Path names the copy to remove. Required when more than one present,
	// confirmed path exists for this model: a bare hash would be asking the
	// daemon to guess which file to delete, and it does not guess.
	Path string `json:"path,omitempty"`

	// Upstream disambiguates a model pulled from more than one upstream.
	Upstream string `json:"upstream,omitempty"`
}

type evictResponse struct {
	Status     string       `json:"status"`
	Path       string       `json:"path"`
	FreedBytes int64        `json:"freed_bytes"`
	Upstream   string       `json:"upstream"`
	Detail     *ModelDetail `json:"detail,omitempty"`
}

// handleEvict handles POST /api/models/{sha}/evict.
//
// POST rather than DELETE. DELETE on this resource would be a lie: the model
// survives the call, and the spelling is worth reserving for a future "forget
// this model entirely" that really would remove the record.
func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
	// (1) Unlike the file endpoint, this is a mutation and takes the ordinary
	// read-only guard.
	if s.readOnlyGuard(w) {
		return
	}
	// (2) And a permission of its own, separate from --writable. That flag has
	// meant "may add rows, and may create files you asked for"; deleting is a
	// different thing to be allowed, and folding it in would silently grant it
	// to every daemon already running writable.
	if !s.cfg.AllowEvict {
		writeError(w, http.StatusServiceUnavailable,
			"this daemon may not delete model files",
			"restart it with --allow-evict to reclaim space from pulled copies")
		return
	}

	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha")))
	var req evictRequest
	if r.Body != nil {
		// An empty body is the ordinary case -- one copy, one upstream -- so a
		// decode failure on no content is not an error.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}

	// (3) onwards live in internal/evict, because `mm evict` needs the same
	// checks in the same order and a destructive operation with this many
	// preconditions must not have two implementations that can drift.
	res, err := evict.Do(s.cfg.Store, evict.Request{
		SHA256: sha, Path: req.Path, Upstream: req.Upstream,
		HeldBy: s.downloadHolding,
	})
	if err != nil {
		writeEvictError(w, err)
		return
	}

	resp := evictResponse{
		Status: "evicted", Path: res.Path, FreedBytes: res.FreedBytes, Upstream: res.Upstream,
	}
	if res.AlreadyGone {
		resp.Status = "already gone"
	}
	if detail, err := s.modelDetail(sha); err == nil {
		resp.Detail = detail
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeEvictError maps a refusal to a status.
//
// Each refusal is a different problem with a different fix, so they are
// distinguished by sentinel rather than by matching on a message: a caller
// deciding whether to offer a rescan, a re-pull, or nothing at all needs to
// know which one it hit.
func writeEvictError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, evict.ErrUnknownModel):
		writeError(w, http.StatusNotFound, "no model with that hash")
	case errors.Is(err, evict.ErrNotPulled):
		writeError(w, http.StatusConflict,
			"this copy was not pulled from an upstream, so it will not be deleted",
			"eviction is only offered for files this daemon fetched and can fetch again")
	case errors.Is(err, evict.ErrAmbiguous), errors.Is(err, evict.ErrNotThePulledCopy):
		writeError(w, http.StatusConflict, "could not identify which copy to remove", err.Error())
	case errors.Is(err, evict.ErrChanged):
		writeError(w, http.StatusConflict, "the file has changed since it was indexed", err.Error())
	case errors.Is(err, evict.ErrViewLinked):
		writeError(w, http.StatusConflict, "a generated view still links to this model", err.Error())
	case errors.Is(err, evict.ErrDownloadInFlight):
		writeError(w, http.StatusConflict,
			"a download is still transferring this model", err.Error())
	case errors.Is(err, scan.ErrOutsideRoots):
		writeError(w, http.StatusForbidden,
			"that file is not under any enabled model root", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not evict the file", err.Error())
	}
}

// downloadHolding names an unfinished transfer for a hash, or "" if none.
//
// Matched on ExpectedSHA256 and on ActualSHA: the first covers every pull and
// every browse-driven download, the second covers the window between hashing
// and publish, where a job knows what it fetched but the file is not in place
// yet. A job carrying neither cannot be matched and must not be -- refusing an
// eviction on the strength of a transfer that never claimed these bytes would
// be a guess, and this guard exists to replace guessing.
func (s *Server) downloadHolding(sha string) string {
	if s.cfg.Downloads == nil {
		return ""
	}
	for _, j := range s.cfg.Downloads.Jobs() {
		if !j.InFlight() {
			continue
		}
		if strings.EqualFold(j.ExpectedSHA256, sha) || strings.EqualFold(j.ActualSHA, sha) {
			return j.ID
		}
	}
	return ""
}

// handlePulls handles GET /api/pulls.
func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request) {
	pulls, err := s.cfg.Store.ResidentPulls()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list pulled copies", err.Error())
		return
	}
	var total int64
	for _, p := range pulls {
		total += p.SizeBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pulls": pulls,
		// What could be freed right now, which is the number the settings panel
		// exists to show. Views are not subtracted here: whether a given copy is
		// actually evictable is decided per-request, and pre-filtering would
		// mean running that whole check for every row on every poll.
		"reclaimable_bytes": total,
		"evict_available":   s.cfg.AllowEvict && !s.cfg.ReadOnly,
	})
}
