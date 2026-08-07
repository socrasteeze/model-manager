package scan

// IndexFile had no test coverage at all before the single-pass change, which is
// uncomfortable for the function that gives every downloaded model its
// identity. These pin the equivalence the change rests on: a file recorded from
// a handed-in identity must be indistinguishable from one recorded by reading
// it here.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/store"
)

func safetensors(headerJSON string, tensors []byte) []byte {
	var b bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(headerJSON)))
	b.Write(n[:])
	b.WriteString(headerJSON)
	b.Write(tensors)
	return b.Bytes()
}

func modelBytes() []byte {
	return safetensors(`{"w":{"dtype":"F16","shape":[4],"data_offsets":[0,8]}}`,
		[]byte{9, 8, 7, 6, 5, 4, 3, 2})
}

func tempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "master.db"),
		store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func fileRow(t *testing.T, st *store.Store, sha string) (store.ModelFile, store.FilePath) {
	t.Helper()
	var f store.ModelFile
	var weights *string
	err := st.DB().QueryRow(`
        SELECT sha256, weights_sha256, probe_sha256, size, format,
               header_blob, header_offset, header_truncated
          FROM model_file WHERE sha256 = ?`, sha).
		Scan(&f.SHA256, &weights, &f.ProbeSHA256, &f.Size, &f.Format,
			&f.HeaderBlob, &f.HeaderOffset, &f.HeaderTruncated)
	if err != nil {
		t.Fatalf("reading model_file for %s: %v", sha, err)
	}
	if weights != nil {
		f.WeightsSHA256 = *weights
	}
	paths, err := st.PathsFor(sha)
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths for %s = %+v (%v)", sha, paths, err)
	}
	return f, paths[0]
}

// The equivalence the whole change rests on: handing in an identity must record
// exactly what reading the file would have recorded.
func TestIndexPublishedMatchesIndexFileFieldForField(t *testing.T) {
	body := modelBytes()
	dir := t.TempDir()

	a := filepath.Join(dir, "a.safetensors")
	b := filepath.Join(dir, "b.safetensors")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stA := tempStore(t)
	shaA, err := IndexFile(stA, a, dir)
	if err != nil {
		t.Fatal(err)
	}

	// The download's shape: hash under the destination name while the bytes are
	// still called something else, then record without reading again.
	pre, err := hashing.New(0, 0).FullNamed(b, "b.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	stB := tempStore(t)
	shaB, err := IndexPublished(stB, b, dir, &pre)
	if err != nil {
		t.Fatal(err)
	}

	if shaA != shaB {
		t.Fatalf("content hashes differ: %s vs %s", shaA, shaB)
	}
	fa, pa := fileRow(t, stA, shaA)
	fb, pb := fileRow(t, stB, shaB)

	if fa.WeightsSHA256 != fb.WeightsSHA256 {
		t.Errorf("weights hash: %q vs %q", fa.WeightsSHA256, fb.WeightsSHA256)
	}
	if fa.ProbeSHA256 != fb.ProbeSHA256 || fa.Size != fb.Size || fa.Format != fb.Format {
		t.Errorf("content facts differ:\n%+v\n%+v", fa, fb)
	}
	if !bytes.Equal(fa.HeaderBlob, fb.HeaderBlob) ||
		fa.HeaderOffset != fb.HeaderOffset || fa.HeaderTruncated != fb.HeaderTruncated {
		t.Error("header capture differs between the two paths")
	}
	if pa.Present != pb.Present || pa.Provisional != pb.Provisional {
		t.Errorf("path flags differ: %+v vs %+v", pa, pb)
	}
	if pb.Size != int64(len(body)) {
		t.Errorf("path size = %d, want %d", pb.Size, len(body))
	}
}

// Complication (c), and the only place it can be tested: no CI runner straddles
// filesystems, so the cross-filesystem publish is simulated by copying, which
// produces exactly what it produces -- a new inode and a new mtime.
//
// The recorded four-tuple must describe the file that landed, not the staging
// file. It is the key the next incremental scan and every eviction compare
// against, so a tuple describing the .part would make the next scan re-hash
// every downloaded model and make eviction refuse to remove any of them.
func TestIndexPublishedRecordsTheCacheKeyOfTheFileOnDisk(t *testing.T) {
	body := modelBytes()
	dir := t.TempDir()

	staged := filepath.Join(dir, "3f9c1a.part")
	if err := os.WriteFile(staged, body, 0o644); err != nil {
		t.Fatal(err)
	}
	pre, err := hashing.New(0, 0).FullNamed(staged, "m.safetensors")
	if err != nil {
		t.Fatal(err)
	}

	// The copy path: a different inode and a different mtime from the staged file.
	published := filepath.Join(dir, "m.safetensors")
	if err := os.WriteFile(published, body, 0o644); err != nil {
		t.Fatal(err)
	}

	st := tempStore(t)
	sha, err := IndexPublished(st, published, dir, &pre)
	if err != nil {
		t.Fatal(err)
	}
	_, row := fileRow(t, st, sha)

	// Literally what eviction asks before it deletes anything.
	id, err := StatIdentity(published)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Matches(row.Device, row.Inode, row.Size, row.MtimeNs) {
		t.Fatalf("the recorded key describes the staging file, not the published one:\n"+
			"row = dev %d ino %d size %d mtime %d\ndisk = %+v",
			row.Device, row.Inode, row.Size, row.MtimeNs, id)
	}
	if row.Path != published {
		t.Errorf("path = %q, want the published file", row.Path)
	}
	// And the identity that was handed in still governs the content facts.
	if sha != pre.SHA256 {
		t.Errorf("sha = %s, want the handed-in %s", sha, pre.SHA256)
	}
}

// pre is never trusted about the file. A size or format that does not match
// what is on disk means it describes something else, so it is discarded and the
// file is read -- one read against a permanently wrong row.
func TestIndexPublishedRefusesAnIdentityThatDoesNotDescribeTheFile(t *testing.T) {
	body := modelBytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.safetensors")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	real, err := hashing.New(0, 0).Full(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		pre  hashing.Result
	}{
		{"wrong size", hashing.Result{SHA256: "deadbeef", Size: real.Size + 1, Format: real.Format}},
		{"wrong format", hashing.Result{SHA256: "deadbeef", Size: real.Size, Format: "gguf"}},
		{"no hash", hashing.Result{SHA256: "", Size: real.Size, Format: real.Format}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tempStore(t)
			pre := tc.pre
			sha, err := IndexPublished(st, path, dir, &pre)
			if err != nil {
				t.Fatal(err)
			}
			if sha != real.SHA256 {
				t.Errorf("recorded %s, want the file's real hash %s -- pre was trusted",
					sha, real.SHA256)
			}
			f, _ := fileRow(t, st, sha)
			if f.Format != real.Format {
				t.Errorf("format = %q, want %q", f.Format, real.Format)
			}
		})
	}
}

// A nil identity is the ordinary single-file path and must behave exactly as
// IndexFile always has.
func TestIndexPublishedWithNilReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.safetensors")
	if err := os.WriteFile(path, modelBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	st := tempStore(t)
	sha, err := IndexPublished(st, path, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fileRow(t, st, sha)
	if f.Format != "safetensors" || f.WeightsSHA256 == "" {
		t.Errorf("nil identity did not read the file properly: %+v", f)
	}
}
