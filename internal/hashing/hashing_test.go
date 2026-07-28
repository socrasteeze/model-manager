package hashing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func buildSafetensors(headerJSON string, tensors []byte) []byte {
	var b bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(headerJSON)))
	b.Write(n[:])
	b.WriteString(headerJSON)
	b.Write(tensors)
	return b.Bytes()
}

func writeFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFullMatchesPlainSHA256(t *testing.T) {
	body := buildSafetensors(`{"w":{"dtype":"F16"}}`, bytes.Repeat([]byte{7}, 5000))
	path := writeFile(t, "m.safetensors", body)

	res, err := New(1024, 0).Full(path)
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if res.SHA256 != sha256Hex(body) {
		t.Fatalf("SHA256 = %s, want %s", res.SHA256, sha256Hex(body))
	}
	if res.Size != int64(len(body)) || res.BytesRead != int64(len(body)) {
		t.Fatalf("size/bytesRead = %d/%d, want %d", res.Size, res.BytesRead, len(body))
	}
}

func TestWeightsHashCoversExactlyTheTensorBytes(t *testing.T) {
	tensors := bytes.Repeat([]byte{0x5A}, 3000)
	body := buildSafetensors(`{"w":{"dtype":"F16"}}`, tensors)
	path := writeFile(t, "m.safetensors", body)

	res, err := New(512, 0).Full(path)
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if res.WeightsSHA256 != sha256Hex(tensors) {
		t.Fatalf("WeightsSHA256 = %s, want %s (the tensor bytes alone)",
			res.WeightsSHA256, sha256Hex(tensors))
	}
	if res.WeightsOffset != int64(len(body)-len(tensors)) {
		t.Fatalf("WeightsOffset = %d, want %d", res.WeightsOffset, len(body)-len(tensors))
	}
}

// This is the property the whole weights-region design exists for (spec §2.1):
// a tool rewrites the safetensors header in place, the file's SHA256 changes,
// and the record would silently orphan -- except that the weights hash is
// unchanged and can rebind it.
//
// Both shapes of rewrite are covered. A same-length rewrite leaves the tensor
// bytes where they are; a different-length rewrite shifts them to a new offset.
// The weights hash must survive both, because it is computed over the tensor
// bytes rather than over a fixed range of the file.
func TestHeaderRewritePreservesWeightsHash(t *testing.T) {
	tensors := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 900)
	dir := t.TempDir()
	h := New(1024, 0)

	original := buildSafetensors(`{"meta":"aaaaaaaa"}`, tensors)
	origPath := filepath.Join(dir, "orig.safetensors")
	if err := os.WriteFile(origPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := h.Full(origPath)
	if err != nil {
		t.Fatalf("Full(original): %v", err)
	}

	rewrites := map[string]string{
		"same length header": `{"meta":"bbbbbbbb"}`,
		"longer header":      `{"meta":"bbbbbbbb","added":"by another tool"}`,
		"shorter header":     `{"m":"b"}`,
	}
	for name, newHeader := range rewrites {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, "rewritten.safetensors")
			if err := os.WriteFile(p, buildSafetensors(newHeader, tensors), 0o644); err != nil {
				t.Fatal(err)
			}
			after, err := h.Full(p)
			if err != nil {
				t.Fatalf("Full(rewritten): %v", err)
			}

			if after.SHA256 == before.SHA256 {
				t.Fatal("SHA256 unchanged after a header rewrite; the fixture is not exercising the case")
			}
			if after.WeightsSHA256 != before.WeightsSHA256 {
				t.Fatalf("weights hash changed under a header rewrite: %s != %s\n"+
					"this is the failure mode weights_sha256 exists to survive",
					after.WeightsSHA256, before.WeightsSHA256)
			}
		})
	}
}

// A .ckpt cannot have its weights region located without unpickling it, which is
// forbidden. The empty string here becomes SQL NULL -- "no rebinding key" -- and
// must never be a digest of nothing that reads like a real value.
func TestPickleHasNoWeightsHash(t *testing.T) {
	path := writeFile(t, "m.ckpt", bytes.Repeat([]byte{0x80, 0x02}, 100))
	res, err := New(0, 0).Full(path)
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if res.WeightsSHA256 != "" {
		t.Fatalf("WeightsSHA256 = %q for a pickle file, want empty", res.WeightsSHA256)
	}
	if res.SHA256 == "" {
		t.Fatal("a pickle file must still get a whole-file hash")
	}
}

// A header that consumes the entire file leaves an empty weights region. Hashing
// nothing yields the digest of the empty string -- identical for every such file,
// and actively dangerous as a rebinding key.
func TestEmptyWeightsRegionYieldsNoWeightsHash(t *testing.T) {
	body := buildSafetensors(`{"only":"header"}`, nil)
	path := writeFile(t, "m.safetensors", body)

	res, err := New(0, 0).Full(path)
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if res.WeightsSHA256 != "" {
		t.Fatalf("WeightsSHA256 = %q for an empty weights region, want empty", res.WeightsSHA256)
	}
}

