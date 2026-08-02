package store

// User preferences, stored server-side.
//
// These live in the database rather than in the browser's localStorage because
// the same daemon is reached from the desktop and from a phone over the
// tailnet. A filter set configured on one is meant to be the same view on the
// other, and the project's stated position is that the database is the single
// authority -- a preference that exists only in one browser profile is a second
// authority that disagrees.
//
// Values are JSON. The table is deliberately generic so that adding a
// preference is a write rather than a migration.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Setting keys in use. Kept here rather than spread across packages so the set
// of things the app remembers is readable in one place.
const (
	// SettingLibraryFilters is the saved library view: selected types, base
	// models, tags, presence, and sort.
	SettingLibraryFilters = "library.filters"

	// SettingDefaultDownloadRoot is the canonical path of the root new
	// downloads default to.
	SettingDefaultDownloadRoot = "downloads.default_root"

	// SettingFolderMap is the per-(root, type) download subfolder map,
	// serialized as {"<root path>": {"<type>": "<subfolder>"}}.
	SettingFolderMap = "downloads.folder_map"

	// SettingComfyOutputDir is a ComfyUI output directory to offer generated
	// images from when choosing a thumbnail.
	SettingComfyOutputDir = "thumbnails.comfy_output_dir"

	// SettingComfyURL is where a running ComfyUI is listening. Setting it is
	// what turns rendering on: with no address there is no service to ask, and
	// the app will not go looking for one.
	SettingComfyURL = "thumbnails.comfy_url"

	// SettingComfyWorkflow is the API-format workflow used to render a
	// thumbnail, with {{placeholders}} filled per model.
	SettingComfyWorkflow = "thumbnails.comfy_workflow"

	// SettingComfyWorkflowDir is a directory of saved ComfyUI workflows to pick
	// from, normally <ComfyUI>/user/default/workflows. Naming a file beats
	// pasting JSON: the file stays editable in ComfyUI, and the next render
	// picks the edit up.
	SettingComfyWorkflowDir = "thumbnails.comfy_workflow_dir"

	// SettingComfyCheckpoint is the base checkpoint a lora preview renders on.
	// A lora cannot render anything by itself and this app cannot guess which
	// checkpoint you have, so it is configured rather than inferred.
	SettingComfyCheckpoint = "thumbnails.comfy_checkpoint"
)

// ErrNoSetting means the key has never been written.
var ErrNoSetting = errors.New("store: setting not set")

// GetSetting returns the raw JSON stored under key.
func (s *Store) GetSetting(key string) (json.RawMessage, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM setting WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoSetting, key)
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading setting %s: %w", key, err)
	}
	return json.RawMessage(value), nil
}

// GetSettingInto decodes the setting into v. A key that was never written is
// not an error: v is left at its zero value and ok is false. Callers overwhelm-
// ingly want "use the default if unset", and forcing each one to distinguish
// ErrNoSetting from a real failure is how defaults end up silently skipped.
func (s *Store) GetSettingInto(key string, v any) (bool, error) {
	raw, err := s.GetSetting(key)
	if errors.Is(err, ErrNoSetting) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		// A value this binary cannot parse is treated as unset rather than
		// fatal: a preference written by a newer version must never be able to
		// stop the library from opening.
		return false, nil
	}
	return true, nil
}

// PutSetting stores v as JSON under key.
func (s *Store) PutSetting(key string, v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encoding setting %s: %w", key, err)
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err = s.db.Exec(`
        INSERT INTO setting (key, value, updated_at) VALUES (?, ?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value,
                                       updated_at = excluded.updated_at`,
		key, string(encoded), nowUTC())
	if err != nil {
		return fmt.Errorf("store: writing setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting forgets a key, restoring the built-in default.
func (s *Store) DeleteSetting(key string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM setting WHERE key = ?`, key)
	return err
}

// AllSettings returns every stored setting as raw JSON, for the settings UI to
// load in one request.
func (s *Store) AllSettings() (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT key, value FROM setting`)
	if err != nil {
		return nil, fmt.Errorf("store: listing settings: %w", err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}
