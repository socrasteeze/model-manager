package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func rootsTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "master.db"), Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A trailing slash is the single most likely way a user hands the same folder
// to the app twice. Both spellings must land on one row: SweepAbsentPaths
// matches root exactly, so a fork strands every file under it at present=1.
func TestAddRootCanonicalizesTrailingSlash(t *testing.T) {
	st := rootsTestStore(t)
	dir := t.TempDir()

	first, err := st.AddRoot(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRoot(dir+string(filepath.Separator), "", ""); !errors.Is(err, ErrRootExists) {
		t.Fatalf("trailing slash forked the root: %v", err)
	}
	roots, err := st.ListRoots()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].Path != first.Path {
		t.Fatalf("got %+v, want exactly one root at %s", roots, first.Path)
	}
}

func TestAddRootResolvesSymlinks(t *testing.T) {
	st := rootsTestStore(t)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := st.AddRoot(link, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got.Path != want {
		t.Errorf("stored %q, want the resolved target %q", got.Path, want)
	}
	// The same directory reached through the link is not a second root.
	if _, err := st.AddRoot(real, "", ""); !errors.Is(err, ErrRootExists) {
		t.Fatalf("a symlink and its target became two roots: %v", err)
	}
}

// Overlap is refused, not merged: the present-sweep is scoped per root, so a
// file reachable under two roots is swept by whichever root did not claim it
// and flaps present/absent on every scan.
func TestAddRootRejectsNesting(t *testing.T) {
	st := rootsTestStore(t)
	outer := t.TempDir()
	inner := filepath.Join(outer, "loras")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := st.AddRoot(outer, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRoot(inner, "", ""); !errors.Is(err, ErrRootNested) {
		t.Fatalf("accepted a root nested inside a managed root: %v", err)
	}

	// And the other direction: adding a parent of a managed root.
	st2 := rootsTestStore(t)
	if _, err := st2.AddRoot(inner, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.AddRoot(outer, "", ""); !errors.Is(err, ErrRootNested) {
		t.Fatalf("accepted a root containing a managed root: %v", err)
	}
}

func TestPathIsUnderComparesWholeSegments(t *testing.T) {
	parent := filepath.Join(string(filepath.Separator), "srv", "models")
	sibling := filepath.Join(string(filepath.Separator), "srv", "models2")
	child := filepath.Join(parent, "loras")

	if PathIsUnder(sibling, parent) {
		t.Errorf("%s treated as living under %s", sibling, parent)
	}
	if !PathIsUnder(child, parent) {
		t.Errorf("%s not recognised as under %s", child, parent)
	}
	if !PathIsUnder(parent, parent) {
		t.Error("a root is not under itself")
	}
}

// Removing a root must never touch the disk, and must keep the model records:
// re-adding the folder later should restore the library rather than re-derive
// it from scratch.
func TestRemoveRootKeepsModelsAndFilesOnDisk(t *testing.T) {
	st := rootsTestStore(t)
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "a.safetensors")
	if err := os.WriteFile(onDisk, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := st.AddRoot(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.BeginScanRun(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileAndPath(
		ModelFile{SHA256: "aaa", ProbeSHA256: "p", Size: 7, Format: "safetensors"},
		FilePath{SHA256: "aaa", Path: onDisk, Root: root.Path,
			Device: 1, Inode: 1, Size: 7, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	if err := st.RemoveRoot(root.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("removing a root touched the disk: %v", err)
	}
	var models, present int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM model_file`).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if models != 1 {
		t.Errorf("model record lost on root removal: got %d", models)
	}
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM model_file_path WHERE present = 1`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Errorf("paths under a removed root still claim present: got %d", present)
	}
}

func TestDisabledRootIsNotOfferedForScanOrDownload(t *testing.T) {
	st := rootsTestStore(t)
	root, err := st.AddRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRootEnabled(root.ID, false); err != nil {
		t.Fatal(err)
	}
	paths, err := st.EnabledRootPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("disabled root still offered: %v", paths)
	}
}

func TestSettingsRoundTripAndDefaults(t *testing.T) {
	st := rootsTestStore(t)

	type filters struct {
		Types []string `json:"types"`
		Sort  string   `json:"sort"`
	}

	// Unset is not an error: callers want "use the default", and making each
	// one distinguish not-set from broken is how defaults get skipped.
	var got filters
	ok, err := st.GetSettingInto(SettingLibraryFilters, &got)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("reported a value for a key never written")
	}

	want := filters{Types: []string{"lora", "checkpoint"}, Sort: "name"}
	if err := st.PutSetting(SettingLibraryFilters, want); err != nil {
		t.Fatal(err)
	}
	ok, err = st.GetSettingInto(SettingLibraryFilters, &got)
	if err != nil || !ok {
		t.Fatalf("read back failed: ok=%v err=%v", ok, err)
	}
	if got.Sort != want.Sort || len(got.Types) != 2 {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Overwrite, then reset to the built-in default.
	if err := st.PutSetting(SettingLibraryFilters, filters{Sort: "size"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSetting(SettingLibraryFilters); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.GetSettingInto(SettingLibraryFilters, &got); ok {
		t.Error("delete did not restore the unset state")
	}
}

// A preference written by a newer build must never stop the library opening.
func TestUnparseableSettingReadsAsUnset(t *testing.T) {
	st := rootsTestStore(t)
	if err := st.PutSetting(SettingLibraryFilters, "a string, not the object we expect"); err != nil {
		t.Fatal(err)
	}
	var into struct {
		Types []string `json:"types"`
	}
	ok, err := st.GetSettingInto(SettingLibraryFilters, &into)
	if err != nil {
		t.Fatalf("a shape mismatch became a hard error: %v", err)
	}
	if ok {
		t.Error("claimed to have decoded a value it could not decode")
	}
}
