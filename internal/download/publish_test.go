package download

// The cross-filesystem publish had no test coverage, and until now it had no
// verification either: the indexer's full re-read of the published file was
// incidentally the only thing that ever looked at what copyFile produced.
// Removing that read is what makes these necessary.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forceCopyPublish makes rename fail for the rest of the test, so publish takes
// the cross-filesystem branch on a machine with one volume.
func forceCopyPublish(t *testing.T) {
	t.Helper()
	prev := renameFile
	renameFile = func(string, string) error { return errors.New("simulated cross-device rename") }
	t.Cleanup(func() { renameFile = prev })
}

// copyFile's digest must describe the bytes it wrote, because publish decides
// whether to keep the file on the strength of it.
func TestCopyFileReturnsTheDigestOfWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	body := []byte(strings.Repeat("model-bytes", 5000))
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")

	got, err := copyFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != sha256Of(body) {
		t.Errorf("digest = %s, want %s", got, sha256Of(body))
	}
	if landed, err := os.ReadFile(dst); err != nil || string(landed) != string(body) {
		t.Errorf("copied file differs from the source (%v)", err)
	}
	// A consuming tool scanning the model root must not find a second,
	// half-named copy of every model.
	if _, err := os.Stat(dst + ".incoming"); !os.IsNotExist(err) {
		t.Error(".incoming left behind after a successful copy")
	}
}

// The check the removed second read used to provide by accident: a copy that
// does not come out at the verified hash never stays in the model tree.
func TestPublishRefusesACorruptedCopy(t *testing.T) {
	forceCopyPublish(t)
	m := newManager(t)
	dest := t.TempDir()

	body := []byte("the real model bytes")
	partial := filepath.Join(m.WorkDir, "job.part")
	if err := os.WriteFile(partial, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// A hash the copy cannot possibly produce -- what a garbled destination
	// write looks like from here.
	_, err := m.publish(partial, dest, "m.safetensors", sha256Of([]byte("different bytes")))
	if !errors.Is(err, ErrCopyCorrupted) {
		t.Fatalf("err = %v, want ErrCopyCorrupted", err)
	}

	// Nothing unverified is left in the model tree.
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("unverified bytes left in the destination: %v", names)
	}

	// And the partial survives. It passed verification; the destination is what
	// failed, so destroying the resume would force a full re-transfer to recover
	// from a local write fault.
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the partial was destroyed by a destination-side failure: %v", err)
	}
}

// The same branch, succeeding: a matching copy is published and the partial is
// cleaned up.
func TestPublishAcceptsAMatchingCopy(t *testing.T) {
	forceCopyPublish(t)
	m := newManager(t)
	dest := t.TempDir()

	body := []byte("verified model bytes")
	partial := filepath.Join(m.WorkDir, "ok.part")
	if err := os.WriteFile(partial, body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := m.publish(partial, dest, "m.safetensors", sha256Of(body))
	if err != nil {
		t.Fatalf("publish of matching bytes failed: %v", err)
	}
	if landed, err := os.ReadFile(out); err != nil || string(landed) != string(body) {
		t.Errorf("published file wrong (%v)", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Error("the partial was not cleaned up after a successful copy")
	}
}

// A job with no expected hash still has a verified one by the time publish runs
// -- run() always hashes. But wantSHA empty must not be treated as a mismatch,
// or a caller passing nothing would lose every file.
func TestPublishWithoutAVerifiedHash(t *testing.T) {
	forceCopyPublish(t)
	m := newManager(t)
	dest := t.TempDir()
	partial := filepath.Join(m.WorkDir, "nohash.part")
	if err := os.WriteFile(partial, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.publish(partial, dest, "m.safetensors", ""); err != nil {
		t.Fatalf("publish with no expected hash failed: %v", err)
	}
}

// The rename path skips the check, and must: dest is the very inode that was
// hashed, so there is nothing to prove and a digest would be pure cost.
func TestPublishOnTheRenamePathDoesNotVerify(t *testing.T) {
	m := newManager(t)
	dest := t.TempDir()
	partial := filepath.Join(m.WorkDir, "renamed.part")
	if err := os.WriteFile(partial, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A deliberately wrong hash: rename succeeds first, so it is never consulted.
	if _, err := m.publish(partial, dest, "m.safetensors", sha256Of([]byte("nope"))); err != nil {
		t.Fatalf("rename path consulted the hash: %v", err)
	}
}

// The two new sentinels have deliberately different dispositions from a
// checksum mismatch -- fail and keep, not quarantine -- so they must not be
// conflated with it.
func TestNewErrorsAreDistinct(t *testing.T) {
	for _, e := range []error{ErrCopyCorrupted, ErrPartialUnstable} {
		if errors.Is(e, ErrChecksumMismatch) || errors.Is(e, ErrSizeMismatch) {
			t.Errorf("%v is conflated with a quarantine error; the dispositions differ", e)
		}
	}
}
