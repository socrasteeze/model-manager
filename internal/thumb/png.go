// Package thumb derives grid-sized copies of preview images and reads the
// metadata ComfyUI leaves inside its own PNGs.
//
// Both halves are stdlib only. A JPEG/PNG decoder and a scaler already ship
// with Go, and a PNG text chunk is a length-prefixed record any reader can walk
// -- adding an image library to this project would mean adding a dependency to
// every build of a tool whose whole point is to be one static binary you can
// copy onto a NAS.
package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// WorkflowKeys are the PNG text-chunk keywords ComfyUI writes.
//
// `workflow` is the graph as the editor shows it -- the one worth keeping,
// because it is what can be dragged back into ComfyUI. `prompt` is the
// flattened API form, kept as a fallback since some front-ends write only that.
var WorkflowKeys = []string{"workflow", "prompt"}

// ErrNoWorkflow means the image carried no recognisable workflow chunk.
var ErrNoWorkflow = errors.New("thumb: no ComfyUI workflow in this image")

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// ExtractWorkflow pulls the ComfyUI workflow JSON out of a PNG's text chunks.
//
// A generated image is the only artifact that reliably carries the graph that
// made it, which is why the user asked for the workflow to survive attaching
// the picture: the thumbnail and the recipe are the same file.
//
// Returns the keyword it found alongside the JSON, so a caller can tell a full
// `workflow` graph from the flattened `prompt` form.
func ExtractWorkflow(data []byte) (key string, workflow []byte, err error) {
	chunks, err := textChunks(data)
	if err != nil {
		return "", nil, err
	}
	for _, want := range WorkflowKeys {
		if v, ok := chunks[want]; ok && len(bytes.TrimSpace(v)) > 0 {
			return want, v, nil
		}
	}
	return "", nil, ErrNoWorkflow
}

// TextChunks returns every tEXt/iTXt/zTXt keyword in a PNG.
func TextChunks(data []byte) (map[string][]byte, error) { return textChunks(data) }

// maxTextChunk caps one text chunk. A ComfyUI workflow is tens of kilobytes of
// JSON; anything past this is not a workflow, and decompressing it to find out
// is how a zip bomb gets in.
const maxTextChunk = 8 << 20

func textChunks(data []byte) (map[string][]byte, error) {
	if len(data) < len(pngMagic) || !bytes.Equal(data[:len(pngMagic)], pngMagic) {
		return nil, errors.New("thumb: not a PNG")
	}
	out := map[string][]byte{}

	// PNG is a flat sequence of length-prefixed chunks after the signature:
	// 4-byte length, 4-byte type, payload, 4-byte CRC. Walking it needs no
	// decoder, and deliberately does not use one -- this runs on bytes a user
	// uploaded, and the less that is parsed the better.
	pos := len(pngMagic)
	for pos+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		body := pos + 8
		if length < 0 || body+length+4 > len(data) {
			break // truncated; keep whatever was already read
		}
		payload := data[body : body+length]

		switch typ {
		case "tEXt":
			if k, v, ok := splitKeyword(payload); ok {
				out[k] = v
			}
		case "iTXt":
			if k, v, ok := parseITXt(payload); ok {
				out[k] = v
			}
		case "zTXt":
			if k, v, ok := parseZTXt(payload); ok {
				out[k] = v
			}
		case "IEND":
			return out, nil
		}
		pos = body + length + 4
	}
	return out, nil
}

// splitKeyword splits a NUL-separated keyword and value (tEXt layout).
func splitKeyword(payload []byte) (string, []byte, bool) {
	i := bytes.IndexByte(payload, 0)
	if i <= 0 {
		return "", nil, false
	}
	return string(payload[:i]), payload[i+1:], true
}

// parseITXt reads the international text layout:
//
//	keyword \0 compressionFlag compressionMethod languageTag \0 translatedKeyword \0 text
func parseITXt(payload []byte) (string, []byte, bool) {
	i := bytes.IndexByte(payload, 0)
	if i <= 0 || i+3 > len(payload) {
		return "", nil, false
	}
	keyword := string(payload[:i])
	compressed := payload[i+1] != 0
	rest := payload[i+3:] // skip compression flag and method

	// languageTag \0 translatedKeyword \0
	for n := 0; n < 2; n++ {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return "", nil, false
		}
		rest = rest[j+1:]
	}
	if compressed {
		v, err := inflate(rest)
		if err != nil {
			return "", nil, false
		}
		return keyword, v, true
	}
	return keyword, rest, true
}

// parseZTXt reads keyword \0 compressionMethod compressedText.
func parseZTXt(payload []byte) (string, []byte, bool) {
	i := bytes.IndexByte(payload, 0)
	if i <= 0 || i+2 > len(payload) {
		return "", nil, false
	}
	v, err := inflate(payload[i+2:])
	if err != nil {
		return "", nil, false
	}
	return string(payload[:i]), v, true
}

func inflate(b []byte) ([]byte, error) {
	r, err := newZlibReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()

	// Bounded read: a compressed chunk that expands without limit is a zip
	// bomb, and this is running on an uploaded file.
	var buf bytes.Buffer
	if _, err := copyN(&buf, r, maxTextChunk+1); err != nil {
		return nil, err
	}
	if buf.Len() > maxTextChunk {
		return nil, fmt.Errorf("thumb: text chunk over %d bytes", maxTextChunk)
	}
	return buf.Bytes(), nil
}
