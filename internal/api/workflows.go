package api

// Pointing at a workflow file instead of pasting one.
//
// A family slot may hold the graph inline, or the *name of a file* holding it.
// Naming a file is the better of the two by some distance: the workflow stays
// where ComfyUI saved it, stays editable in ComfyUI, and the next render picks
// the edit up -- where inline JSON has to be re-pasted every time the graph
// changes, which in practice means it stops being changed.
//
// The path is operator-configured, never client-supplied, which is what makes
// reading it acceptable at all. A named file is resolved inside the configured
// workflow directory; an absolute path is accepted too, since an operator who
// typed a path into their own settings has already chosen it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/store"
)

// maxWorkflowBytes caps a workflow file. A ComfyUI graph is tens of kilobytes
// of JSON; anything past this is not one.
const maxWorkflowBytes = 8 << 20

// workflowDir returns the configured directory of saved workflows.
func (s *Server) workflowDir() (string, error) {
	var configured string
	ok, err := s.cfg.Store.GetSettingInto(store.SettingComfyWorkflowDir, &configured)
	if err != nil || !ok || strings.TrimSpace(configured) == "" {
		return "", errors.New("no workflow folder configured")
	}
	dir, err := filepath.Abs(strings.TrimSpace(configured))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}

// looksLikePath reports whether a slot value names a file rather than holding a
// graph. A graph is JSON and starts with a brace; nothing else does.
func looksLikePath(value string) bool {
	t := strings.TrimSpace(value)
	return t != "" && !strings.HasPrefix(t, "{")
}

// loadWorkflowFile reads a graph from the named file.
//
// Read at render time rather than cached, which is the entire point of naming a
// file: edit the workflow in ComfyUI, press Render, get the new graph.
func (s *Server) loadWorkflowFile(name string) (json.RawMessage, error) {
	path, err := s.resolveWorkflowPath(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("workflow file %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", name)
	}
	if info.Size() > maxWorkflowBytes {
		return nil, fmt.Errorf("%s is %d bytes, too large to be a workflow", name, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", name, err)
	}
	return probe, nil
}

// resolveWorkflowPath turns a slot value into a path on disk.
func (s *Server) resolveWorkflowPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("no workflow named")
	}

	// An absolute path is taken at face value: it is a setting the operator
	// typed, not a value that arrived with a request.
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}

	dir, err := s.workflowDir()
	if err != nil {
		return "", fmt.Errorf("%q is a relative name but %v", name, err)
	}
	// Relative names are still confined to the folder, so a stored setting
	// cannot walk out of it with `..`.
	clean := cleanSubdir(name)
	if clean == "" {
		return "", fmt.Errorf("%q does not name a file", name)
	}
	full := filepath.Join(dir, clean)
	resolved := full
	if r, err := filepath.EvalSymlinks(full); err == nil {
		resolved = r
	}
	if !withinRoot(dir, resolved) {
		return "", fmt.Errorf("%q is outside the workflow folder", name)
	}
	return full, nil
}

// workflowFile describes one saved workflow, and whether it can be used.
type workflowFile struct {
	Name string `json:"name"`
	Rel  string `json:"rel"`

	// APIFormat is false for a graph saved from the ComfyUI canvas without
	// "Save (API Format)". Reported rather than hidden, because "my workflow is
	// not in the list" is a worse puzzle than "this one says it is the wrong
	// format".
	APIFormat bool   `json:"api_format"`
	Note      string `json:"note,omitempty"`

	Warnings []comfy.Warning `json:"warnings,omitempty"`
}

// handleListWorkflows handles GET /api/comfy/workflows.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	dir, err := s.workflowDir()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"dir":       "",
			"workflows": []workflowFile{},
			"error":     err.Error(),
		})
		return
	}

	out := []workflowFile{}
	const maxDepth = 3
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".json") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > maxWorkflowBytes {
			return nil
		}

		entry := workflowFile{Name: d.Name(), Rel: filepath.ToSlash(rel)}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			entry.Note = readErr.Error()
			out = append(out, entry)
			return nil
		}
		warnings := comfy.Lint(json.RawMessage(data))
		entry.APIFormat = true
		for _, warn := range warnings {
			if warn.Code == "not_api_format" {
				entry.APIFormat = false
				entry.Note = warn.Message
			}
		}
		if entry.APIFormat {
			entry.Warnings = warnings
		}
		out = append(out, entry)
		return nil
	})

	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "workflows": out})
}
