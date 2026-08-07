package api

// Archive intake over HTTP.
//
// Register-and-return, like POST /api/enrich rather than the per-model enrich
// button: an intake is a metadata fetch, up to a dozen preview downloads and a
// handover to the download queue, which is not something to hold a request open
// for. That also keeps the route local for the offline contract -- it registers
// work and dials nothing itself.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/socrasteeze/model-manager/internal/archivejob"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// jobArchive is this member's key in the shared-throttle group.
const jobArchive = "archive"

// archivePrereq reports why intake is unavailable, or 0 if it is not.
//
// The same shape as enrichPrereq and updatePrereq, in one place, so a client
// never gets 404 from one endpoint and 503 from another for the same missing
// dependency.
func (s *Server) archivePrereq() (int, string, string) {
	if s.cfg.Origin == nil {
		return http.StatusServiceUnavailable, "remote lookups are disabled",
			"this daemon was started with --no-remote, so it does not contact any provider"
	}
	if !s.cfg.AllowArchive || s.cfg.Archives == nil {
		return http.StatusServiceUnavailable, "archive intake is not available",
			"intake fetches from a provider and writes files, and does so on a timer; " +
				"start the daemon with --writable --allow-archive to enable it"
	}
	return s.sharedThrottleFree(jobArchive)
}

type archivePullRequest struct {
	Provider  string `json:"provider"`
	ModelID   string `json:"model_id"`
	VersionID string `json:"version_id,omitempty"`

	// Where the file lands. Resolved through exactly the same containment check
	// a browse-driven download uses -- an intake that could write outside a
	// scanned root would be the primitive downloads.go exists to deny.
	DestRoot string `json:"dest_root,omitempty"`
	Subdir   string `json:"subdir,omitempty"`
	Type     string `json:"type,omitempty"`

	Watch        bool `json:"watch,omitempty"`
	Force        bool `json:"force,omitempty"`
	PreviewLimit int  `json:"preview_limit,omitempty"`
}