// The probe computed during a full streaming read and the probe computed by
// seeking to both ends must agree, or the second-tier cache would never hit.
func TestProbeAgreesWithStreamingProbe(t *testing.T) {
	sizes := []int{
		0, 1, 1000,
		ProbeWindow - 1, ProbeWindow, ProbeWindow + 1,
		2 * ProbeWindow, 2*ProbeWindow + 12345,
	}
	rng := rand.New(rand.NewSource(1))
	// A small buffer forces many chunks, exercising the tail buffer's shifting
	// path rather than the whole file landing in one read.
	h := New(64<<10, 0)

	for _, size := range sizes {
		body := make([]byte, size)
		rng.Read(body)
		path := writeFile(t, "m.safetensors", body)

		probe, err := h.Probe(path)
		if err != nil {
			t.Fatalf("size %d: Probe: %v", size, err)
		}
		full, err := h.Full(path)
		if err != nil {
			t.Fatalf("size %d: Full: %v", size, err)
		}
		if probe.ProbeSHA256 != full.ProbeSHA256 {
			t.Errorf("size %d: probe mismatch\n seek: %s\nstream: %s",
				size, probe.ProbeSHA256, full.ProbeSHA256)
		}
		if probe.Size != int64(size) {
			t.Errorf("size %d: Probe reported size %d", size, probe.Size)
		}
	}
}

// Two files differing only in the middle -- outside both sample windows -- get
// the same probe. That is exactly why a probe match binds provisionally and
// never confers identity (spec §10.1).
func TestProbeIsNotIdentity(t *testing.T) {
	size := 3 * ProbeWindow
	a := make([]byte, size)
	rand.New(rand.NewSource(2)).Read(a)
	b := append([]byte(nil), a...)
	b[size/2] ^= 0xFF

	dir := t.TempDir()
	pa := filepath.Join(dir, "a.safetensors")
	pb := filepath.Join(dir, "b.safetensors")
	if err := os.WriteFile(pa, a, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, b, 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(0, 0)
	ra, err := h.Full(pa)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := h.Full(pb)
	if err != nil {
		t.Fatal(err)
	}

	if ra.SHA256 == rb.SHA256 {
		t.Fatal("fixture is wrong: the files should differ")
	}
	// The differing byte sits at 1.5*ProbeWindow -- past the head window, before
	// the tail window -- so the probes are necessarily equal. Asserting it makes
	// the weakness a documented property rather than a hopeful one, and pins the
	// reason provisional bindings exist at all.
	if ra.ProbeSHA256 != rb.ProbeSHA256 {
		t.Fatalf("probe distinguished two files differing only outside both sample "+
			"windows (%s vs %s); the fixture no longer exercises the gap",
			ra.ProbeSHA256, rb.ProbeSHA256)
	}
}

func TestVerifyStable(t *testing.T) {
	const size, mtime = 1000, 555

	if err := verifyStable("/f", size, mtime, size, mtime, size); err != nil {
		t.Fatalf("stable file rejected: %v", err)
	}

	cases := map[string]struct{ postSize, postMtime, read int64 }{
		"file grew mid-read":      {size + 10, mtime, size},
		"file shrank mid-read":    {size - 10, mtime, size},
		"mtime moved mid-read":    {size, mtime + 1, size},
		"short read, stable stat": {size, mtime, size - 1},
		"over-read":               {size, mtime, size + 1},
	}
	for name, c := range cases {
		err := verifyStable("/f", size, mtime, c.postSize, c.postMtime, c.read)
		if !errors.Is(err, ErrChangedDuringHash) {
			t.Errorf("%s: err = %v, want ErrChangedDuringHash", name, err)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	cases := []struct {
		name   string
		n      int
		chunks [][]byte
		want   string
	}{
		{"single chunk shorter than window", 4, [][]byte{[]byte("ab")}, "ab"},
		{"single chunk exactly window", 4, [][]byte{[]byte("abcd")}, "abcd"},
		{"single chunk longer than window", 4, [][]byte{[]byte("abcdefg")}, "defg"},
		{"many small chunks", 4, [][]byte{[]byte("ab"), []byte("cd"), []byte("ef")}, "cdef"},
		{"uneven chunks", 3, [][]byte{[]byte("a"), []byte("bcde"), []byte("f")}, "def"},
		{"exact refill", 2, [][]byte{[]byte("xy"), []byte("z")}, "yz"},
	}
	for _, c := range cases {
		tb := newTailBuffer(c.n)
		for _, chunk := range c.chunks {
			tb.write(chunk)
		}
		if got := string(tb.bytes()); got != c.want {
			t.Errorf("%s: tail = %q, want %q", c.name, got, c.want)
		}
		if len(tb.bytes()) > c.n {
			t.Errorf("%s: tail grew past the window (%d > %d)", c.name, len(tb.bytes()), c.n)
		}
	}
}

func TestFullOnEmptyFile(t *testing.T) {
	path := writeFile(t, "empty.safetensors", nil)
	res, err := New(0, 0).Full(path)
	if err != nil {
		t.Fatalf("Full: %v", err)
	}
	if res.SHA256 != sha256Hex(nil) {
		t.Fatalf("SHA256 = %s, want the empty digest", res.SHA256)
	}
	if res.WeightsSHA256 != "" {
		t.Fatal("empty file reported a weights hash")
	}
}

func TestFullOnMissingFile(t *testing.T) {
	if _, err := New(0, 0).Full(filepath.Join(t.TempDir(), "nope.safetensors")); err == nil {
		t.Fatal("Full on a missing file returned no error")
	}
}
