package api

// User-chosen thumbnails.
//
// Fetched previews were already sticky: enrichment copies the bytes into the
// content-addressed blob store, so a Civitai takedown cannot reach back and
// blank a local thumbnail. What was missing is the other half -- a picture the
// *user* chose, and a guarantee that the next enrichment run cannot displace
// it.
//
// That guarantee is the same tiering the field provenance already uses: a
// `manual` preview outranks every fetched one, and re-ingesting the same bytes
// from a provider cannot demote it (see AddPreviewImage's upsert).
//
// A ComfyUI PNG additionally carries the graph that produced it in a text
// chunk. That gets pulled out and stored beside the image, because the picture
// and the recipe arriving as one file is exactly what makes "attach my own
// thumbnail" worth more than a file picker.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/thumb"
)

// maxUploadBytes caps an uploaded preview. Same limit the blob store enforces,
// checked here first so a 200 MB body is refused at the door rather than after
// it has been read into memory.
const maxUploadBytes = blobstore.MaxBlobBytes

// handleUploadPreview handles POST /api/models/{sha}/previews.
//
// The body is the raw image. Content-Type is ignored: the type is sniffed from
// the bytes, because a filename or a header is a claim by the uploader and this
// image gets served back from the UI's own origin, where an HTML file that
// renders would be XSS.
func (s *Server) handleUploadPreview(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "no preview store configured")
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))
	if detail, err := s.modelDetail(sha); err != nil || detail == nil {
		writeError(w, http.StatusNotFound, "no such model", sha)
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "upload too large", err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty upload")
		return
	}
	if !blobstore.IsImage(data) {
		writeError(w, http.StatusUnsupportedMediaType, "not an image",
			"the uploaded bytes are not a recognised image format")
		return
	}

	preview, err := s.storePreview(sha, data, store.SourceManual)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store preview", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"preview": preview})
}

// storePreview puts the image (and its derived thumbnail and any embedded
// ComfyUI workflow) in the blob store and attaches it to a model.
func (s *Server) storePreview(sha string, data []byte, source string) (*store.PreviewImage, error) {
	blob, err := s.cfg.Blobs.Put(data)
	if err != nil {
		return nil, err
	}

	p := store.PreviewImage{
		SHA256: sha, ImageSHA256: blob.SHA256, MIME: blob.MIME,
		Bytes: blob.Bytes, Source: source,
	}

	// A derived thumbnail is what the grid actually loads. Failure here is not
	// fatal: an image that cannot be scaled is still a perfectly good preview,
	// and refusing the upload over it would be the wrong trade.
	if t, err := thumb.Derive(data); err == nil {
		if tb, err := s.cfg.Blobs.Put(t.Data); err == nil {
			p.ThumbSHA256 = tb.SHA256
		}
		p.Width, p.Height = t.SourceWidth, t.SourceHeight
	} else if w, h, err := thumb.Dimensions(data); err == nil {
		p.Width, p.Height = w, h
	}

	// The workflow is stored as its own blob rather than inline, so it survives
	// the image being replaced and can be handed back as a file.
	if _, wf, err := thumb.ExtractWorkflow(data); err == nil {
		if wb, err := s.cfg.Blobs.Put(wf); err == nil {
			p.WorkflowSHA256 = wb.SHA256
		}
	}

	if err := s.cfg.Store.AddPreviewImage(p); err != nil {
		return nil, err
	}
	return s.cfg.Store.PreviewByImage(sha, blob.SHA256)
}

// handleDeletePreview handles DELETE /api/models/{sha}/previews/{image}.
//
// Detaches the image from this model. The blob is left in place: blobs are
// content-addressed and shared, so deleting the bytes would blank the same
// picture on every other model using it.
func (s *Server) handleDeletePreview(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))
	image := strings.ToLower(r.PathValue("image"))
	if err := s.cfg.Store.RemovePreviewImage(sha, image); err != nil {
		writeError(w, http.StatusNotFound, "no such preview", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reorderPreviewsRequest struct {
	Order []string `json:"order"`
}

// handleReorderPreviews handles PUT /api/models/{sha}/previews/order.
func (s *Server) handleReorderPreviews(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))
	var req reorderPreviewsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if err := s.cfg.Store.ReorderPreviewImages(sha, req.Order); err != nil {
		writeError(w, http.StatusBadRequest, "could not reorder", err.Error())
		return
	}
	images, err := s.cfg.Store.PreviewImages(sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"previews": images})
}

