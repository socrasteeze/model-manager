package view

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/link"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

type fixture struct {
	st        *store.Store
	mgr       *Manager
	modelRoot string
	viewRoot  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()

	st, err := store.Open(filepath.Join(base, "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{
		st:        st,
		mgr:       NewManager(st),
		modelRoot: filepath.Join(base, "models"),
		viewRoot:  filepath.Join(base, "views", "by-base"),
	}
	if err := os.MkdirAll(f.modelRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) addModel(t *testing.T, sha, filename, name, typ, baseModel string, tags []string) string {
	t.Helper()

	path := filepath.Join(f.modelRoot, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("model bytes for " + sha)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	run, _ := f.st.BeginScanRun(f.modelRoot)
	if err := f.st.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: info.Size(), Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: path, Root: f.modelRoot,
			Device: 1, Inode: fnv(sha), Size: info.Size(),
			MtimeNs: info.ModTime().UnixNano(), ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	var obs []store.FieldObservation
	if name != "" {
		obs = append(obs, store.FieldObservation{Field: provenance.FieldName, Value: name})
	}
	if typ != "" {
		obs = append(obs, store.FieldObservation{Field: provenance.FieldType, Value: typ})
	}
	if baseModel != "" {
		obs = append(obs, store.FieldObservation{Field: provenance.FieldBaseModel, Value: baseModel})
	}
	if len(obs) > 0 {
		if err := f.st.RecordObservations(sha, provenance.SourceCivitai, obs); err != nil {
			t.Fatal(err)
		}
	}
	if len(tags) > 0 {
		if err := f.st.SetTags(sha, provenance.SourceCivitai, tags); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.ResolveModel(sha); err != nil {
		t.Fatal(err)
	}
	return path
}

func fnv(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestGenerateGroupsByBaseModel(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "Cinematic Style", "lora", "SDXL", nil)
	f.addModel(t, "b", "two.safetensors", "Anime Flux", "lora", "Flux", nil)
	f.addModel(t, "c", "three.safetensors", "", "checkpoint", "", nil)

	if _, err := f.mgr.Create(Definition{
		Name: "by-base", Root: f.viewRoot, GroupBy: GroupBaseModel,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := f.mgr.Generate(context.Background(), "by-base", GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Created != 3 {
		t.Fatalf("created %d, want 3 (errors: %v)", res.Created, res.Errors)
	}

	got := filesUnder(t, f.viewRoot)
	want := map[string]bool{
		"SDXL/Cinematic Style.safetensors": true,
		"Flux/Anime Flux.safetensors":      true,
		// No base model resolved: grouped as unsorted, named from the file.
		"unsorted/three.safetensors": true,
	}
	if len(got) != 3 {
		t.Fatalf("view holds %v, want 3 entries", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
}

// A view regenerated after one model was added must not rewrite the tree.
func TestGenerateIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	first, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 {
		t.Fatalf("first run created %d", first.Created)
	}

	second, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Kept != 1 {
		t.Fatalf("second run created %d kept %d; want 0/1", second.Created, second.Kept)
	}
}

// Removing a model from the library must remove it from the view.
func TestGenerateRemovesEntriesThatNoLongerBelong(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)
	f.addModel(t, "b", "two.safetensors", "Two", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := filesUnder(t, f.viewRoot); len(got) != 2 {
		t.Fatalf("view holds %v", got)
	}

	// Mark one absent, as a scan would after the file disappeared.
	if _, err := f.st.DB().Exec(`UPDATE model_file_path SET present = 0 WHERE sha256 = 'b'`); err != nil {
		t.Fatal(err)
	}

	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed %d, want 1", res.Removed)
	}
	if got := filesUnder(t, f.viewRoot); len(got) != 1 {
		t.Fatalf("view still holds %v", got)
	}
}

// Deleting a view must remove only what this app created. A user who points a
// view at a directory that already holds something must not lose it.
func TestDeleteOnlyRemovesWhatItCreated(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Something the user put there themselves.
	bystander := filepath.Join(f.viewRoot, "my-notes.txt")
	if err := os.WriteFile(bystander, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := f.mgr.Delete("v", true)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d entries, want 1", removed)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatal("deleting the view destroyed a file it did not create")
	}
}

// A view inside a scanned tree would be picked up by the next scan and counted
// as another copy of every model in it.
func TestRefusesToGenerateInsideAModelRoot(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	inside := filepath.Join(f.modelRoot, "views")
	if _, err := f.mgr.Create(Definition{Name: "bad", Root: inside, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	_, err := f.mgr.Generate(context.Background(), "bad", GenerateOptions{})
	if err == nil {
		t.Fatal("a view inside a model root was generated")
	}
	if got := filesUnder(t, inside); len(got) != 0 {
		t.Fatalf("files were created despite the refusal: %v", got)
	}
}

// A probe-bound path has not been confirmed by a full hash, and §10.1 bars those
// from any write-side decision. Generating a view is one.
func TestProvisionalPathsAreNotMaterialized(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)
	if _, err := f.st.DB().Exec(`UPDATE model_file_path SET provisional = 1`); err != nil {
		t.Fatal(err)
	}

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 {
		t.Fatalf("created %d entries from a provisional path", res.Created)
	}
}

func TestFilterNarrowsAView(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "lora.safetensors", "A Lora", "lora", "SDXL", nil)
	f.addModel(t, "b", "ckpt.safetensors", "A Checkpoint", "checkpoint", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{
		Name: "loras-only", Root: f.viewRoot, GroupBy: GroupFlat,
		Filter: store.SearchQuery{Types: []string{"lora"}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "loras-only", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d, want only the LoRA", res.Created)
	}
	got := filesUnder(t, f.viewRoot)
	if len(got) != 1 || got[0] != "A Lora.safetensors" {
		t.Fatalf("view holds %v", got)
	}
}

func TestGroupByTag(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", []string{"style"})
	f.addModel(t, "b", "two.safetensors", "Two", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupTag}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{}); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, g := range filesUnder(t, f.viewRoot) {
		got[g] = true
	}
	if !got["style/One.safetensors"] || !got["untagged/Two.safetensors"] {
		t.Fatalf("view holds %v", got)
	}
}

// Two models can legitimately share a display name; dropping one silently would
// be worse than disambiguating.
func TestNameCollisionsAreDisambiguated(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "aaaaaaaaaa", "one.safetensors", "Same Name", "lora", "SDXL", nil)
	f.addModel(t, "bbbbbbbbbb", "two.safetensors", "Same Name", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 {
		t.Fatalf("created %d, want both models kept", res.Created)
	}
	if got := filesUnder(t, f.viewRoot); len(got) != 2 {
		t.Fatalf("view holds %v", got)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 {
		t.Fatalf("dry run reported %d", res.Created)
	}
	if got := filesUnder(t, f.viewRoot); len(got) != 0 {
		t.Fatalf("dry run created %v", got)
	}
}

// The source is only ever read. A view must never modify the library.
func TestGenerationNeverModifiesTheSource(t *testing.T) {
	f := newFixture(t)
	src := f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	originalBytes, _ := os.ReadFile(src)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{}); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatal("view generation modified the source file")
	}
	nowBytes, _ := os.ReadFile(src)
	if string(originalBytes) != string(nowBytes) {
		t.Fatal("the source file's contents changed")
	}
}

func TestExplicitStrategyIsHonored(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{Strategy: link.Copy})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != link.Copy {
		t.Fatalf("strategy = %s, want copy", res.Strategy)
	}
	// A copy is a genuinely separate file.
	entry := filepath.Join(f.viewRoot, "One.safetensors")
	if _, err := os.Lstat(entry); err != nil {
		t.Fatal(err)
	}
}

// A symlink strategy must carry the SMB warning, and a hardlink the
// write-through warning: these are §9.2 and §9.3 made visible in-product.
func TestWarningsAreSurfaced(t *testing.T) {
	f := newFixture(t)
	f.addModel(t, "a", "one.safetensors", "One", "lora", "SDXL", nil)

	if _, err := f.mgr.Create(Definition{Name: "v", Root: f.viewRoot, GroupBy: GroupFlat}); err != nil {
		t.Fatal(err)
	}
	res, err := f.mgr.Generate(context.Background(), "v", GenerateOptions{Strategy: link.Symlink})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("a symlink view carried no SMB warning")
	}
	if len(link.Warnings(link.Hardlink)) == 0 {
		t.Fatal("the hardlink write-through warning is missing")
	}
}

func TestSanitizeSegment(t *testing.T) {
	cases := map[string]string{
		"SDXL":            "SDXL",
		"a/b":             "a_b",
		`weird:name*here`: "weird_name_here",
		"  spaced  ":      "spaced",
		"":                "unsorted",
		"...":             "unsorted",
		// Windows reserved names create an unopenable file.
		"CON":  "CON_",
		"com1": "com1_",
	}
	for in, want := range cases {
		if got := sanitizeSegment(in); got != want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateAndList(t *testing.T) {
	f := newFixture(t)
	if _, err := f.mgr.Create(Definition{Name: "one", Root: f.viewRoot}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Create(Definition{Name: "", Root: f.viewRoot}); err == nil {
		t.Error("a nameless view was accepted")
	}

	views, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "one" {
		t.Fatalf("views = %+v", views)
	}
	if views[0].Status != "never-generated" {
		t.Errorf("status = %q", views[0].Status)
	}
}
