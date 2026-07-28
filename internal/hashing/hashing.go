// Package hashing computes the content identities of a model file.
//
// Both hashes come out of one streaming pass. The whole-file SHA256 is the
// primary key and the Civitai lookup key; the weights-region SHA256 is the
// rebinding key that survives a tool rewriting a header in place (spec §2.1).
// The second hash costs one extra hash context over bytes already in memory --
// effectively free next to the read itself.
package hashing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/socrasteeze/model-manager/internal/modelformat"
)

// DefaultBufferSize is the read chunk. Large sequential reads are what a
// spinning array wants; see the worker benchmark in spec §17 before assuming
// more concurrency is better.
const DefaultBufferSize = 4 << 20

// ProbeWindow is how much of each end of a file feeds the sampled probe hash
// (spec §10.1).
const ProbeWindow = 1 << 20

// ErrChangedDuringHash means the file's size or mtime moved between the start
// and the end of the read, so the bytes hashed may be a mix of two states.
//
// This is not a hypothetical. Phase 0 hashes 7.5TB while a migration is still
// moving files, and a file caught half-written would otherwise have its
// partially-written bytes committed as a permanent identity (spec §10.2).
var ErrChangedDuringHash = errors.New("hashing: file changed while being read")

// Result is everything one pass over a file establishes.
type Result struct {
	SHA256 string

	// WeightsSHA256 is empty when the weights region is not determinable.
	// Callers must store empty as SQL NULL, never as a value.
	WeightsSHA256 string
	WeightsOffset int64

	ProbeSHA256 string

	Size      int64
	MtimeNs   int64
	Format    string
	Header    modelformat.Header
	BytesRead int64
}

// Hasher holds a reusable read buffer. One per worker: a 4MiB buffer allocated
// per file across 19k files is pure garbage-collector pressure.
type Hasher struct {
	buf            []byte
	maxHeaderBytes int
}

// New returns a Hasher. Zero values select the defaults.
func New(bufferSize, maxHeaderBytes int) *Hasher {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = modelformat.DefaultMaxHeaderBytes
	}
	return &Hasher{
		buf:            make([]byte, bufferSize),
		maxHeaderBytes: maxHeaderBytes,
	}
}

// Full reads the entire file once and returns every identity derived from it.
func (h *Hasher) Full(path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("hashing: opening %s: %w", path, err)
	}
	defer f.Close()

	// Stat through the descriptor, not the path. If the path is replaced mid-scan
	// we want the facts of the file we actually read, not of whatever now answers
	// to that name.
	preInfo, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("hashing: stat %s: %w", path, err)
	}
	size := preInfo.Size()
	preMtime := preInfo.ModTime().UnixNano()

	format := modelformat.DetectFormat(path)
	header := modelformat.Read(f, format, size, h.maxHeaderBytes)

	weightsOffset := header.WeightsOffset
	hasWeights := header.HasWeightsRegion
	// A weights region with no bytes in it would hash to the digest of the empty
	// string -- identical for every such file, and worse than useless as a
	// rebinding key.
	if hasWeights && weightsOffset >= size {
		hasWeights = false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("hashing: rewinding %s: %w", path, err)
	}

	full := sha256.New()
	var weights hash.Hash
	if hasWeights {
		weights = sha256.New()
	}

	first := make([]byte, 0, ProbeWindow)
	tail := newTailBuffer(ProbeWindow)

	var offset int64
	for {
		n, readErr := f.Read(h.buf)
		if n > 0 {
			chunk := h.buf[:n]
			full.Write(chunk)

			if weights != nil && offset+int64(n) > weightsOffset {
				// The chunk straddling the header/data boundary contributes only
				// its tail to the weights hash.
				start := int64(0)
				if offset < weightsOffset {
					start = weightsOffset - offset
				}
				weights.Write(chunk[start:])
			}

			if len(first) < ProbeWindow {
				want := ProbeWindow - len(first)
				if want > n {
					want = n
				}
				first = append(first, chunk[:want]...)
			}
			tail.write(chunk)

			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{}, fmt.Errorf("hashing: reading %s at offset %d: %w", path, offset, readErr)
		}
	}

	// The re-stat that makes a mid-migration scan safe (spec §10.2). Discarding
	// the result is the correct outcome: a wrong hash committed as an identity is
	// permanent, and re-reading one file on the next pass is cheap.
	postInfo, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("hashing: re-stat %s: %w", path, err)
	}
	if err := verifyStable(path, size, preMtime, postInfo.Size(), postInfo.ModTime().UnixNano(), offset); err != nil {
		return Result{}, err
	}

	res := Result{
		SHA256:      hex.EncodeToString(full.Sum(nil)),
		ProbeSHA256: probeDigest(size, first, tail.bytes()),
		Size:        size,
		MtimeNs:     preMtime,
		Format:      format,
		Header:      header,
		BytesRead:   offset,
	}
	if weights != nil {
		res.WeightsSHA256 = hex.EncodeToString(weights.Sum(nil))
		res.WeightsOffset = weightsOffset
	}
	return res, nil
}