// handleArchivePull handles POST /api/archive/pull.
func (s *Server) handleArchivePull(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if status, msg, detail := s.archivePrereq(); status != 0 {
		writeError(w, status, msg, detail)
		return
	}

	var req archivePullRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if strings.TrimSpace(req.ModelID) == "" {
		writeError(w, http.StatusBadRequest, "no model id given")
		return
	}
	if req.Provider == "" {
		req.Provider = origin.ProviderCivitaiID
	}

	job, err := s.cfg.Archives.Start(archivejob.Options{
		Targets: []archivejob.Target{{
			Provider: req.Provider, ModelID: req.ModelID,
			VersionID: req.VersionID, Watch: req.Watch,
		}},
		PreviewLimit:  req.PreviewLimit,
		Force:         req.Force,
		StartDownload: s.archiveDownloadStarter(req),
	})
	if err != nil {
		if errors.Is(err, archivejob.ErrInFlight) {
			// The running job comes back with the conflict, so a client that
			// lost track of it can re-attach rather than being told only "no".
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "an archive run is already in progress", "job": job})
			return
		}
		writeError(w, http.StatusInternalServerError, "could not start the archive run", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// archiveDownloadStarter hands the model file to the download queue.
//
// Returns nil when downloads are unavailable, which the job treats as "archive
// the metadata and previews only". That is the honest fallback rather than a
// refusal: metadata is what disappears first when a model is taken down, and
// capturing it on a daemon that cannot store files is still worth doing.
func (s *Server) archiveDownloadStarter(req archivePullRequest) func(archivejob.Target, origin.RemoteFile) error {
	if s.cfg.Downloads == nil {
		return nil
	}
	return func(t archivejob.Target, file origin.RemoteFile) error {
		target := strings.TrimSpace(file.DownloadURL)
		if target == "" {
			return errors.New("no download url")
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("the provider gave an unusable download url")
		}
		// The same allowlist a browse-driven download goes through. An intake is
		// operator-initiated, but the URL inside it came from a provider
		// response, which is not a reason to trust it further.
		if !s.downloadHostAllowed(u.Host) {
			return errors.New("download host not allowed: " + u.Host)
		}

		destRoot := strings.TrimSpace(req.DestRoot)
		if destRoot == "" {
			if def, err := s.defaultDownloadRoot(); err == nil {
				destRoot = def
			}
		}
		subdir := req.Subdir
		if strings.TrimSpace(subdir) == "" {
			if canonical, err := store.CanonicalRoot(destRoot); err == nil {
				subdir = s.subfolderFor(canonical, firstNonBlank(req.Type, t.Provider))
			}
		}
		destDir, matchedRoot, err := s.resolveDestination(destRoot, subdir)
		if err != nil {
			return err
		}
		if short := s.spaceShortfall(destDir, file.SizeBytes); short != nil {
			return short
		}

		// Detached from any request, exactly as handleCreateDownload does: a
		// multi-gigabyte transfer must not end because the caller went away.
		_, err = s.cfg.Downloads.Start(context.Background(), download.Job{
			URL:            u.String(),
			DestDir:        destDir,
			DestRoot:       matchedRoot,
			Filename:       sanitizeRequestedName(file.Name),
			ExpectedSHA256: strings.ToLower(file.SHA256),
			ExpectedSize:   file.SizeBytes,
			// Set here, from what this request already knows, so the completion
			// hook does not have to work out what kind of transfer this was by
			// re-parsing a URL.
			ArchiveKey: archiveKey(t.Provider, t.ModelID, t.VersionID),
		})
		if errors.Is(err, download.ErrInFlight) {
			// Already transferring is not a failure of the intake.
			return nil
		}
		return err
	}
}

// ArchiveDownloadStarter exposes the download hand-off to the scheduler.
//
// The scheduler has no request to take a destination from, so this is the
// default-destination form of the same function the endpoint uses -- which
// matters: an automatic intake must go through exactly the containment and
// free-space checks a human-initiated one does, not a shortcut around them.
func (s *Server) ArchiveDownloadStarter() func(archivejob.Target, origin.RemoteFile) error {
	return s.archiveDownloadStarter(archivePullRequest{})
}

// handleArchiveStatus handles GET /api/archive.
func (s *Server) handleArchiveStatus(w http.ResponseWriter, r *http.Request) {
	status, msg, _ := s.archivePrereq()
	body := map[string]any{"available": status == 0}
	if msg != "" {
		body["unavailable_because"] = msg
	}
	if s.cfg.Archives != nil {
		if job, ok := s.cfg.Archives.Current(); ok {
			body["job"] = job
		}
	}
	// Counts are read from the database, so they answer even on a daemon that
	// cannot start a run -- the same rule GET /api/updates follows: hide the
	// button, not the data.
	if counts, err := s.cfg.Store.ArchiveSummary(); err == nil {
		body["counts"] = counts
	}
	writeJSON(w, http.StatusOK, body)
}

// handleCancelArchive handles DELETE /api/archive/{id}.
func (s *Server) handleCancelArchive(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Archives == nil || !s.cfg.Archives.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "no such archive run is in progress")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleArchiveItems handles GET /api/archive/items.
func (s *Server) handleArchiveItems(w http.ResponseWriter, r *http.Request) {
	items, err := s.cfg.Store.ArchiveItems(store.ArchiveItemsQuery{
		Incomplete: r.URL.Query().Get("incomplete") == "true",
		Gone:       r.URL.Query().Get("gone") == "true",
		Limit:      queryInt(r, "limit", 0),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list the archive", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleListWatches handles GET /api/archive/watch.
func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	watches, err := s.cfg.Store.ArchiveWatches(queryInt(r, "limit", 0))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list the watchlist", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": watches})
}

// handleAddWatch handles POST /api/archive/watch.
func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	var req store.ArchiveWatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if strings.TrimSpace(req.ModelID) == "" {
		writeError(w, http.StatusBadRequest, "no model id given")
		return
	}
	if req.Provider == "" {
		req.Provider = origin.ProviderCivitaiID
	}
	if err := s.cfg.Store.PutArchiveWatch(req); err != nil {
		writeError(w, http.StatusInternalServerError, "could not watch that model", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "watching"})
}

