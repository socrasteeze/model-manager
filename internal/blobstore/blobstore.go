// Package blobstore is a content-addressed store for preview images.
//
// Previews are app-managed rather than referenced in place (spec §18). An
// in-place reference breaks the moment a file moves, which is the precise
// failure this whole project exists to eliminate -- so the bytes are copied here
// and addressed by their own hash.
//
// They live beside the database rather than inside it. The single-file database
// is what makes the declared sole authority backable (§16.4), and tens of
// gigabytes of JPEGs inside it would destroy that property.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MaxBlobBytes caps what will be admitted. A preview is a thumbnail-to-poster
// sized image; anything larger is a mistake or a hostile input, and there is no
// reason to spend disk finding out which.
const MaxBlobBytes = 32 << 20

// Store is a content-addressed blob directory.
type Store struct {
	root string
}

// New opens (creating if necessary) a blob store rooted at dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blobstore: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("blobstore: resolving %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("blobstore: creating %s: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// Root is the blob directory.
func (s *Store) Root() string { return s.root }

// Blob describes stored bytes.
type Blob struct {
	SHA256 string
	Bytes  int64
	MIME   string
}

// Put stores data and returns its content address.
//
// Writing is atomic via a temporary file and a rename, so an interrupted write
// leaves no half-blob that a later read would serve as a valid image. Storing
// the same bytes twice is a no-op, which is what makes ingest re-runnable.
func (s *Store) Put(data []byte) (Blob, error) {
	if len(data) == 0 {
		return Blob{}, errors.New("blobstore: refusing to store empty data")
	}
	if len(data) > MaxBlobBytes {
		return Blob{}, fmt.Errorf("blobstore: %d bytes exceeds the %d-byte limit", len(data), MaxBlobBytes)
	}

	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	blob := Blob{SHA256: sha, Bytes: int64(len(data)), MIME: DetectMIME(data)}

	dest := s.Path(sha)
	if _, err := os.Stat(dest); err == nil {
		return blob, nil // already stored; identical bytes by construction
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Blob{}, fmt.Errorf("blobstore: creating shard: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return Blob{}, fmt.Errorf("blobstore: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Blob{}, fmt.Errorf("blobstore: writing blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Blob{}, fmt.Errorf("blobstore: closing blob: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return Blob{}, fmt.Errorf("blobstore: publishing blob: %w", err)
	}
	return blob, nil
}

// PutFile copies a file into the store.
func (s *Store) PutFile(path string) (Blob, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Blob{}, fmt.Errorf("blobstore: stat %s: %w", path, err)
	}
	if info.Size() > MaxBlobBytes {
		return Blob{}, fmt.Errorf("blobstore: %s is %d bytes, over the limit", path, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Blob{}, fmt.Errorf("blobstore: reading %s: %w", path, err)
	}
	return s.Put(data)
}

// Path is where a blob lives. Two levels of sharding keep any one directory from
// holding tens of thousands of entries, which several filesystems handle badly.
func (s *Store) Path(sha string) string {
	if len(sha) < 4 {
		return filepath.Join(s.root, sha)
	}
	return filepath.Join(s.root, sha[0:2], sha[2:4], sha)
}

// Open returns a reader for a stored blob.
func (s *Store) Open(sha string) (*os.File, error) {
	if !isHex(sha) {
		// A non-hex name cannot be a content address, and refusing it here stops
		// a caller-supplied string from being used to traverse out of the store.
		return nil, fmt.Errorf("blobstore: %q is not a content address", sha)
	}
	return os.Open(s.Path(sha))
}

// Exists reports whether a blob is stored.
func (s *Store) Exists(sha string) bool {
	if !isHex(sha) {
		return false
	}
	_, err := os.Stat(s.Path(sha))
	return err == nil
}

// Read returns a stored blob's bytes.
func (s *Store) Read(sha string) ([]byte, error) {
	f, err := s.Open(sha)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxBlobBytes))
}

func isHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// DetectMIME sniffs an image type from its magic bytes rather than trusting a
// filename. A preview named .png that is actually a JPEG would otherwise be
// served with the wrong type, and an HTML file named .png would be served as an
// image -- or worse, as HTML.
func DetectMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return mime
	}
	// Anything that does not sniff as a known image is stored as an opaque
	// stream. Serving it as text/html would make a preview into an XSS vector.
	return "application/octet-stream"
}

// IsImage reports whether the bytes sniff as a supported image format.
func IsImage(data []byte) bool {
	return strings.HasPrefix(DetectMIME(data), "image/")
}
