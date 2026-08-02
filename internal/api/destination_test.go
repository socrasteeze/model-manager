package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/socrasteeze/model-manager/internal/modeltype"
	"github.com/socrasteeze/model-manager/internal/store"
)

// A download of a known type lands in the folder the root's tool uses for it,
// without the client having said anything about directories.
func TestDestinationResolvesPerRootAndType(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := serverWithRoot(t, root, nil)

	// The root announces itself as ComfyUI by holding a `checkpoints` folder,
	// so a lora belongs in `loras`, not `Lora` and not `loras`-pluralized-by-
	// the-browser.
	if got := s.subfolderFor(root, "LORA"); got != "loras" {
		t.Errorf("lora under a ComfyUI root = %q, want \"loras\"", got)
	}
	if got := s.subfolderFor(root, "Checkpoint"); got != "checkpoints" {
		t.Errorf("checkpoint under a ComfyUI root = %q, want \"checkpoints\"", got)
	}
}

func TestDestinationHonoursTheConfiguredMapOverTheDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := serverWithRoot(t, root, nil)

	if err := s.cfg.Store.PutSetting(store.SettingFolderMap, map[string]map[string]string{
		root: {modeltype.LoRA: "my-loras/style"},
	}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("my-loras", "style")
	if got := s.subfolderFor(root, "lora"); got != want {
		t.Errorf("configured map ignored: got %q, want %q", got, want)
	}
}

// The bug this stage exists to close. An unrecognised provider type must fall
// back to the root, never become a directory named after itself.
func TestUnknownTypeFallsBackToTheRootRatherThanInventingAFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := serverWithRoot(t, root, nil)

	for _, weird := range []string{"Workflows", "Poses", "MotionModule", "../escape"} {
		if got := s.subfolderFor(root, weird); got != "" {
			t.Errorf("type %q produced subfolder %q; it must produce none", weird, got)
		}
	}

	destDir, _, err := s.resolveDestination(root, s.subfolderFor(root, "Workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if destDir != root {
		t.Errorf("unknown type landed in %q, want the root %q", destDir, root)
	}
}

// A configured folder is still a path segment joined onto a model root, so it
// goes through the same traversal stripping every other subdir does.
func TestConfiguredFolderCannotEscapeTheRoot(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, nil)

	if err := s.cfg.Store.PutSetting(store.SettingFolderMap, map[string]map[string]string{
		root: {modeltype.LoRA: "../../../etc"},
	}); err != nil {
		t.Fatal(err)
	}
	sub := s.subfolderFor(root, "lora")
	if strings.Contains(sub, "..") {
		t.Fatalf("traversal survived into the subfolder: %q", sub)
	}
	destDir, _, err := s.resolveDestination(root, sub)
	if err != nil {
		t.Fatal(err)
	}
	if !withinRoot(root, destDir) {
		t.Errorf("configured folder escaped the root: %q", destDir)
	}
}

func TestDestinationEndpointReportsTheResolvedPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "StableDiffusion"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := serverWithRoot(t, root, nil)

	req := httptest.NewRequest(http.MethodGet,
		"http://localhost/api/downloads/destination?root="+root+"&type=LORA", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Root    string `json:"root"`
		Subdir  string `json:"subdir"`
		DestDir string `json:"dest_dir"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subdir != "Lora" {
		t.Errorf("subdir = %q, want Lora (Stability Matrix vocabulary)", got.Subdir)
	}
	if got.DestDir != filepath.Join(root, "Lora") {
		t.Errorf("dest_dir = %q", got.DestDir)
	}
	if got.Type != modeltype.LoRA {
		t.Errorf("type = %q, want %q", got.Type, modeltype.LoRA)
	}
}

// A default root that was removed or disabled must not stop downloads: fall
// back to a live root rather than refusing until the user notices the setting.
func TestStaleDefaultRootFallsBack(t *testing.T) {
	root := t.TempDir()
	s := serverWithRoot(t, root, nil)

	if err := s.cfg.Store.PutSetting(store.SettingDefaultDownloadRoot,
		filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	got, err := s.defaultDownloadRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Errorf("got %q, want the surviving root %q", got, root)
	}
}
