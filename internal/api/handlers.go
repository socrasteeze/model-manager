package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/detect"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/report"
	"github.com/socrasteeze/model-manager/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version, err := s.cfg.Store.SchemaVersion()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.cfg.Version,
		"schema_version": version,
		"database":       s.cfg.Store.Path(),
		"read_only":      s.cfg.ReadOnly,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := store.SearchQuery{
		Text:           r.URL.Query().Get("q"),
		Types:          queryList(r, "type"),
		BaseModels:     queryList(r, "base_model"),
		Tags:           queryList(r, "tag"),
		Origins:        queryList(r, "origin"),
		Formats:        queryList(r, "format"),
		HasPreview:     queryBool(r, "has_preview"),
		NSFW:           queryBool(r, "nsfw"),
		Present:        queryBool(r, "present"),
		NeedsAttention: queryBool(r, "needs_attention") != nil && *queryBool(r, "needs_attention"),
		Sort:           r.URL.Query().Get("sort"),
		Desc:           r.URL.Query().Get("order") == "desc",
		Limit:          queryInt(r, "limit", 50),
		Offset:         queryInt(r, "offset", 0),
	}

	results, err := s.cfg.Store.Search(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// ModelDetail is everything known about one model.
type ModelDetail struct {
	SHA256        string                `json:"sha256"`
	WeightsSHA256 string                `json:"weights_sha256,omitempty"`
	Size          int64                 `json:"size"`
	Format        string                `json:"format"`
	FirstSeen     string                `json:"first_seen"`
	LastVerified  string                `json:"last_verified"`
	Record        *store.ModelRecord    `json:"record,omitempty"`
	Paths         []store.FilePath      `json:"paths"`
	Previews      []store.PreviewImage  `json:"previews"`
	Tags          []string              `json:"tags"`
	Training      *store.TrainingRecord `json:"training,omitempty"`
	Suggestions   []store.Suggestion    `json:"suggestions"`

	// HeaderTruncated tells the UI why a training record might be thin.
	HeaderTruncated bool `json:"header_truncated,omitempty"`
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	detail, err := s.modelDetail(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "no model with that hash")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) modelDetail(sha string) (*ModelDetail, error) {
	st := s.cfg.Store

	var d ModelDetail
	var weights *string
	var truncated int
	err := st.DB().QueryRow(`
        SELECT sha256, weights_sha256, size, format, first_seen, last_verified, header_truncated
          FROM model_file WHERE sha256 = ?`, sha,
	).Scan(&d.SHA256, &weights, &d.Size, &d.Format, &d.FirstSeen, &d.LastVerified, &truncated)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	if weights != nil {
		d.WeightsSHA256 = *weights
	}
	d.HeaderTruncated = truncated == 1

	if d.Record, err = st.GetModelRecord(sha); err != nil {
		return nil, err
	}
	if d.Paths, err = st.PathsFor(sha); err != nil {
		return nil, err
	}
	if d.Previews, err = st.PreviewImages(sha); err != nil {
		return nil, err
	}
	if d.Tags, err = st.Tags(sha); err != nil {
		return nil, err
	}
	if d.Training, err = st.GetTrainingRecord(sha); err != nil {
		return nil, err
	}
	if d.Suggestions, err = st.PendingSuggestions(sha, 0); err != nil {
		return nil, err
	}
	return &d, nil
}

// CandidateView exposes the provenance behind one field, so the UI can show what
// else was on offer rather than presenting the winner as the only opinion.
type CandidateView struct {
	Field  string           `json:"field"`
	Winner candidateEntry   `json:"winner"`
	Losers []candidateEntry `json:"losers,omitempty"`
}

type candidateEntry struct {
	Value      json.RawMessage `json:"value"`
	Source     string          `json:"source"`
	Tier       int             `json:"tier"`
	TierName   string          `json:"tier_name"`
	ObservedAt string          `json:"observed_at"`
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	candidates, err := s.cfg.Store.Candidates(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}

	byField := map[string][]provenance.Candidate{}
	for _, c := range candidates {
		byField[c.Field] = append(byField[c.Field], c)
	}

	out := []CandidateView{}
	for _, field := range provenance.Fields {
		list, ok := byField[field]
		if !ok {
			continue
		}
		res, ok := provenance.Resolve(list)
		if !ok {
			continue
		}
		view := CandidateView{Field: field, Winner: toEntry(res.Value, res.Source, res.Tier, list)}
		for _, l := range res.Losers {
			view.Losers = append(view.Losers, toEntry(l.Value, l.Source, l.Tier, list))
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func toEntry(value, source string, tier provenance.Tier, all []provenance.Candidate) candidateEntry {
	e := candidateEntry{
		Value:    json.RawMessage(value),
		Source:   source,
		Tier:     int(tier),
		TierName: tierName(tier),
	}
	for _, c := range all {
		if c.Source == source {
			e.ObservedAt = c.ObservedAt.UTC().Format(time.RFC3339)
			break
		}
	}
	return e
}

func tierName(t provenance.Tier) string {
	switch t {
	case provenance.TierManual:
		return "manual"
	case provenance.TierOrigin:
		return "origin"
	default:
		return "tool"
	}
}

// updateRequest is a manual edit. A field present with a null value is an
// explicit clear, which is a different intention from omitting it -- and from
// setting it to an empty string, which is a legitimate value to want (§7.1).
type updateRequest map[string]*json.RawMessage

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	sha := r.PathValue("sha")

	var req updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}

	allowed := map[string]bool{}
	for _, f := range provenance.Fields {
		allowed[f] = true
	}

	var observations []store.FieldObservation
	var cleared []string
	for field, raw := range req {
		if !allowed[field] {
			writeError(w, http.StatusBadRequest, "unknown field: "+field)
			return
		}
		if raw == nil {
			cleared = append(cleared, field)
			continue
		}
		var value any
		if err := json.Unmarshal(*raw, &value); err != nil {
			writeError(w, http.StatusBadRequest, "invalid value for "+field, err.Error())
			return
		}
		observations = append(observations, store.FieldObservation{Field: field, Value: value})
	}

	for _, field := range cleared {
		if err := s.cfg.Store.ClearManualField(sha, field); err != nil {
			writeError(w, http.StatusInternalServerError, "clear failed", err.Error())
			return
		}
	}
	if len(observations) > 0 {
		if err := s.cfg.Store.RecordObservations(sha, provenance.SourceManual, observations); err != nil {
			writeError(w, http.StatusInternalServerError, "update failed", err.Error())
			return
		}
	}

	rec, err := s.cfg.Store.ResolveModel(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleClearField(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	sha, field := r.PathValue("sha"), r.PathValue("field")

	if err := s.cfg.Store.ClearManualField(sha, field); err != nil {
		writeError(w, http.StatusInternalServerError, "clear failed", err.Error())
		return
	}
	rec, err := s.cfg.Store.ResolveModel(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleSetTags(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	sha := r.PathValue("sha")

	var tags []string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&tags); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON array of tag names", err.Error())
		return
	}
	// Manual tags are stored under their own source, so re-running an ingest
	// cannot delete a tag the user added by hand.
	if err := s.cfg.Store.SetTags(sha, provenance.SourceManual, tags); err != nil {
		writeError(w, http.StatusInternalServerError, "tagging failed", err.Error())
		return
	}
	if _, err := s.cfg.Store.ResolveModel(sha); err != nil {
		writeError(w, http.StatusInternalServerError, "resolve failed", err.Error())
		return
	}
	current, err := s.cfg.Store.Tags(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, current)
}

func (s *Server) handleGetTraining(w http.ResponseWriter, r *http.Request) {
	tr, err := s.cfg.Store.GetTrainingRecord(r.PathValue("sha"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	if tr == nil {
		writeError(w, http.StatusNotFound, "no training record")
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (s *Server) handlePutTraining(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	var tr store.TrainingRecord
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&tr); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	tr.SHA256 = r.PathValue("sha")
	// Anything arriving through this endpoint is the user typing, whatever the
	// body claims -- otherwise a client could write a record that a later header
	// pass would then feel free to overwrite.
	tr.Source = "manual"

	if err := s.cfg.Store.UpsertTrainingRecord(tr); err != nil {
		writeError(w, http.StatusInternalServerError, "save failed", err.Error())
		return
	}
	saved, err := s.cfg.Store.GetTrainingRecord(tr.SHA256)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.PendingSuggestions(r.URL.Query().Get("sha256"), queryInt(r, "limit", 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid suggestion id")
		return
	}
	if err := s.cfg.Store.AcceptSuggestion(id); err != nil {
		writeError(w, http.StatusInternalServerError, "accept failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDismissSuggestion(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid suggestion id")
		return
	}
	if err := s.cfg.Store.DismissSuggestion(id); err != nil {
		writeError(w, http.StatusInternalServerError, "dismiss failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	facets, err := s.cfg.Store.FacetCounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "facets failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, facets)
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.cfg.Store.AllTags()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	rep, err := report.Generate(s.cfg.Store.DB(), s.cfg.Store.Path(), queryInt(r, "top", 20))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "report failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleScans(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cfg.Store.RecentScanRuns(queryInt(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	installs := detect.Detect()
	writeJSON(w, http.StatusOK, map[string]any{
		"installs":    installs,
		"model_roots": detect.ModelRoots(installs),
	})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Blobs == nil {
		writeError(w, http.StatusNotFound, "no preview store configured")
		return
	}
	image := r.PathValue("image")

	data, err := s.cfg.Blobs.Read(image)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such preview")
		return
	}

	// Serve the sniffed type rather than anything the database recorded. A
	// stored MIME could have come from a sidecar, and a preview that renders as
	// HTML is an XSS vector in the UI's own origin.
	mime := blobstoreMIME(data)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", "inline")
	// Content-addressed, so the bytes behind this URL can never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, _ = w.Write(data)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec(s.cfg.Version))
}