// handleRemoveWatch handles DELETE /api/archive/watch/{provider}/{model}.
func (s *Server) handleRemoveWatch(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if err := s.cfg.Store.RemoveArchiveWatch(r.PathValue("provider"), r.PathValue("model")); err != nil {
		writeError(w, http.StatusInternalServerError, "could not stop watching", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// afterArchiveDownload finishes an intake once its file has landed.
//
// This is the first moment a model_file row exists, so it is the first moment
// the two things that key on one can be written: the origin identity, and the
// staged previews. Both were deliberately deferred to here rather than skipped.
func (s *Server) afterArchiveDownload(j download.Job) string {
	provider, modelID, versionID, ok := parseArchiveKey(j.ArchiveKey)
	if !ok || j.ActualSHA == "" {
		return ""
	}
	sha := strings.ToLower(j.ActualSHA)

	if err := s.cfg.Store.SetArchiveFile(provider, modelID, versionID, sha); err != nil {
		return "recorded the file but not against its archive entry: " + err.Error()
	}

	var problems []string

	// "I asked for this exact version deliberately", which is what a download
	// means -- not the weaker archive-derived inference.
	if err := s.cfg.Store.PutModelOrigin(store.ModelOrigin{
		SHA256: sha, Provider: provider, ModelID: modelID, VersionID: versionID,
		Source: store.OriginSourceDownload,
	}); err != nil {
		problems = append(problems, "origin identity: "+err.Error())
	}

	// The field observations, replayed out of the body archived during intake.
	//
	// This is the step that could not happen earlier: field_value and model_tag
	// key on model_file, which did not exist until this transfer landed. Without
	// it the metadata is captured and never applied, and the model appears in
	// the library as a bare hash with a filename -- everything fetched, nothing
	// showing.
	if err := s.applyArchivedMetadata(provider, modelID, versionID, sha); err != nil {
		problems = append(problems, "metadata: "+err.Error())
	}

	if s.cfg.Blobs != nil {
		if _, err := archivejob.AttachStagedPreviews(s.cfg.Store, s.cfg.Blobs,
			provider, modelID, versionID, sha,
			func(sha string, data []byte, source string, position int) error {
				_, err := s.storePreviewAt(sha, data, source, position)
				return err
			}); err != nil {
			problems = append(problems, "previews: "+err.Error())
		}
	}

	if len(problems) > 0 {
		return strings.Join(problems, "; ")
	}
	return ""
}

// applyArchivedMetadata turns the archived response into local field values.
//
// Read back out of origin_cache rather than carried in memory from the intake,
// and that is deliberate: the intake and this hook are separated by a transfer
// that can take an hour and can outlive a restart. The archive is the durable
// hand-off between them, which is the property it was built for.
func (s *Server) applyArchivedMetadata(provider, modelID, versionID, sha string) error {
	if provider != origin.ProviderCivitaiID {
		return nil
	}
	cache := origin.NewCache(s.cfg.Store)
	entry, ok, err := cache.Get(origin.ProviderCivitaiVersionID, versionID)
	if err != nil {
		return err
	}
	if !ok || entry == nil || len(entry.Raw) == 0 {
		// Nothing archived: the intake never got that far, and the item's own
		// completeness flags already say so.
		return nil
	}

	obs, tags, hashes, _ := origin.ObservationsFromCivitai(entry.Raw, sha)
	if len(obs) > 0 {
		// Under the provider's own source name, so the tier lands at origin and
		// a value the user later types still outranks it.
		if err := s.cfg.Store.RecordObservations(sha, provenance.SourceCivitai, obs); err != nil {
			return err
		}
	}
	if len(tags) > 0 {
		if err := s.cfg.Store.SetTags(sha, provenance.SourceCivitai, tags); err != nil {
			return err
		}
	}
	if len(hashes) > 0 {
		if err := cache.PutHashes(sha, origin.ProviderCivitai, hashes); err != nil {
			return err
		}
	}
	// Materialise, or the model shows in the library as a bare hash until
	// something else happens to resolve it.
	if _, err := s.cfg.Store.ResolveModel(sha); err != nil {
		return err
	}
	return nil
}

// archiveKey and parseArchiveKey carry the intake's identity on the download
// job. A single string because Job is a flat record the API serializes, and
// three fields there would be three fields every other kind of download leaves
// empty.
func archiveKey(provider, modelID, versionID string) string {
	return provider + "\x1f" + modelID + "\x1f" + versionID
}

func parseArchiveKey(key string) (provider, modelID, versionID string, ok bool) {
	parts := strings.Split(key, "\x1f")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
