package api

// User preferences over HTTP.
//
// Preferences are server-side rather than in localStorage because the same
// daemon answers the desktop browser and the phone over the tailnet. A saved
// filter set that only exists in one browser profile is not a saved filter set.

import (
	"encoding/json"
	"net/http"

	"github.com/socrasteeze/model-manager/internal/store"
)

// settingKeyAllowed limits what may be written. The table is generic on
// purpose, but an endpoint that accepts any key is an endpoint that lets a
// caller write unbounded rows into the master database.
func settingKeyAllowed(key string) bool {
	switch key {
	case store.SettingLibraryFilters,
		store.SettingDefaultDownloadRoot,
		store.SettingFolderMap,
		store.SettingComfyOutputDir,
		store.SettingComfyURL,
		store.SettingComfyWorkflow,
		store.SettingComfyWorkflowDir,
		store.SettingComfyCheckpoint:
		return true
	}
	return false
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.cfg.Store.AllSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": all})
}

func (s *Server) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	key := r.PathValue("key")
	if !settingKeyAllowed(key) {
		writeError(w, http.StatusBadRequest, "unknown setting", key)
		return
	}
	var value json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON value", err.Error())
		return
	}
	if err := s.cfg.Store.PutSetting(key, value); err != nil {
		writeError(w, http.StatusInternalServerError, "write failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": value})
}

func (s *Server) handleDeleteSetting(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyGuard(w) {
		return
	}
	key := r.PathValue("key")
	if !settingKeyAllowed(key) {
		writeError(w, http.StatusBadRequest, "unknown setting", key)
		return
	}
	if err := s.cfg.Store.DeleteSetting(key); err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}