// verifyStable decides whether the bytes just read can be trusted as the whole
// content of a single stable file.
//
// Any disagreement is fatal to the result rather than something to paper over:
// discarding a suspect hash costs one re-read on the next pass, while committing
// one assigns a wrong identity permanently.
func verifyStable(path string, preSize, preMtime, postSize, postMtime, bytesRead int64) error {
	if postSize != preSize || postMtime != preMtime {
		return fmt.Errorf("%w: %s (size %d->%d, mtime %d->%d)",
			ErrChangedDuringHash, path, preSize, postSize, preMtime, postMtime)
	}
	// A short read under a stable stat means we hashed a prefix, which would be
	// an identity for something that is not the file.
	if bytesRead != preSize {
		return fmt.Errorf("%w: %s (read %d bytes of %d)",
			ErrChangedDuringHash, path, bytesRead, preSize)
	}
	return nil
}

// ProbeResult is the cheap sampled identity used to decide whether a full read
// is worth doing.
type ProbeResult struct {
	ProbeSHA256 string
	Size        int64
	MtimeNs     int64
}

// Probe reads only the first and last ProbeWindow bytes.
//
// A probe result is a hint and nothing more. A sampled hash over a multi-GB file
// is a far weaker guarantee than a full one, and a false positive in a
// content-addressed system assigns a wrong identity permanently. Callers bind a
// probe match as provisional and confirm it with Full later (spec §10.1).
func (h *Hasher) Probe(path string) (ProbeResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("hashing: opening %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("hashing: stat %s: %w", path, err)
	}
	size := info.Size()

	window := int64(ProbeWindow)
	if size < window {
		window = size
	}

	first := make([]byte, window)
	if window > 0 {
		if _, err := f.ReadAt(first, 0); err != nil {
			return ProbeResult{}, fmt.Errorf("hashing: probing head of %s: %w", path, err)
		}
	}
	last := make([]byte, window)
	if window > 0 {
		if _, err := f.ReadAt(last, size-window); err != nil {
			return ProbeResult{}, fmt.Errorf("hashing: probing tail of %s: %w", path, err)
		}
	}

	return ProbeResult{
		ProbeSHA256: probeDigest(size, first, last),
		Size:        size,
		MtimeNs:     info.ModTime().UnixNano(),
	}, nil
}

// probeDigest defines the probe identically for the streaming and seeking paths,
// which is what lets a probe computed during a full hash be compared against one
// computed by Probe. Size is mixed in so the sample is self-discriminating rather
// than relying solely on the caller's size predicate.
//
// For a file smaller than 2*ProbeWindow the two windows overlap. That is
// harmless -- the digest stays deterministic, which is the only property it
// needs.
func probeDigest(size int64, first, last []byte) string {
	d := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	d.Write(sz[:])
	d.Write(first)
	d.Write(last)
	return hex.EncodeToString(d.Sum(nil))
}

// tailBuffer retains the last n bytes of an arbitrarily long stream, so the
// probe's trailing window comes out of the same sequential read as everything
// else instead of costing a seek to the end of a multi-gigabyte file.
type tailBuffer struct {
	buf []byte
	n   int
}

func newTailBuffer(n int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, 0, n), n: n}
}

func (t *tailBuffer) write(chunk []byte) {
	if len(chunk) >= t.n {
		t.buf = append(t.buf[:0], chunk[len(chunk)-t.n:]...)
		return
	}
	room := t.n - len(chunk)
	if len(t.buf) > room {
		// Drop from the front to make space for the new bytes.
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-room:]...)
	}
	t.buf = append(t.buf, chunk...)
}

func (t *tailBuffer) bytes() []byte { return t.buf }
