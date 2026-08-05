package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

func testPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func uploadPreview(t *testing.T, s *Server, sha string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"http://localhost/api/models/"+sha+"/previews", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func writableServer(t *testing.T, root string) *Server {
	t.Helper()
	return serverWithRoot(t, root, func(c *Config) { c.ReadOnly = false })
}

func TestUploadedPreviewIsStoredWithAThumbnail(t *testing.T) {
	s := writableServer(t, testutil.TempDir(t))

	rec := uploadPreview(t, s, "aaa", testPNG(1200, 800))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Preview store.PreviewImage `json:"preview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Preview.Source != store.SourceManual {
		t.Errorf("source = %q, want manual", got.Preview.Source)
	}
	if got.Preview.ThumbSHA256 == "" {
		t.Error("no thumbnail derived for a 1200px upload")
	}
	if got.Preview.Width != 1200 || got.Preview.Height != 800 {
		t.Errorf("source dimensions not recorded: %dx%d",
			got.Preview.Width, got.Preview.Height)
	}
}

// The bytes come from an upload and are served back from the UI's own origin,
// where an HTML file that renders is XSS. The type is sniffed, never taken from
// a header or a filename.
func TestUploadRejectsNonImages(t *testing.T) {
	s := writableServer(t, testutil.TempDir(t))

	for _, body := range [][]byte{
		[]byte("<html><script>alert(1)</script></html>"),
		[]byte("%PDF-1.7\n"),
		[]byte("plain text"),
	} {
		rec := uploadPreview(t, s, "aaa", body)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("got %d for %q, want 415", rec.Code, body[:min(12, len(body))])
		}
	}
}

func TestUploadRefusedInReadOnlyMode(t *testing.T) {
	s := serverWithRoot(t, testutil.TempDir(t), func(c *Config) { c.ReadOnly = true })
	if rec := uploadPreview(t, s, "aaa", testPNG(64, 64)); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// The guarantee the user asked for: a thumbnail you chose is not displaced by
// the next enrichment run, and does not vanish when the source deletes it.
func TestManualPreviewOutranksAndSurvivesFetchedOnes(t *testing.T) {
	s := writableServer(t, testutil.TempDir(t))

	// A provider preview arrives first, at the front of the order.
	fetched, err := s.cfg.Blobs.Put(testPNG(300, 300))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.Store.AddPreviewImage(store.PreviewImage{
		SHA256: "aaa", ImageSHA256: fetched.SHA256, MIME: fetched.MIME,
		Bytes: fetched.Bytes, Source: provenance.SourceCivitai, Position: 0,
	}); err != nil {
		t.Fatal(err)
	}

	rec := uploadPreview(t, s, "aaa", testPNG(640, 480))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload failed: %d %s", rec.Code, rec.Body.String())
	}

	images, err := s.cfg.Store.PreviewImages("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("got %d previews, want 2 — the upload replaced rather than added", len(images))
	}
	if images[0].Source != store.SourceManual {
		t.Errorf("first preview is %q; the chosen one must rank first", images[0].Source)
	}

	// A later enrichment re-ingesting the same manual bytes must not demote it.
	if err := s.cfg.Store.AddPreviewImage(store.PreviewImage{
		SHA256: "aaa", ImageSHA256: images[0].ImageSHA256,
		MIME: images[0].MIME, Bytes: images[0].Bytes,
		Source: provenance.SourceCivitai, Position: 0,
	}); err != nil {
		t.Fatal(err)
	}
	images, _ = s.cfg.Store.PreviewImages("aaa")
	if images[0].Source != store.SourceManual {
		t.Errorf("enrichment demoted a manual preview to %q", images[0].Source)
	}
}

func TestPreviewWorkflowIsExtractedAndServedBack(t *testing.T) {
	s := writableServer(t, testutil.TempDir(t))

	base := testPNG(700, 700)
	workflow := []byte(`{"nodes":[{"id":1,"type":"KSampler"}]}`)
	img := splicePNGChunk(base, "tEXt", append([]byte("workflow\x00"), workflow...))

	rec := uploadPreview(t, s, "aaa", img)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Preview store.PreviewImage `json:"preview"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Preview.WorkflowSHA256 == "" {
		t.Fatal("workflow chunk was not extracted from the upload")
	}

	req := httptest.NewRequest(http.MethodGet,
		"http://localhost/api/models/aaa/previews/"+got.Preview.ImageSHA256+"/workflow", nil)
	wfRec := httptest.NewRecorder()
	s.ServeHTTP(wfRec, req)

	if wfRec.Code != http.StatusOK {
		t.Fatalf("workflow download: %d %s", wfRec.Code, wfRec.Body.String())
	}
	if !bytes.Equal(wfRec.Body.Bytes(), workflow) {
		t.Errorf("workflow round-trip changed the bytes: %q", wfRec.Body.String())
	}
}

func TestDeletePreviewDetachesButKeepsTheBlob(t *testing.T) {
	s := writableServer(t, testutil.TempDir(t))

	rec := uploadPreview(t, s, "aaa", testPNG(700, 700))
	var got struct {
		Preview store.PreviewImage `json:"preview"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	req := httptest.NewRequest(http.MethodDelete,
		"http://localhost/api/models/aaa/previews/"+got.Preview.ImageSHA256, nil)
	delRec := httptest.NewRecorder()
	s.ServeHTTP(delRec, req)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", delRec.Code, delRec.Body.String())
	}

	images, _ := s.cfg.Store.PreviewImages("aaa")
	if len(images) != 0 {
		t.Errorf("preview still attached: %+v", images)
	}
	// Blobs are content-addressed and shared, so the bytes stay put.
	if !s.cfg.Blobs.Exists(got.Preview.ImageSHA256) {
		t.Error("detaching a preview deleted shared blob bytes")
	}
}

// The generated-image picker reads the local filesystem for a browser, so its
// reach must be the one directory the user configured.
func TestGeneratedImagePickerCannotEscapeItsFolder(t *testing.T) {
	outside := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), testPNG(32, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(testutil.TempDir(t), "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := writableServer(t, testutil.TempDir(t))
	if err := s.cfg.Store.PutSetting(store.SettingComfyOutputDir, outDir); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"../secret.png",
		"../../secret.png",
		filepath.Join(outside, "secret.png"),
	} {
		if got, err := s.resolveGenerated(rel); err == nil && !withinRoot(outDir, got) {
			t.Errorf("%q escaped the output folder: %q", rel, got)
		}
	}
}

func TestGeneratedImagesListsNewestFirst(t *testing.T) {
	outDir := filepath.Join(testutil.TempDir(t), "output")
	if err := os.MkdirAll(filepath.Join(outDir, "2026-08-01"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.png", "2026-08-01/b.png", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(outDir, name), testPNG(32, 32), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := writableServer(t, testutil.TempDir(t))
	if err := s.cfg.Store.PutSetting(store.SettingComfyOutputDir, outDir); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/generated", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Images []generatedImage `json:"images"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 2 {
		t.Fatalf("got %d images, want the 2 pictures and not the .txt: %+v",
			len(got.Images), got.Images)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// splicePNGChunk inserts a chunk right after the signature, which is where
// ComfyUI's writer puts its metadata.
func splicePNGChunk(base []byte, chunkType string, payload []byte) []byte {
	var out bytes.Buffer
	out.Write(base[:8])

	_ = binary.Write(&out, binary.BigEndian, uint32(len(payload)))
	out.WriteString(chunkType)
	out.Write(payload)
	crc := crc32.NewIEEE()
	crc.Write([]byte(chunkType))
	crc.Write(payload)
	_ = binary.Write(&out, binary.BigEndian, crc.Sum32())

	out.Write(base[8:])
	return out.Bytes()
}