// handlePreviewWorkflow handles GET /api/models/{sha}/previews/{image}/workflow.
//
// Serves the extracted ComfyUI graph as a download, so it can be dropped
// straight back into ComfyUI.
func (s *Server) handlePreviewWorkflow(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Blobs == nil {
		writeError(w, http.StatusNotFound, "no preview store configured")
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))
	image := strings.ToLower(r.PathValue("image"))

	p, err := s.cfg.Store.PreviewByImage(sha, image)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such preview", err.Error())
		return
	}
	if p.WorkflowSHA256 == "" {
		writeError(w, http.StatusNotFound, "this preview carries no ComfyUI workflow")
		return
	}
	data, err := s.cfg.Blobs.Read(p.WorkflowSHA256)
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow blob missing", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "workflow-"+shortSHA(image)+".json"))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// --- generated-image picker --------------------------------------------------

// generatedImage is one file offered from the configured ComfyUI output folder.
type generatedImage struct {
	Name     string    `json:"name"`
	Rel      string    `json:"rel"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// handleGeneratedImages handles GET /api/generated.
//
// Lists recent images under the configured ComfyUI output directory so a
// preview can be chosen from work just rendered, rather than hunted for in a
// file dialog.
//
// Only that one configured directory is readable, and only image files within
// it: this endpoint reads the local filesystem on behalf of a browser, so its
// reach has to be a directory the user named, not a path the request supplies.
func (s *Server) handleGeneratedImages(w http.ResponseWriter, r *http.Request) {
	dir, err := s.comfyOutputDir()
	if err != nil {
		writeError(w, http.StatusBadRequest, "no ComfyUI output folder configured",
			"set "+store.SettingComfyOutputDir+" to a directory to pick generated images from")
		return
	}

	limit := queryInt(r, "limit", 60)
	if limit <= 0 || limit > 500 {
		limit = 60
	}

	var out []generatedImage
	// Bounded walk: an output folder is dated subdirectories deep, not a tree,
	// and an unbounded descent into a directory the user pointed at by mistake
	// should not stat a million files.
	const maxDepth = 3
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png", ".jpg", ".jpeg", ".webp":
		default:
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		out = append(out, generatedImage{
			Name: d.Name(), Rel: filepath.ToSlash(rel),
			Bytes: info.Size(), Modified: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read output folder", err.Error())
		return
	}

	// Newest first: the image you want is the one you just rendered.
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []generatedImage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "images": out})
}

type attachGeneratedRequest struct {
	// Rel is a path relative to the configured ComfyUI output directory, as
	// reported by GET /api/generated.
	Rel string `json:"rel"`
}

// handleAttachGenerated handles POST /api/models/{sha}/previews/generated.
func (s *Server) handleAttachGenerated(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "no preview store configured")
		return
	}
	sha := strings.ToLower(r.PathValue("sha"))

	var req attachGeneratedRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	path, err := s.resolveGenerated(req.Rel)
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid image", err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "no such generated image", req.Rel)
		return
	}
	if info.Size() > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "image too large")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read image", err.Error())
		return
	}
	if !blobstore.IsImage(data) {
		writeError(w, http.StatusUnsupportedMediaType, "not an image")
		return
	}

	preview, err := s.storePreview(sha, data, store.SourceManual)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store preview", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"preview": preview})
}

func (s *Server) comfyOutputDir() (string, error) {
	return s.settingDir(store.SettingComfyOutputDir, "ComfyUI output folder")
}

// resolveGenerated turns a client-supplied relative path into an absolute one
// inside the configured output directory.
//
// Same shape as the download destination check, and for the same reason: this
// is a client-supplied path used to read a local file, so traversal segments
// are stripped and containment is re-checked after symlinks are resolved.
func (s *Server) resolveGenerated(rel string) (string, error) {
	dir, err := s.comfyOutputDir()
	if err != nil {
		return "", err
	}
	return confineToDir(dir, rel, "image")
}
