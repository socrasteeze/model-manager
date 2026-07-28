package modelformat

import (
	"encoding/binary"
	"fmt"
	"io"
)

// GGUF framing:
//
//	magic "GGUF" | version u32 | tensor_count u64 | kv_count u64
//	kv_count × (key string, value_type u32, value)
//	tensor_count × (name string, n_dims u32, dims[n_dims] u64, ggml_type u32, offset u64)
//	padding to general.alignment
//	tensor data
//
// The data offset is not stored in the file; it is where the walk ends, rounded
// up to the alignment. So locating the weights region means walking every
// key/value pair and every tensor info -- there is no shortcut.

const (
	ggufMagic          = 0x46554747 // "GGUF" little-endian
	ggufDefaultAlign   = 32
	ggufMaxCount       = 1 << 24 // tensors or KV pairs; far above any real model
	ggufMaxStringBytes = 1 << 30
	ggufMaxDims        = 1 << 10
)

// GGUF metadata value types.
const (
	ggufUint8 uint32 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

var ggufScalarSize = map[uint32]int64{
	ggufUint8: 1, ggufInt8: 1, ggufBool: 1,
	ggufUint16: 2, ggufInt16: 2,
	ggufUint32: 4, ggufInt32: 4, ggufFloat32: 4,
	ggufUint64: 8, ggufInt64: 8, ggufFloat64: 8,
}

func readGGUF(r io.ReaderAt, size int64, maxHeaderBytes int) Header {
	h := Header{Format: GGUF, BlobOffset: 0}

	c := &cursor{r: r, size: size}

	magic, err := c.u32()
	if err != nil {
		h.ParseErr = fmt.Errorf("%w: reading magic: %v", ErrNoWeightsRegion, err)
		return h
	}
	if magic != ggufMagic {
		h.ParseErr = fmt.Errorf("%w: not a GGUF file (magic %#x)", ErrNoWeightsRegion, magic)
		return h
	}

	version, err := c.u32()
	if err != nil {
		h.ParseErr = fmt.Errorf("%w: reading version: %v", ErrNoWeightsRegion, err)
		return h
	}
	// GGUF v1 used 32-bit counts and string lengths. Walking it with the 64-bit
	// layout yields a plausible-looking but wrong offset, and a wrong weights
	// offset produces a wrong rebinding key -- worse than no key at all. Decline.
	if version < 2 || version > 3 {
		h.ParseErr = fmt.Errorf("%w: unsupported GGUF version %d", ErrNoWeightsRegion, version)
		return h
	}

	tensorCount, err := c.u64()
	if err != nil {
		h.ParseErr = fmt.Errorf("%w: reading tensor count: %v", ErrNoWeightsRegion, err)
		return h
	}
	kvCount, err := c.u64()
	if err != nil {
		h.ParseErr = fmt.Errorf("%w: reading kv count: %v", ErrNoWeightsRegion, err)
		return h
	}
	if tensorCount > ggufMaxCount || kvCount > ggufMaxCount {
		h.ParseErr = fmt.Errorf("%w: implausible counts (%d tensors, %d kv pairs)", ErrNoWeightsRegion, tensorCount, kvCount)
		return h
	}

	alignment := int64(ggufDefaultAlign)

	for i := uint64(0); i < kvCount; i++ {
		key, err := c.str()
		if err != nil {
			h.ParseErr = fmt.Errorf("%w: kv pair %d key: %v", ErrNoWeightsRegion, i, err)
			return h
		}
		vtype, err := c.u32()
		if err != nil {
			h.ParseErr = fmt.Errorf("%w: kv pair %d type: %v", ErrNoWeightsRegion, i, err)
			return h
		}

		// general.alignment is the one value whose contents we need, because the
		// data offset is rounded up to it. Everything else is skipped by size.
		if key == "general.alignment" && vtype == ggufUint32 {
			v, err := c.u32()
			if err != nil {
				h.ParseErr = fmt.Errorf("%w: reading general.alignment: %v", ErrNoWeightsRegion, err)
				return h
			}
			if v == 0 || v&(v-1) != 0 {
				h.ParseErr = fmt.Errorf("%w: general.alignment %d is not a power of two", ErrNoWeightsRegion, v)
				return h
			}
			alignment = int64(v)
			continue
		}

		if err := c.skipValue(vtype); err != nil {
			h.ParseErr = fmt.Errorf("%w: kv pair %d (%q): %v", ErrNoWeightsRegion, i, key, err)
			return h
		}
	}

	for i := uint64(0); i < tensorCount; i++ {
		if _, err := c.str(); err != nil { // tensor name
			h.ParseErr = fmt.Errorf("%w: tensor %d name: %v", ErrNoWeightsRegion, i, err)
			return h
		}
		nDims, err := c.u32()
		if err != nil {
			h.ParseErr = fmt.Errorf("%w: tensor %d dim count: %v", ErrNoWeightsRegion, i, err)
			return h
		}
		if nDims > ggufMaxDims {
			h.ParseErr = fmt.Errorf("%w: tensor %d declares %d dimensions", ErrNoWeightsRegion, i, nDims)
			return h
		}
		if err := c.skip(int64(nDims) * 8); err != nil { // dims
			h.ParseErr = fmt.Errorf("%w: tensor %d dims: %v", ErrNoWeightsRegion, i, err)
			return h
		}
		if err := c.skip(4 + 8); err != nil { // ggml type + offset
			h.ParseErr = fmt.Errorf("%w: tensor %d type/offset: %v", ErrNoWeightsRegion, i, err)
			return h
		}
	}

	dataOffset := align(c.pos, alignment)
	if dataOffset > size {
		h.ParseErr = fmt.Errorf("%w: computed data offset %d overruns the %d-byte file", ErrNoWeightsRegion, dataOffset, size)
		return h
	}

	h.WeightsOffset = dataOffset
	h.HasWeightsRegion = true

	blobLen := dataOffset
	if blobLen > int64(maxHeaderBytes) {
		blobLen = int64(maxHeaderBytes)
		h.Truncated = true
	}
	blob := make([]byte, blobLen)
	if _, err := r.ReadAt(blob, 0); err != nil && err != io.EOF {
		h.ParseErr = fmt.Errorf("capturing header blob: %w", err)
		return h
	}
	h.Blob = blob
	return h
}

func align(off, a int64) int64 {
	if a <= 1 {
		return off
	}
	return (off + a - 1) / a * a
}

// cursor is a sequential reader over an io.ReaderAt that refuses to run past the
// end of the file. Every read is bounds-checked against size, so a malformed
// header cannot walk the cursor into a huge allocation or an endless loop.
type cursor struct {
	r    io.ReaderAt
	size int64
	pos  int64
	buf  [8]byte
}

func (c *cursor) read(n int64) ([]byte, error) {
	if n < 0 || c.pos+n > c.size {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	if _, err := c.r.ReadAt(b, c.pos); err != nil {
		return nil, err
	}
	c.pos += n
	return b, nil
}

func (c *cursor) fixed(n int64) ([]byte, error) {
	if c.pos+n > c.size {
		return nil, io.ErrUnexpectedEOF
	}
	b := c.buf[:n]
	if _, err := c.r.ReadAt(b, c.pos); err != nil {
		return nil, err
	}
	c.pos += n
	return b, nil
}

func (c *cursor) u32() (uint32, error) {
	b, err := c.fixed(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *cursor) u64() (uint64, error) {
	b, err := c.fixed(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (c *cursor) str() (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	if n > ggufMaxStringBytes {
		return "", fmt.Errorf("string length %d is implausible", n)
	}
	// Only short strings are materialized -- long ones are almost always
	// tokenizer payloads, and we need lengths, not contents.
	if n > 4096 {
		return "", c.skip(int64(n))
	}
	b, err := c.read(int64(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *cursor) skip(n int64) error {
	if n < 0 || c.pos+n > c.size {
		return io.ErrUnexpectedEOF
	}
	c.pos += n
	return nil
}

func (c *cursor) skipValue(vtype uint32) error {
	if sz, ok := ggufScalarSize[vtype]; ok {
		return c.skip(sz)
	}
	switch vtype {
	case ggufString:
		n, err := c.u64()
		if err != nil {
			return err
		}
		if n > ggufMaxStringBytes {
			return fmt.Errorf("string length %d is implausible", n)
		}
		return c.skip(int64(n))

	case ggufArray:
		elemType, err := c.u32()
		if err != nil {
			return err
		}
		count, err := c.u64()
		if err != nil {
			return err
		}
		if sz, ok := ggufScalarSize[elemType]; ok {
			// Fixed-width elements skip in constant time. Multiplication is
			// bounds-checked by skip, which compares against the file size.
			if count > uint64(c.size) {
				return fmt.Errorf("array of %d elements overruns the file", count)
			}
			return c.skip(int64(count) * sz)
		}
		if elemType == ggufString {
			// A vocabulary array: hundreds of thousands of length-prefixed
			// strings that must each be stepped over individually.
			if count > ggufMaxCount {
				return fmt.Errorf("string array of %d elements is implausible", count)
			}
			for i := uint64(0); i < count; i++ {
				n, err := c.u64()
				if err != nil {
					return err
				}
				if n > ggufMaxStringBytes {
					return fmt.Errorf("string length %d is implausible", n)
				}
				if err := c.skip(int64(n)); err != nil {
					return err
				}
			}
			return nil
		}
		// Nested arrays are not part of the format.
		return fmt.Errorf("unsupported array element type %d", elemType)

	default:
		return fmt.Errorf("unknown value type %d", vtype)
	}
}
