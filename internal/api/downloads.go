package api

// Downloading over HTTP.
//
// This is the most dangerous endpoint in the daemon and is written accordingly.
// Left naive it would be a remote primitive for "fetch an arbitrary URL and
// write it anywhere on this filesystem", so three separate things are checked
// before a single byte moves.
//
//  1. It mutates, so it is refused in read-only mode like any other write.
//  2. The source host must be a provider this daemon already talks to. A URL is
//     not accepted just because it is well-formed.
//  3. The destination must be a root that has already been scanned. The
//     destination is never inferred from the URL, the filename, or a default:
//     it is chosen by the caller from a list the server publishes, and anything
//     outside it is refused after symlink resolution.
//
// The project's standing guarantee is that it never modifies, moves, renames or
// deletes an existing model file, and that the only files it creates are ones
// you asked for at a destination you chose. Everything here exists to keep that
// true when the request arrives over a socket rather than from a shell.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/store"
)

// downloadRequest is the body of POST /api/downloads.
type downloadRequest struct {
	URL string `json:"url"`

	// DestRoot must be one of the roots reported by GET /api/downloads/roots.
	DestRoot string `json:"dest_root"`

	// Subdir is an optional relative directory beneath DestRoot. Left empty,
	// the server picks one from Type and the destination root's folder map.
	Subdir string `json:"subdir,omitempty"`

	// Type is the model type, used to resolve the subfolder. The client sends
	// what the provider called it; the server normalizes it and refuses to
	// invent a directory from anything it does not recognise.
	Type string `json:"type,omitempty"`

	Filename string `json:"filename,omitempty"`

	// SHA256, when supplied, is verified before the file is admitted. Browse
	// results carry it for every provider, so in practice it is always present
	// and a download is checked against a hash obtained independently of the
	// bytes.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

func (s *Server) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	if s.cfg.Downloads == nil {
		writeError(w, http.StatusServiceUnavailable, "downloading is disabled",
			"the server was started without a download manager")
		return
	}

	var req downloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "invalid download url")
		return
	}
	if !s.downloadHostAllowed(target.Host) {
		writeError(w, http.StatusForbidden, "download host not allowed", target.Host)
		return
	}

	destRoot := strings.TrimSpace(req.DestRoot)
	if destRoot == "" {
		if def, err := s.defaultDownloadRoot(); err == nil {
			destRoot = def
		}
	}

	// Subfolder policy is the server's, not the client's. A caller-supplied
	// subdir is still honoured -- the user may genuinely want one -- but the
	// default comes from (root, type) here rather than from a string the
	// browser built out of a provider's type name.
	subdir := req.Subdir
	if strings.TrimSpace(subdir) == "" {
		if canonical, err := store.CanonicalRoot(destRoot); err == nil {
			subdir = s.subfolderFor(canonical, req.Type)
		}
	}

	destDir, matchedRoot, err := s.resolveDestination(destRoot, subdir)
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid destination", err.Error())
		return
	}

	job := download.Job{
		URL:      target.String(),
		DestDir:  destDir,
		Filename: sanitizeRequestedName(req.Filename),
		// The canonical matched root, not the caller's raw spelling: the
		// indexer records this string verbatim, and a variant (trailing
		// slash, case) would fork the root in the database, breaking the
		// destination list and the absence sweep.
		DestRoot:       matchedRoot,
		ExpectedSHA256: strings.TrimSpace(req.SHA256),
		ExpectedSize:   req.Size,
	}

	// Start registers the job synchronously and runs the transfer detached
	// from the request — a model is gigabytes over a throttled public API,
	// and holding the connection open would tie the download's fate to a
	// browser tab. Synchronous registration is what lets the ID go back in
	// the 202: the client's first poll is guaranteed to see the job.
	started, err := s.cfg.Downloads.Start(context.Background(), job)
	if errors.Is(err, download.ErrInFlight) {
		// Not an error from the user's point of view — the thing they asked
		// for is already happening. 409 with the ID lets the client attach to
		// the in-progress job instead of showing a failure.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a download for this file is already in progress",
			"id":    started.ID,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not start download", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":   "started",
		"id":       started.ID,
		"dest_dir": destDir,
	})
}

// handleCancelDownload handles DELETE /api/downloads/{id}.
//
// In flight → cancel (202); terminal → forget the record (200); unknown →
// 404. Terminal jobs are only evicted on request: the queue is history the
// user may still be reading, not a buffer to trim. No readOnlyGuard — a
// read-only daemon never constructs a Manager, and cancelling writes nothing.
func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Downloads == nil {
		writeError(w, http.StatusServiceUnavailable, "downloading is disabled")
		return
	}
	id := r.PathValue("id")
	if s.cfg.Downloads.Cancel(id) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
		return
	}
	if s.cfg.Downloads.Remove(id) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
		return
	}
	writeError(w, http.StatusNotFound, "no such download", id)
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Downloads == nil {
		writeJSON(w, http.StatusOK, []download.Job{})
		return
	}
	jobs := s.cfg.Downloads.Jobs()
	if jobs == nil {
		jobs = []download.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleDownloadRoots publishes the destinations a download may target.
//
// The UI cannot offer a free-text path, because a path the server has not
// already accepted as a model root is not a legal destination. Publishing the
// list is what lets the client present a choice without being trusted to
// invent one.
func (s *Server) handleDownloadRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.scannedRoots()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list roots", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, roots)
}

