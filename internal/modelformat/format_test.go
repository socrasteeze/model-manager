package modelformat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestDetectFormat(t *testing.T) {
	cases := map[string]string{
		"/m/a.safetensors": Safetensors,
		"/m/a.SAFETENSORS": Safetensors,
		"/m/a.sft":         Safetensors,
		"/m/a.gguf":        GGUF,
		"/m/a.ckpt":        Ckpt,
		"/m/a.pt":          PT,
		"/m/a.pth":         PT,
		"/m/a.bin":         PT,
		"/m/a.json":        Unknown,
		"/m/a":             Unknown,
	}
	for path, want := range cases {
		if got := DetectFormat(path); got != want {
			t.Errorf("DetectFormat(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSafetensorsWeightsOffset(t *testing.T) {
	header := `{"__metadata__":{"format":"pt"},"w":{"dtype":"F16","shape":[2],"data_offsets":[0,4]}}`
	tensors := []byte{1, 2, 3, 4}
	body, wantOffset := buildSafetensors(header, tensors)

	h := Read(bytes.NewReader(body), Safetensors, int64(len(body)), 0)
	if h.ParseErr != nil {
		t.Fatalf("ParseErr = %v", h.ParseErr)
	}
	if !h.HasWeightsRegion {
		t.Fatal("HasWeightsRegion = false for a well-formed safetensors file")
	}
	if h.WeightsOffset != wantOffset {
		t.Fatalf("WeightsOffset = %d, want %d", h.WeightsOffset, wantOffset)
	}
	if h.BlobOffset != 8 {
		t.Fatalf("BlobOffset = %d, want 8", h.BlobOffset)
	}
	if string(h.Blob) != header {
		t.Fatalf("Blob was not captured verbatim:\n got %q\nwant %q", h.Blob, header)
	}
	if h.Truncated {
		t.Fatal("Truncated = true for a header well under the cap")
	}
}

// The header is stored uninterpreted. A header that is not valid JSON at all
// must still yield a weights offset, because the offset comes from the declared
// length -- nothing about locating the tensor bytes requires understanding them.
func TestSafetensorsHeaderIsNotInterpreted(t *testing.T) {
	body, wantOffset := buildSafetensors("this is not json at all", []byte{9, 9})
	h := Read(bytes.NewReader(body), Safetensors, int64(len(body)), 0)
	if !h.HasWeightsRegion || h.WeightsOffset != wantOffset {
		t.Fatalf("non-JSON header broke offset detection: has=%v offset=%d want=%d",
			h.HasWeightsRegion, h.WeightsOffset, wantOffset)
	}
}

func TestSafetensorsHeaderTruncation(t *testing.T) {
	header := `{"pad":"` + strings.Repeat("x", 4096) + `"}`
	body, wantOffset := buildSafetensors(header, []byte{1})

	h := Read(bytes.NewReader(body), Safetensors, int64(len(body)), 512)
	if !h.Truncated {
		t.Fatal("Truncated = false for a header past the cap")
	}
	if len(h.Blob) != 512 {
		t.Fatalf("len(Blob) = %d, want 512", len(h.Blob))
	}
	// Truncating the stored blob must not disturb the offset: weights_sha256
	// depends on the declared length, not on how much of the header we kept.
	if h.WeightsOffset != wantOffset {
		t.Fatalf("truncation moved WeightsOffset to %d, want %d", h.WeightsOffset, wantOffset)
	}
}

// A corrupt or non-safetensors file can declare a header length of 2^63. The
// parser must reject it on the declared number, before allocating -- otherwise
// one bad file OOM-kills a scan partway through 7.5TB.
func TestSafetensorsAbsurdHeaderLengthIsRejectedBeforeAllocating(t *testing.T) {
	var body bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], 1<<62)
	body.Write(n[:])
	body.WriteString("short")

	h := Read(bytes.NewReader(body.Bytes()), Safetensors, int64(body.Len()), 0)
	if h.HasWeightsRegion {
		t.Fatal("absurd header length was accepted")
	}
	if !errors.Is(h.ParseErr, ErrNoWeightsRegion) {
		t.Fatalf("ParseErr = %v, want ErrNoWeightsRegion", h.ParseErr)
	}
}

func TestSafetensorsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty file":       {},
		"truncated prefix": {1, 2, 3},
		"zero length":      make([]byte, 8),
	}
	for name, body := range cases {
		h := Read(bytes.NewReader(body), Safetensors, int64(len(body)), 0)
		if h.HasWeightsRegion {
			t.Errorf("%s: HasWeightsRegion = true", name)
		}
		if !errors.Is(h.ParseErr, ErrNoWeightsRegion) {
			t.Errorf("%s: ParseErr = %v, want ErrNoWeightsRegion", name, h.ParseErr)
		}
	}

	// A header that overruns the file: declared length longer than what is there.
	var over bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], 1000)
	over.Write(n[:])
	over.WriteString("only a little")
	h := Read(bytes.NewReader(over.Bytes()), Safetensors, int64(over.Len()), 0)
	if h.HasWeightsRegion {
		t.Error("overrunning header length was accepted")
	}
}

