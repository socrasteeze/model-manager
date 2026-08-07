package api

// Serving the bytes of a model.
//
// Every other read endpoint here hands out facts *about* the library. This one
// hands out the library itself, and it is the only place the daemon does, which
// is why it is opt-in and why it re-checks a path the database already gave it.
//
// It exists so a second model-manager -- a laptop, a workstation with a small
// SSD -- can fetch a model from the machine that holds the master library
// instead of from the internet. The client verifies the stream against the
// SHA256 it asked for, and since that hash is this endpoint's own path
// parameter, the check is exact by construction: there is no filename to
// mismatch and no metadata to disagree about.

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/store"
)

// handleModelFile streams one model file.
//
// Deliberately not behind readOnlyGuard. This is a read, like /api/models and
// the preview endpoints, and a read-only daemon exists to serve an index
// somebody else built -- serving the files that index describes is the same job,
// not a mutation of it.
func (s *Server) handleModelFile(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ServeFiles {
		writeError(w, http.StatusServiceUnavailable,
			"this daemon does not serve model files",
			"restart it with --serve-files to allow other machines to fetch from this library")
		return
	}

	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha")))
	row, size, err := s.cfg.Store.ConfirmedPresentPath(sha)
	if err != nil {
		// One status for "no such model" and "the model is known but no copy is
		// on this disk". The difference is real but it is not the caller's to
		// act on: either way this daemon cannot serve these bytes.
		writeError(w, http.StatusNotFound, "no on-disk copy of that model")
		return
	}

	resolved, err := resolveServable(s.cfg.Store, row.Path)
	if err != nil {
		writeServableError(w, err)
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		writeError(w, http.StatusNotFound, "model file is no longer readable", err.Error())
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not stat the model file", err.Error())
		return
	}
	if info.Size() != size {
		// The index says one thing and the disk says another, so the hash in the
		// URL is not a promise this daemon can keep. Better a refusal now than a
		// checksum failure after the client has pulled twelve gigabytes.
		writeError(w, http.StatusConflict,
			"the file on disk no longer matches the index",
			fmt.Sprintf("indexed %d bytes, found %d; rescan this root", size, info.Size()))
		return
	}

	// Set before ServeContent, which reads Etag itself for If-Range and
	// If-None-Match. A content hash is the strongest validator that exists: it
	// answers "am I resuming the same bytes" without any of the guesswork a
	// timestamp-and-length pair involves.
	w.Header().Set("ETag", `"`+sha+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(store.FilenameOf(row.Path)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Immutable is literally true here: this URL is addressed by the hash of what
	// it returns, so the bytes behind it can never change.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	// Empty name so ServeContent never sniffs a type -- a safetensors file opens
	// with a JSON header and would be served as text/plain -- and a zero modTime
	// so no Last-Modified is emitted. mtime is an accident of how a given copy
	// was written; for content-addressed bytes it is noise, and omitting it
	// forces validation onto the ETag, which means something.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// resolveServable turns a recorded path into one this daemon is willing to open.
//
// The containment rule lives in scan, beside the other things that know which
// files belong to which root, because eviction needs exactly the same one: the
// write side refuses to write outside a managed root, and both the read side
// and the delete side should refuse to act outside one.
func resolveServable(st *store.Store, recorded string) (string, error) {
	roots, err := st.EnabledRootPaths()
	if err != nil {
		return "", err
	}
	return scan.ResolveWithinRoots(roots, recorded)
}

func writeServableError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "model file is no longer on disk",
			"the index still lists it; rescan this root to clear the entry")
	case errors.Is(err, scan.ErrOutsideRoots):
		writeError(w, http.StatusForbidden,
			"that file is not under any enabled model root", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not resolve the model file", err.Error())
	}
}

// contentDisposition builds an attachment header for a filename.
//
// Two spellings on purpose, which RFC 6266 provides for: a sanitized ASCII
// fallback that any client can parse, and a UTF-8 form for the ones that
// implement it. Model filenames routinely carry non-ASCII characters, and a
// download that silently arrives named "_______.safetensors" is a bad enough
// outcome to be worth eight lines.
func contentDisposition(name string) string {
	if name == "" {
		name = "model.bin"
	}
	var ascii strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters, including the newline that would let a
			// filename inject a second header.
		case r > 0x7e:
			ascii.WriteByte('_')
		case r == '"' || r == '\\':
			ascii.WriteByte('_')
		default:
			ascii.WriteRune(r)
		}
	}
	fallback := ascii.String()
	if strings.TrimSpace(fallback) == "" {
		fallback = "model.bin"
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		fallback, pathEscapeFilename(name))
}

// pathEscapeFilename percent-encodes for the ext-value grammar of RFC 5987,
// which is narrower than url.PathEscape: it keeps only attr-char.
func pathEscapeFilename(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '!', c == '#', c == '$', c == '&', c == '+', c == '-',
			c == '.', c == '^', c == '_', c == '`', c == '|', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
