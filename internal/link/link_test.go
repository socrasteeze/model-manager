package link

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The probe is empirical rather than inferred. §16.2 makes the point that btrfs
// subvolumes report different st_dev values on the same filesystem, so comparing
// device IDs gives a false negative -- the reliable test is to attempt the
// operation and see whether it works.
func TestProbeFindsAtLeastCopy(t *testing.T) {
	dir := t.TempDir()
	cap, err := Probe(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Supports(Copy) {
		t.Fatal("copy was not reported as available; it always is")
	}
	if len(cap.Available) == 0 {
		t.Fatal("no strategies reported")
	}
	// The probe must clean up after itself.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 3 && e.Name()[:3] == ".mm" {
			t.Fatalf("probe left %s behind", e.Name())
		}
	}
}

// A hardlink is the one mechanism that can corrupt the original, so it must
// never be chosen unless asked for (§9.3).
func TestBestNeverPicksHardlinkUnlessAllowed(t *testing.T) {
	c := &Capability{Available: []Strategy{Hardlink, Copy}}
	if got := c.Best(false); got == Hardlink {
		t.Fatal("Best chose a hardlink without permission")
	}
	if got := c.Best(true); got != Hardlink {
		t.Fatalf("Best(allow) = %s, want hardlink when it is the best available", got)
	}
}

// Copy-on-write mechanisms come first; hardlink sits below plain copy because it
// is the one that can write through to the original.
func TestPreferenceOrder(t *testing.T) {
	all := []Strategy{Hardlink, Copy, Symlink, BlockClone, Reflink}
	sortByPreference(all)

	want := []Strategy{Reflink, BlockClone, Symlink, Copy, Hardlink}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("order = %v, want %v", all, want)
		}
	}
}

func TestMaterializeCopy(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src.bin", 4096)
	dst := filepath.Join(dir, "sub", "dst.bin")

	if err := Materialize(src, dst, Copy); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(src)
	if string(got) != string(want) {
		t.Fatal("copy does not match the source")
	}
}

func TestMaterializeSymlinkAndHardlink(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src.bin", 1024)

	symPath := filepath.Join(dir, "sym.bin")
	if err := Materialize(src, symPath, Symlink); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	info, err := os.Lstat(symPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink strategy did not create a symlink")
	}

	hardPath := filepath.Join(dir, "hard.bin")
	if err := Materialize(src, hardPath, Hardlink); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}
	srcInfo, _ := os.Stat(src)
	hardInfo, _ := os.Stat(hardPath)
	if !os.SameFile(srcInfo, hardInfo) {
		t.Fatal("hardlink strategy did not produce the same file")
	}
}

// A failed clone must not leave a zero-byte file where a model should be: a
// consuming tool would try to load it.
func TestFailedMaterializeLeavesNoStub(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src.bin", 1024)
	dst := filepath.Join(dir, "dst.bin")

	// BlockClone is a Windows feature; on other platforms it always fails.
	err := Materialize(src, dst, BlockClone)
	if err == nil {
		t.Skip("block cloning is available here")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Fatal("a failed clone left a file behind")
	}
}

func TestUnknownStrategy(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src.bin", 16)
	if err := Materialize(src, filepath.Join(dir, "x"), Strategy("nonsense")); err == nil {
		t.Fatal("an unknown strategy was accepted")
	}
}

// These warnings are §9.2 and §9.3 made visible in-product rather than buried in
// a spec, so their absence is a real regression.
func TestWarningsExistForTheDangerousStrategies(t *testing.T) {
	if len(Warnings(Symlink)) == 0 {
		t.Error("no SMB warning for symlinks")
	}
	if len(Warnings(Hardlink)) == 0 {
		t.Error("no write-through warning for hardlinks")
	}
	if len(Warnings(Reflink)) != 0 {
		t.Error("reflinks carry a warning they do not need")
	}
}

func TestExtentsReportsSomethingUsable(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src.bin", 128*1024)

	info, err := Extents(src)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	if info.ApparentBytes != 128*1024 {
		t.Fatalf("ApparentBytes = %d", info.ApparentBytes)
	}
	// Supported may be false on a filesystem without FIEMAP; that is a valid
	// answer, and the caller must be able to tell it apart from "zero shared".
	if !info.Supported && (info.SharedBytes != 0 || info.ExclusiveBytes != 0) {
		t.Fatal("unsupported result carried nonzero measurements")
	}
}

func TestExtentsOnMissingFile(t *testing.T) {
	if _, err := Extents(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("Extents on a missing file returned no error")
	}
}

func TestCapabilityDescribe(t *testing.T) {
	c := &Capability{
		Dir:       "/views",
		Available: []Strategy{Copy},
		Notes:     []string{"reflink unavailable: not supported"},
	}
	out := c.Describe()
	if out == "" {
		t.Fatal("Describe returned nothing")
	}
	// The chosen strategy and its caveat both have to be visible.
	if !contains(out, "copy") || !contains(out, "not supported") {
		t.Fatalf("Describe output is missing detail:\n%s", out)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