// .ckpt and .pt are Python pickle. Parsing them is arbitrary code execution on
// files sourced from the internet (spec §10.4), so they get no weights region --
// ever, and by construction rather than by accident.
func TestPickleFormatsHaveNoWeightsRegion(t *testing.T) {
	for _, format := range []string{Ckpt, PT} {
		h := Read(bytes.NewReader(bytes.Repeat([]byte{0x80}, 128)), format, 128, 0)
		if h.HasWeightsRegion {
			t.Errorf("%s: HasWeightsRegion = true; pickle must never be parsed", format)
		}
		if h.Blob != nil {
			t.Errorf("%s: captured a header blob from a pickle file", format)
		}
		if !errors.Is(h.ParseErr, ErrNoWeightsRegion) {
			t.Errorf("%s: ParseErr = %v, want ErrNoWeightsRegion", format, h.ParseErr)
		}
	}
}

func TestGGUFWeightsOffset(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 256)
	body, wantOffset := newGGUF().
		kvString("general.architecture", "llama").
		kvUint32("llama.block_count", 32).
		kvFloat64("llama.rope.freq_base", 0x4079000000000000).
		kvInt32Array("llama.head_counts", []int32{8, 8, 8}).
		kvStringArray("tokenizer.ggml.tokens", []string{"a", "bb", "ccc", "dddd"}).
		tensor("blk.0.weight", []uint64{4096, 4096}, 1, 0).
		tensor("blk.1.weight", []uint64{4096}, 0, 128).
		build(data)

	h := Read(bytes.NewReader(body), GGUF, int64(len(body)), 0)
	if h.ParseErr != nil {
		t.Fatalf("ParseErr = %v", h.ParseErr)
	}
	if !h.HasWeightsRegion {
		t.Fatal("HasWeightsRegion = false for a well-formed GGUF file")
	}
	if h.WeightsOffset != wantOffset {
		t.Fatalf("WeightsOffset = %d, want %d", h.WeightsOffset, wantOffset)
	}
	if !bytes.Equal(body[h.WeightsOffset:], data) {
		t.Fatal("bytes at WeightsOffset are not the tensor data")
	}
}

// general.alignment moves the data offset. Ignoring it yields an offset that is
// merely close, which is worse than useless: the weights hash would be stable
// but computed over the wrong region.
func TestGGUFRespectsAlignmentOverride(t *testing.T) {
	data := bytes.Repeat([]byte{0xCD}, 64)
	body, wantOffset := newGGUF().
		kvString("general.architecture", "qwen").
		setAlignment(4096).
		tensor("t", []uint64{2}, 0, 0).
		build(data)

	h := Read(bytes.NewReader(body), GGUF, int64(len(body)), 0)
	if h.ParseErr != nil {
		t.Fatalf("ParseErr = %v", h.ParseErr)
	}
	if h.WeightsOffset != wantOffset {
		t.Fatalf("WeightsOffset = %d, want %d", h.WeightsOffset, wantOffset)
	}
	if h.WeightsOffset%4096 != 0 {
		t.Fatalf("WeightsOffset %d is not aligned to the declared 4096", h.WeightsOffset)
	}
	if !bytes.Equal(body[h.WeightsOffset:], data) {
		t.Fatal("alignment override produced the wrong data offset")
	}
}

// GGUF v1 used 32-bit counts and lengths. Walking it with the 64-bit layout
// produces a plausible but wrong offset, and a wrong rebinding key is worse than
// no key -- so v1 must be declined rather than guessed at.
func TestGGUFVersion1IsDeclined(t *testing.T) {
	body, _ := newGGUF().version(1).kvString("general.architecture", "llama").build([]byte{1, 2})
	h := Read(bytes.NewReader(body), GGUF, int64(len(body)), 0)
	if h.HasWeightsRegion {
		t.Fatal("GGUF v1 was parsed with the v2 layout")
	}
	if !errors.Is(h.ParseErr, ErrNoWeightsRegion) {
		t.Fatalf("ParseErr = %v, want ErrNoWeightsRegion", h.ParseErr)
	}
}

func TestGGUFMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"wrong magic":      []byte("NOPE\x03\x00\x00\x00"),
		"truncated header": []byte("GGUF\x03\x00\x00\x00\x01"),
	}
	for name, body := range cases {
		h := Read(bytes.NewReader(body), GGUF, int64(len(body)), 0)
		if h.HasWeightsRegion {
			t.Errorf("%s: HasWeightsRegion = true", name)
		}
	}

	// Implausible counts must be rejected on the declared number rather than by
	// looping 2^64 times.
	var absurd bytes.Buffer
	putU32(&absurd, ggufMagic)
	putU32(&absurd, 3)
	putU64(&absurd, 1<<40) // tensor count
	putU64(&absurd, 1<<40) // kv count
	h := Read(bytes.NewReader(absurd.Bytes()), GGUF, int64(absurd.Len()), 0)
	if h.HasWeightsRegion {
		t.Error("implausible GGUF counts were accepted")
	}
}

func TestAlign(t *testing.T) {
	cases := []struct{ off, a, want int64 }{
		{0, 32, 0}, {1, 32, 32}, {32, 32, 32}, {33, 32, 64},
		{100, 1, 100}, {100, 0, 100},
	}
	for _, c := range cases {
		if got := align(c.off, c.a); got != c.want {
			t.Errorf("align(%d, %d) = %d, want %d", c.off, c.a, got, c.want)
		}
	}
}
