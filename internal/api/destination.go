package api

// Where a download of a given type lands, decided by the server.
//
// The browser used to decide this: BrowsePanel pluralized the provider's type
// string into `${type}s` and sent it as a subdirectory. That is wrong twice
// over. It fabricates folder names from strings nothing validated -- `vaes/`,
// `lycoriss/`, and whatever a provider invents next -- and it assumes one
// vocabulary, when the user's own machine has three tools that name the same
// folder three different ways.
//
// So the policy lives here, next to the containment check that already had to
// be trusted, and the UI displays the resolved destination rather than choosing
// it.

import (
	"net/http"
	"strings"

	"github.com/socrasteeze/model-manager/internal/modeltype"
	"github.com/socrasteeze/model-manager/internal/store"
)

// folderMap is the user's configured (root path -> type -> subfolder) map.
type folderMap map[string]map[string]string

func (s *Server) folderMap() folderMap {
	var m folderMap
	if _, err := s.cfg.Store.GetSettingInto(store.SettingFolderMap, &m); err != nil || m == nil {
		return folderMap{}
	}
	return m
}

// subfolderFor resolves the subdirectory a model of this type belongs in under
// this root.
//
// Order: the user's configured map, then the built-in vocabulary of whatever
// tool the root belongs to, then nothing -- meaning the root itself. "Nothing"
// is a real answer, not a failure: an unrecognised type must land somewhere the
// user will find it, not in a directory invented from the type's name.
func (s *Server) subfolderFor(rootPath, modelType string) string {
	t := modeltype.Normalize(modelType)
	if t == "" {
		return ""
	}
	if byType, ok := s.folderMap()[rootPath]; ok {
		if sub, ok := byType[t]; ok {
			// An explicitly configured empty string means "the root itself",
			// and is honoured rather than falling through to the default.
			return cleanSubdir(sub)
		}
	}

	tool := ""
	if root, err := s.cfg.Store.RootByPath(rootPath); err == nil {
		tool = root.Tool
	}
	if tool == "" {
		tool = modeltype.InferTool(rootPath, modeltype.DirExists)
	}
	return cleanSubdir(modeltype.DefaultFolder(tool, t))
}

// handleResolveDestination answers GET /api/downloads/destination?root=&type=.
//
// The Browse tab calls it so the user can see exactly where a file will land
// before pressing Download. Showing the resolved path is the point: a
// destination the user cannot see is a destination they cannot object to.
func (s *Server) handleResolveDestination(w http.ResponseWriter, r *http.Request) {
	rawRoot := strings.TrimSpace(r.URL.Query().Get("root"))
	modelType := r.URL.Query().Get("type")

	if rawRoot == "" {
		// No root named: answer for the configured default, which is what the
		// UI shows before the user has touched the picker.
		def, err := s.defaultDownloadRoot()
		if err != nil || def == "" {
			writeError(w, http.StatusBadRequest, "no destination root given")
			return
		}
		rawRoot = def
	}

	sub := ""
	if root, err := store.CanonicalRoot(rawRoot); err == nil {
		sub = s.subfolderFor(root, modelType)
	}

	destDir, matchedRoot, err := s.resolveDestination(rawRoot, sub)
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid destination", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root":     matchedRoot,
		"subdir":   sub,
		"dest_dir": destDir,
		"type":     modeltype.Normalize(modelType),
	})
}

// defaultDownloadRoot returns the root new downloads target when the caller
// names none: the configured one if it is still managed, otherwise the first
// managed root.
func (s *Server) defaultDownloadRoot() (string, error) {
	roots, err := s.scannedRoots()
	if err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", nil
	}
	var configured string
	if ok, _ := s.cfg.Store.GetSettingInto(store.SettingDefaultDownloadRoot, &configured); ok {
		for _, r := range roots {
			if r == configured {
				return r, nil
			}
		}
		// The configured root was removed or disabled. Falling back beats
		// refusing every download until the user notices the setting is stale.
	}
	return roots[0], nil
}

// handleFolderDefaults publishes the built-in per-tool vocabularies, so the
// settings UI can show what it would do before anything is configured.
func (s *Server) handleFolderDefaults(w http.ResponseWriter, r *http.Request) {
	defaults := map[string]map[string]string{}
	for _, tool := range modeltype.Tools() {
		byType := map[string]string{}
		for _, t := range modeltype.All {
			byType[t] = modeltype.DefaultFolder(tool, t)
		}
		defaults[tool] = byType
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"types":    modeltype.All,
		"tools":    modeltype.Tools(),
		"defaults": defaults,
	})
}
