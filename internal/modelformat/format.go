// Package modelformat locates the weights region of a model file and captures
// its header verbatim.
//
// It does not interpret headers. Phase 0 stores raw facts only (spec §15), so
// the JSON inside a safetensors header and the key/value pairs inside a GGUF
// header are captured as opaque bytes and left alone. The only thing parsed here
// is the structural framing needed to answer one question: at what byte offset
// do the tensor bytes begin?
//
// That offset is what makes weights_sha256 possible, and weights_sha256 is what
// lets a record survive a tool rewriting a safetensors header in place -- the
// one thing that breaks content-addressing (spec §2.1).
package modelformat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Format names, stored verbatim in model_file.format.
const (
	Safetensors = "safetensors"
	GGUF        = "gguf"
	Ckpt        = "ckpt"
	PT          = "pt"
	Unknown     = "unknown"
)

// DefaultMaxHeaderBytes caps how much of a header is stored. Large models carry
// multi-megabyte headers -- a GGUF tokenizer vocabulary especially -- and 19k of
// them would dominate a database that is meant to stay copyable (spec §16.4).
//
// Truncation costs nothing structurally: the weights offset is derived from the
// header's declared length, not from the bytes we retain, so a truncated blob
// never affects weights_sha256.
const DefaultMaxHeaderBytes = 8 << 20 // 8 MiB

// safetensorsMaxHeader is the limit from the safetensors format itself. A
// declared length beyond this means the file is not a safetensors file, or is
// corrupt; either way, guessing is worse than declining.
const safetensorsMaxHeader = 100 << 20

// ErrNoWeightsRegion means the weights region cannot be located for this file.
// Callers must map it to a NULL weights_sha256 -- "no rebinding key available"
// -- and never to a zero value that later reads like a real hash.
var ErrNoWeightsRegion = errors.New("modelformat: weights region is not determinable")

// Header is the outcome of inspecting a file's framing.
type Header struct {
	// Format is the detected format name.
	Format string

	// WeightsOffset is the byte offset at which tensor data begins.
	// Meaningful only when HasWeightsRegion is true.
	WeightsOffset int64

	// HasWeightsRegion is false for .ckpt and .pt, whose layout cannot be
	// determined without deserializing -- which is forbidden (spec §10.4) -- and
	// for files whose framing failed to parse.
	HasWeightsRegion bool

	// Blob is the header captured verbatim and uninterpreted.
	Blob []byte

	// BlobOffset is where Blob starts in the file.
	BlobOffset int64

	// Truncated reports that Blob was cut short at the configured cap.
	Truncated bool

	// ParseErr records why framing could not be read, for files whose extension
	// promised a parseable header. Non-fatal: the file is still hashed.
	ParseErr error
}

// DetectFormat classifies by extension. Content sniffing is applied afterwards
// for GGUF, which has a magic number; safetensors has none.
func DetectFormat(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".safetensors", ".sft":
		return Safetensors
	case ".gguf":
		return GGUF
	case ".ckpt":
		return Ckpt
	case ".pt", ".pth", ".bin":
		return PT
	default:
		return Unknown
	}
}

// IsModelExt reports whether a filename looks like a model file worth hashing.
func IsModelExt(path string) bool {
	return DetectFormat(path) != Unknown
}

// Read inspects the framing of a file of the given format and size.
//
// It never returns an error for an unparseable header: a file that fails to
// parse is still a file that must be hashed and recorded. The failure surfaces
// as HasWeightsRegion == false with ParseErr set.
func Read(r io.ReaderAt, format string, size int64, maxHeaderBytes int) Header {
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = DefaultMaxHeaderBytes
	}
	h := Header{Format: format}

	switch format {
	case Safetensors:
		return readSafetensors(r, size, maxHeaderBytes)
	case GGUF:
		return readGGUF(r, size, maxHeaderBytes)
	case Ckpt, PT:
		// Python pickle. Parsing it is arbitrary code execution on files sourced
		// from the internet (spec §10.4). Hash and size only, forever.
		h.ParseErr = ErrNoWeightsRegion
		return h
	default:
		h.ParseErr = ErrNoWeightsRegion
		return h
	}
}

// readSafetensors parses the fixed framing: an 8-byte little-endian header
// length, that many bytes of JSON, then tensor bytes.
func readSafetensors(r io.ReaderAt, size int64, maxHeaderBytes int) Header {
	h := Header{Format: Safetensors, BlobOffset: 8}

	if size < 8 {
		h.ParseErr = fmt.Errorf("%w: file is %d bytes, shorter than the 8-byte length prefix", ErrNoWeightsRegion, size)
		return h
	}

	var lenBuf [8]byte
	if _, err := r.ReadAt(lenBuf[:], 0); err != nil {
		h.ParseErr = fmt.Errorf("%w: reading header length: %v", ErrNoWeightsRegion, err)
		return h
	}
	headerLen := binary.LittleEndian.Uint64(lenBuf[:])

	// Reject before allocating. A corrupt or non-safetensors file can declare a
	// header length of 2^63, and a naive make([]byte, headerLen) turns that into
	// an OOM kill partway through a 19k-file scan.
	if headerLen == 0 {
		h.ParseErr = fmt.Errorf("%w: declared header length is zero", ErrNoWeightsRegion)
		return h
	}
	if headerLen > safetensorsMaxHeader {
		h.ParseErr = fmt.Errorf("%w: declared header length %d exceeds the format maximum", ErrNoWeightsRegion, headerLen)
		return h
	}
	weightsOffset := 8 + int64(headerLen)
	if weightsOffset > size {
		h.ParseErr = fmt.Errorf("%w: declared header length %d overruns the %d-byte file", ErrNoWeightsRegion, headerLen, size)
		return h
	}

	// The offset is established. Capturing the blob is a separate, best-effort
	// step: a short read of the header bytes must not cost us the weights hash.
	h.WeightsOffset = weightsOffset
	h.HasWeightsRegion = true

	readLen := int64(headerLen)
	if readLen > int64(maxHeaderBytes) {
		readLen = int64(maxHeaderBytes)
		h.Truncated = true
	}
	blob := make([]byte, readLen)
	if _, err := r.ReadAt(blob, 8); err != nil {
		h.ParseErr = fmt.Errorf("capturing header blob: %w", err)
		return h
	}
	h.Blob = blob
	return h
}
