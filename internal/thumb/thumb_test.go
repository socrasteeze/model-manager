package thumb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func testImage(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestDeriveScalesDownAndKeepsAspect(t *testing.T) {
	got, err := Derive(testImage(2048, 1024))
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != MaxDimension {
		t.Errorf("width = %d, want %d", got.Width, MaxDimension)
	}
	if got.Height != MaxDimension/2 {
		t.Errorf("height = %d, want %d (aspect not preserved)", got.Height, MaxDimension/2)
	}
	if got.SourceWidth != 2048 || got.SourceHeight != 1024 {
		t.Errorf("source dimensions lost: %dx%d", got.SourceWidth, got.SourceHeight)
	}
	if got.MIME != "image/jpeg" {
		t.Errorf("mime = %q", got.MIME)
	}
	// The saving is in pixels, which is what the byte saving follows from on
	// real images. Comparing encoded sizes here would not test anything: a
	// synthetic gradient PNG compresses far better than the photographic
	// renders this actually runs on.
	srcPixels := 2048 * 1024
	gotPixels := got.Width * got.Height
	if gotPixels*8 > srcPixels {
		t.Errorf("thumbnail is %d pixels against a %d-pixel source; barely a saving",
			gotPixels, srcPixels)
	}
}

// Storing a second near-identical copy of an already-small preview would double
// the blob store for nothing.
func TestDeriveRefusesAnAlreadySmallImage(t *testing.T) {
	if _, err := Derive(testImage(256, 256)); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("got %v, want ErrTooSmall", err)
	}
}

func TestDeriveRejectsNonImages(t *testing.T) {
	if _, err := Derive([]byte("<html>not an image</html>")); err == nil {
		t.Fatal("accepted non-image bytes")
	}
}

// --- PNG text chunks ---------------------------------------------------------

// pngWith rebuilds a PNG with an extra chunk spliced in after the signature,
// which is where ComfyUI's own writer puts its metadata.
func pngWith(base []byte, chunkType string, payload []byte) []byte {
	var out bytes.Buffer
	out.Write(base[:8]) // signature

	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(payload)))
	chunk.WriteString(chunkType)
	chunk.Write(payload)
	crc := crc32.NewIEEE()
	crc.Write([]byte(chunkType))
	crc.Write(payload)
	_ = binary.Write(&chunk, binary.BigEndian, crc.Sum32())

	out.Write(chunk.Bytes())
	out.Write(base[8:])
	return out.Bytes()
}

func TestExtractsComfyWorkflowFromATextChunk(t *testing.T) {
	workflow := []byte(`{"nodes":[{"id":1,"type":"KSampler"}]}`)
	payload := append([]byte("workflow\x00"), workflow...)
	img := pngWith(testImage(64, 64), "tEXt", payload)

	key, got, err := ExtractWorkflow(img)
	if err != nil {
		t.Fatal(err)
	}
	if key != "workflow" {
		t.Errorf("key = %q, want workflow", key)
	}
	if !bytes.Equal(got, workflow) {
		t.Errorf("workflow = %q", got)
	}
}

// Some front-ends write only the flattened API form, so it is the fallback
// rather than being ignored.
func TestFallsBackToThePromptChunk(t *testing.T) {
	prompt := []byte(`{"3":{"class_type":"KSampler"}}`)
	img := pngWith(testImage(64, 64), "tEXt", append([]byte("prompt\x00"), prompt...))

	key, got, err := ExtractWorkflow(img)
	if err != nil {
		t.Fatal(err)
	}
	if key != "prompt" || !bytes.Equal(got, prompt) {
		t.Errorf("key = %q, workflow = %q", key, got)
	}
}

func TestExtractsFromACompressedITXtChunk(t *testing.T) {
	workflow := []byte(`{"nodes":[{"id":7,"type":"CheckpointLoaderSimple"}]}`)

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(workflow)
	_ = zw.Close()

	// keyword \0 compressionFlag(1) compressionMethod(0) lang \0 translated \0 text
	payload := []byte("workflow\x00")
	payload = append(payload, 1, 0)
	payload = append(payload, 0) // empty language tag
	payload = append(payload, 0) // empty translated keyword
	payload = append(payload, compressed.Bytes()...)

	key, got, err := ExtractWorkflow(pngWith(testImage(64, 64), "iTXt", payload))
	if err != nil {
		t.Fatal(err)
	}
	if key != "workflow" || !bytes.Equal(got, workflow) {
		t.Errorf("key = %q, workflow = %q", key, got)
	}
}

func TestOrdinaryImageHasNoWorkflow(t *testing.T) {
	if _, _, err := ExtractWorkflow(testImage(64, 64)); !errors.Is(err, ErrNoWorkflow) {
		t.Fatalf("got %v, want ErrNoWorkflow", err)
	}
	if _, _, err := ExtractWorkflow([]byte("not a png at all")); err == nil {
		t.Fatal("accepted non-PNG bytes")
	}
}

// A chunk header claiming more bytes than the file holds must not panic or read
// past the end -- these bytes come from an upload.
func TestTruncatedChunkIsSurvived(t *testing.T) {
	img := pngWith(testImage(64, 64), "tEXt", append([]byte("workflow\x00"), []byte("{}")...))
	for _, cut := range []int{9, 20, 40, len(img) - 1} {
		if cut <= 0 || cut > len(img) {
			continue
		}
		if _, err := TextChunks(img[:cut]); err != nil && cut > 8 {
			// An error is fine; a panic or an out-of-range read is not, and
			// reaching this line at all means neither happened.
			continue
		}
	}
}
