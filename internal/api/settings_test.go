package api

import (
	"net/http"
	"testing"

	"github.com/socrasteeze/model-manager/internal/store"
)

// Every key the UI writes has to be on the allowlist. Forgetting one is a
// silent 400 on save -- the control appears to work, the value never lands,
// and nothing in the UI says so. Enumerated here so adding a setting without
// allowing it fails a test rather than shipping.
func TestEverySettingKeyIsAllowed(t *testing.T) {
	keys := []string{
		store.SettingLibraryFilters,
		store.SettingDefaultDownloadRoot,
		store.SettingFolderMap,
		store.SettingComfyOutputDir,
		store.SettingComfyURL,
		store.SettingComfyWorkflow,
		store.SettingComfyWorkflowDir,
		store.SettingComfyCheckpoint,
		store.SettingThumbAspect,
		store.SettingBrowseIncludeNSFW,
		store.SettingVersionGrouping,
	}
	for _, k := range keys {
		if !settingKeyAllowed(k) {
			t.Errorf("setting %q is not on the allowlist; writes to it will 400", k)
		}
	}

	// The allowlist exists because the table is generic: an endpoint that
	// accepts any key lets a caller write unbounded rows into the database.
	if settingKeyAllowed("anything.else") {
		t.Error("an unknown key was allowed")
	}
}

// The three new preferences must survive a round trip through the real
// endpoints, not just the allowlist check.
func TestNewPreferencesRoundTrip(t *testing.T) {
	s := newServer(t, nil)

	cases := map[string]string{
		store.SettingThumbAspect:       `"2/3"`,
		store.SettingBrowseIncludeNSFW: `false`,
		store.SettingVersionGrouping:   `"model"`,
	}

	for key, value := range cases {
		w := do(s, "PUT", "http://127.0.0.1/api/settings/"+key, value, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s returned %d: %s", key, w.Code, w.Body.String())
		}
	}

	all, err := s.cfg.Store.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range cases {
		got, ok := all[key]
		if !ok {
			t.Errorf("%s was not stored", key)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}

	// Deleting restores the built-in default, which is how a text setting is
	// reset elsewhere in the UI.
	for key := range cases {
		if w := do(s, "DELETE", "http://127.0.0.1/api/settings/"+key, "", nil); w.Code != http.StatusOK {
			t.Errorf("DELETE %s returned %d", key, w.Code)
		}
	}
	all, err = s.cfg.Store.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	for key := range cases {
		if _, ok := all[key]; ok {
			t.Errorf("%s survived a delete", key)
		}
	}
}
