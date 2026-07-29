package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "previews"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var pngHeader = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
}

func TestPutIsContentAddressed(t *testing.T) {
	s := newStore(t)
	blob, err := s.Put(pngHeader)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	sum := sha256.Sum256(pngHeader)
	if blob.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %s, not the content hash", blob.SHA256)
	}
	if blob.Bytes != int64(len(pngHeader)) {
		t.Fatalf("Bytes = %d", blob.Bytes)
	}
	if blob.MIME != "image/png" {
		t.Fatalf("MIME = %q, want image/png", blob.MIME)
	}
	if !s.Exists(blob.SHA256) {
		t.Fatal("blob does not exist after Put")
	}

	got, err := s.Read(blob.SHA256)
	if err != nil || string(got) != string(pngHeader) {
		t.Fatalf("Read returned %d bytes, err %v", len(got), err)
	}
}

// Two tools shipping the same preview is the normal case, not an edge case.
func TestPutIsIdempotent(t *testing.T) {
	s := newStore(t)
	first, _ := s.Put(pngHeader)
	second, err := s.Put(pngHeader)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("identical bytes produced different addresses")
	}
}

// Sniffing beats trusting a filename: a preview named .png that is actually HTML
// must not be served as HTML.
func TestMIMEIsSniffedNotAssumed(t *testing.T) {
	cases := map[string]string{
		"image/png":                "image/png",
		"application/octet-stream": "application/octet-stream",
	}
	if got := DetectMIME(pngHeader); got != "image/png" {
		t.Errorf("PNG sniffed as %q", got)
	}
	html := []byte("<html><body><script>alert(1)</script></body></html>")
	if got := DetectMIME(html); got != "application/octet-stream" {
		t.Errorf("HTML sniffed as %q, want it neutralized to a byte stream", got)
	}
	if IsImage(html) {
		t.Error("HTML reported as an image")
	}
	if !IsImage(pngHeader) {
		t.Error("PNG not reported as an image")
	}
	_ = cases
}

func TestPutRejectsEmptyAndOversized(t *testing.T) {
	s := newStore(t)
	if _, err := s.Put(nil); err == nil {
		t.Error("empty data was accepted")
	}
	if _, err := s.Put(make([]byte, MaxBlobBytes+1)); err == nil {
		t.Error("oversized data was accepted")
	}
}

// A content address is hex. Refusing anything else keeps a caller-supplied
// string from walking out of the store directory.
func TestOpenRejectsNonAddresses(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{
		"../../../etc/passwd",
		"not-a-hash",
		"",
		strings.Repeat("z", 64),
	} {
		if _, err := s.Open(bad); err == nil {
			t.Errorf("Open(%q) succeeded", bad)
		}
		if s.Exists(bad) {
			t.Errorf("Exists(%q) reported true", bad)
		}
	}
}

func TestShardedLayout(t *testing.T) {
	s := newStore(t)
	blob, _ := s.Put(pngHeader)

	path := s.Path(blob.SHA256)
	rel, err := filepath.Rel(s.Root(), path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != blob.SHA256[0:2] || parts[1] != blob.SHA256[2:4] {
		t.Fatalf("layout = %v, want two shard levels then the full address", parts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob not at the sharded path: %v", err)
	}
}

func TestPutFile(t *testing.T) {
	s := newStore(t)
	src := filepath.Join(t.TempDir(), "img.png")
	if err := os.WriteFile(src, pngHeader, 0o644); err != nil {
		t.Fatal(err)
	}
	blob, err := s.PutFile(src)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if !s.Exists(blob.SHA256) {
		t.Fatal("file was not stored")
	}
	if _, err := s.PutFile(filepath.Join(t.TempDir(), "missing.png")); err == nil {
		t.Error("PutFile on a missing file succeeded")
	}
}

// An interrupted write must not leave a half-blob that a later read serves as a
// valid image.
func TestNoTempFilesSurviveASuccessfulPut(t *testing.T) {
	s := newStore(t)
	if _, err := s.Put(pngHeader); err != nil {
		t.Fatal(err)
	}
	var leftovers []string
	_ = filepath.Walk(s.Root(), func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasPrefix(info.Name(), ".tmp-") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}