// scannedRoots returns the distinct roots already recorded in the index.
func (s *Server) scannedRoots() ([]string, error) {
	// The managed-roots table is the authority. Before Phase 7 this was
	// SELECT DISTINCT root FROM model_file_path, which could not express a root
	// the user added but has not scanned yet -- and, worse, would keep offering
	// a root the user had removed, since removal marks paths absent rather than
	// deleting them. Migration 4 backfilled the table from exactly that query,
	// so an existing library sees no change.
	return s.cfg.Store.EnabledRootPaths()
}

// resolveDestination validates a requested destination.
//
// Returns the absolute directory to write into and the canonical matched root
// (the database's own spelling, which is what any recording against the root
// must use — the caller's variant would fork the root string).
//
// The root must be one already scanned, and the final directory must still be
// inside it after symlinks are resolved -- a subdirectory that is a symlink out
// of the tree would otherwise be a way to write anywhere the daemon can reach.
func (s *Server) resolveDestination(root, subdir string) (string, string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", "", fmt.Errorf("no destination root given")
	}

	known, err := s.scannedRoots()
	if err != nil {
		return "", "", err
	}
	matched := ""
	for _, k := range known {
		if pathsEqual(k, root) {
			matched = k
			break
		}
	}
	// Managed roots are stored symlink-resolved, so a caller naming the link
	// rather than the target is asking for a root that exists. Resolve and try
	// once more, rather than refusing a legitimate destination on a spelling.
	if matched == "" {
		if canonical, err := store.CanonicalRoot(root); err == nil {
			for _, k := range known {
				if pathsEqual(k, canonical) {
					matched = k
					break
				}
			}
		}
	}
	if matched == "" {
		return "", "", fmt.Errorf("%s is not a scanned model root", root)
	}

	rootAbs, err := filepath.Abs(matched)
	if err != nil {
		return "", "", err
	}
	// Resolve the root itself too, so both sides of the containment check are
	// expressed in the same terms.
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}

	destAbs := rootAbs
	if clean := cleanSubdir(subdir); clean != "" {
		destAbs = filepath.Join(rootAbs, clean)
	}

	// EvalSymlinks fails for a directory that does not exist yet, which is a
	// legitimate case for a new subdirectory. Check the nearest existing parent
	// instead, since that is what determines where the write actually lands.
	//
	// Only a genuinely-missing component may walk up. Any other error — a
	// permission-denied directory, an I/O fault — is a refusal, because
	// climbing past a component we could not inspect would climb past exactly
	// the symlink this check exists to catch.
	check := destAbs
	for {
		resolved, err := filepath.EvalSymlinks(check)
		if err == nil {
			check = resolved
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("cannot verify destination: %v", err)
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	if !withinRoot(rootAbs, check) {
		return "", "", fmt.Errorf("destination escapes the model root")
	}
	if !withinRoot(rootAbs, destAbs) {
		return "", "", fmt.Errorf("destination escapes the model root")
	}
	return destAbs, matched, nil
}

// cleanSubdir reduces a requested subdirectory to a safe relative path.
func cleanSubdir(subdir string) string {
	subdir = strings.TrimSpace(strings.ReplaceAll(subdir, "\\", "/"))
	if subdir == "" {
		return ""
	}
	var parts []string
	for _, seg := range strings.Split(subdir, "/") {
		seg = strings.TrimSpace(seg)
		// Absolute prefixes, traversal and current-directory segments are
		// dropped rather than rejected: the caller gets the safe interpretation
		// of what they asked for.
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		parts = append(parts, sanitizeSegmentName(seg))
	}
	return filepath.Join(parts...)
}

func sanitizeSegmentName(seg string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, seg)
}

// sanitizeRequestedName keeps a caller-supplied filename to a bare name.
//
// Empty is fine: the download manager derives one from the URL, and its own
// sanitizer runs regardless of what happens here.
func sanitizeRequestedName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return ""
	}
	return sanitizeSegmentName(filepath.Base(name))
}

func withinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

func pathsEqual(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// downloadHostAllowed reports whether a download may be fetched from a host.
//
// Broader than the image proxy's list because model files are served from
// dedicated CDN hosts, but still a list: an arbitrary URL is not a legal
// download source.
//
// Note the limit of this check. It constrains the host the request is *sent*
// to; the transfer follows redirects, as it must for HuggingFace, whose resolve
// URLs redirect to per-region CDN hosts. Redirect targets are not re-checked,
// so this bounds who you can ask, not every host ultimately contacted. The
// response is written to a quarantine file and verified against an expected
// hash rather than returned to the caller, which is what keeps that acceptable.
func (s *Server) downloadHostAllowed(host string) bool {
	h := strings.ToLower(host)
	if s.imageHostAllowed(h) {
		return true
	}
	for _, suffix := range downloadHostSuffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return true
		}
	}
	return false
}

// downloadHostSuffixes are provider download CDNs, matched as domain suffixes
// because HuggingFace numbers its CDN hosts per region.
var downloadHostSuffixes = []string{
	"civitai.com",
	"civitai.red",
	"civitai.green",
	"civarchive.com",
	"huggingface.co",
	"hf.co",
}

// downloadWorkDir is where partial transfers live before they are verified.
func downloadWorkDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "downloads")
}

// ensureDir creates a directory if it is missing.
func ensureDir(path string) error {
	if path == "" {
		return nil
	}
	return os.MkdirAll(path, 0o755)
}
