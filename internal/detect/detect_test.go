package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mk(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(base, p)
		if filepath.Ext(p) == "" {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdentifyComfyUI(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "comfy", "folder_paths.py", "models/checkpoints", "models/loras")

	install, ok := identify(dir)
	if !ok {
		t.Fatal("ComfyUI was not identified")
	}
	if install.Tool != ComfyUI {
		t.Fatalf("tool = %q", install.Tool)
	}
	if len(install.ModelRoots) != 1 || filepath.Base(install.ModelRoots[0]) != "models" {
		t.Fatalf("model roots = %v", install.ModelRoots)
	}
}

// Forge satisfies every A1111 marker as well as its own, so it has to be tested
// first or it is always reported as A1111.
func TestForgeIsNotMisreportedAsA1111(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "webui.py", "modules", "modules_forge", "models/Stable-diffusion")

	install, ok := identify(dir)
	if !ok {
		t.Fatal("Forge was not identified")
	}
	if install.Tool != Forge {
		t.Fatalf("tool = %q, want Forge", install.Tool)
	}
}

func TestIdentifyA1111(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "webui.py", "modules", "models/Stable-diffusion", "models/Lora")

	install, ok := identify(dir)
	if !ok || install.Tool != A1111 {
		t.Fatalf("identify = %+v, %v", install, ok)
	}
}

// A directory that merely contains "Models" is not SwarmUI. Without a second
// marker this detector would claim half the disks it is pointed at.
func TestSwarmUINeedsMoreThanAModelsDirectory(t *testing.T) {
	bare := t.TempDir()
	mk(t, bare, "Models/Stable-Diffusion")
	if install, ok := identify(bare); ok {
		t.Fatalf("a bare Models directory was identified as %s", install.Tool)
	}

	real := t.TempDir()
	mk(t, real, "Models/Stable-Diffusion", "launch-linux.sh")
	install, ok := identify(real)
	if !ok || install.Tool != SwarmUI {
		t.Fatalf("identify = %+v, %v; want SwarmUI", install, ok)
	}
}

// A source checkout with no model directory is not an installation worth
// scanning.
func TestToolWithoutModelsIsNotReported(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "comfy", "folder_paths.py")
	if _, ok := identify(dir); ok {
		t.Fatal("a checkout with no models directory was reported as an install")
	}
}

func TestUnknownDirectoryIsNotIdentified(t *testing.T) {
	dir := t.TempDir()
	mk(t, dir, "models/loras", "README.md")
	if install, ok := identify(dir); ok {
		t.Fatalf("an unmarked directory was identified as %s", install.Tool)
	}
}

// extra_model_paths.yaml is frequently the only place the real library is named.
func TestComfyExtraModelPaths(t *testing.T) {
	dir := t.TempDir()
	library := t.TempDir()
	mk(t, dir, "comfy", "folder_paths.py", "models/loras")

	yaml := "" +
		"# a comment\n" +
		"comfyui:\n" +
		"    base_path: " + library + "\n" +
		"    checkpoints: models/checkpoints\n" +
		"    loras: models/loras\n"
	if err := os.WriteFile(filepath.Join(dir, "extra_model_paths.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	install, ok := identify(dir)
	if !ok {
		t.Fatal("not identified")
	}
	found := false
	for _, r := range install.ModelRoots {
		if r == filepath.Clean(library) {
			found = true
		}
	}
	if !found {
		t.Fatalf("base_path from extra_model_paths.yaml was not picked up: %v", install.ModelRoots)
	}
	if len(install.Notes) == 0 {
		t.Error("no note recorded about where the extra root came from")
	}
}

func TestParseSimpleYAML(t *testing.T) {
	content := "" +
		"# comment\n" +
		"\n" +
		"comfyui:\n" +
		"    base_path: /mnt/models\n" +
		"    loras: models/loras\n" +
		"other:\n" +
		"    base_path: \"/quoted/path\"\n" +
		"garbage line without a colon\n"

	sections := parseSimpleYAML(content)
	if len(sections) != 2 {
		t.Fatalf("%d sections, want 2", len(sections))
	}
	if sections[0].name != "comfyui" || sections[0].values["base_path"] != "/mnt/models" {
		t.Fatalf("first section = %+v", sections[0])
	}
	if sections[1].values["base_path"] != "/quoted/path" {
		t.Fatalf("quotes were not stripped: %+v", sections[1])
	}
}

// Two tools sharing a model directory is the normal case, and the scanner
// rejects overlapping roots outright -- so this has to be resolved before it
// ever sees the list.
func TestCollapseRootsRemovesNestingAndDuplicates(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "models")
	inner := filepath.Join(outer, "loras")
	sibling := filepath.Join(base, "models2")

	got := collapseRoots([]string{inner, outer, outer, sibling})
	if len(got) != 2 {
		t.Fatalf("collapseRoots = %v, want the outer root and the sibling", got)
	}
	for _, r := range got {
		if r == inner {
			t.Fatalf("a nested root survived: %v", got)
		}
	}
	// A sibling sharing a prefix is not nested.
	foundSibling := false
	for _, r := range got {
		if r == sibling {
			foundSibling = true
		}
	}
	if !foundSibling {
		t.Fatalf("models2 was wrongly collapsed into models: %v", got)
	}
}

func TestModelRootsFlattensInstalls(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared-models")

	roots := ModelRoots([]Install{
		{Tool: ComfyUI, ModelRoots: []string{shared, filepath.Join(base, "a")}},
		{Tool: A1111, ModelRoots: []string{shared, filepath.Join(base, "b")}},
	})
	if len(roots) != 3 {
		t.Fatalf("roots = %v, want 3 with the shared directory once", roots)
	}
}

func TestIsUnder(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a" + sep + "b", "/a", true},
		{"/a", "/a", true},
		{"/ab", "/a", false},
	}
	for _, c := range cases {
		child := filepath.FromSlash(c.child)
		parent := filepath.FromSlash(c.parent)
		if got := isUnder(child, parent); got != c.want {
			t.Errorf("isUnder(%q,%q) = %v, want %v", child, parent, got, c.want)
		}
	}
}

// Detect must never panic or hang on a machine with none of these tools, which
// is the state of every CI runner it will ever run on.
func TestDetectOnACleanMachine(t *testing.T) {
	t.Setenv("MM_SEARCH_PATHS", t.TempDir())
	installs := Detect()
	for _, i := range installs {
		if i.Tool == "" || i.Path == "" {
			t.Fatalf("malformed install: %+v", i)
		}
	}
}

// The override exists for anyone whose layout this cannot guess.
func TestSearchPathOverrideIsHonored(t *testing.T) {
	base := t.TempDir()
	install := filepath.Join(base, "MyComfy")
	mk(t, install, "comfy", "folder_paths.py", "models/loras")

	t.Setenv("MM_SEARCH_PATHS", base)
	found := Detect()

	for _, i := range found {
		if i.Path == install && i.Tool == ComfyUI {
			return
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("path list separator differs; covered by the unit tests above")
	}
	t.Fatalf("override path was not searched; found %+v", found)
}
