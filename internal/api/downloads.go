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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/scan"
)

// downloadRequest is the body of POST /api/downloads.
type downloadRequest struct {
	URL string `json:"url"`

	// DestRoot must be one of the roots reported by GET /api/downloads/roots.
	DestRoot string `json:"dest_root"`

	// Subdir is an optional relative directory beneath DestRoot.
	Subdir string `json:"subdir,omitempty"`

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

	destDir, err := s.resolveDestination(req.DestRoot, req.Subdir)
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid destination", err.Error())
		return
	}

	job := download.Job{
		URL:            target.String(),
		DestDir:        destDir,
		Filename:       sanitizeRequestedName(req.Filename),
		ExpectedSHA256: strings.TrimSpace(req.SHA256),
		ExpectedSize:   req.Size,
	}

	// Run detached from the request. A model is gigabytes over a throttled
	// public API; holding the HTTP connection open for it would tie the
	// download's fate to a browser tab. Progress is polled instead.
	go func() {
		finished, err := s.cfg.Downloads.Fetch(context.Background(), job)
		if err != nil || finished.State != download.StateComplete {
			return
		}
		// Index it immediately so the library reflects reality without a manual
		// rescan, and so browse flips this result from new to have.
		if _, err := scan.IndexFile(s.cfg.Store, finished.FinalPath, req.DestRoot); err != nil {
			_ = err
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":   "started",
		"dest_dir": destDir,
	})
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
	rows, err := s.cfg.Store.DB().Query(
		`SELECT DISTINCT root FROM model_file_path WHERE root <> '' ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roots := []string{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

// resolveDestination validates a requested destination and returns it absolute.
//
// The root must be one already scanned, and the final directory must still be
// inside it after symlinks are resolved -- a subdirectory that is a symlink out
// of the tree would otherwise be a way to write anywhere the daemon can reach.
func (s *Server) resolveDestination(root, subdir string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("no destination root given")
	}

	known, err := s.scannedRoots()
	if err != nil {
		return "", err
	}
	matched := ""
	for _, k := range known {
		if pathsEqual(k, root) {
			matched = k
			break
		}
	}
	if matched == "" {
		return "", fmt.Errorf("%s is not a scanned model root", root)
	}

	rootAbs, err := filepath.Abs(matched)
	if err != nil {
		return "", err
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
	check := destAbs
	for {
		if resolved, err := filepath.EvalSymlinks(check); err == nil {
			check = resolved
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	if !withinRoot(rootAbs, check) {
		return "", fmt.Errorf("destination escapes the model root")
	}
	if !withinRoot(rootAbs, destAbs) {
		return "", fmt.Errorf("destination escapes the model root")
	}
	return destAbs, nil
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
