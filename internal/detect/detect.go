// Package detect finds existing SD tool installations and their model roots.
//
// Spec §3 makes this a hard requirement rather than a convenience: "zero-config
// first run -- auto-detect existing ComfyUI / SwarmUI / Stability Matrix / A1111
// / Forge installs and their model roots, or adoption dies at the setup screen."
//
// Everything here is read-only and best-effort. A tool it fails to find costs
// the user one --root flag; a tool it misidentifies would point the scanner at
// the wrong tree, so every detector requires a positive marker rather than
// inferring from a directory name.
package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Tool names.
const (
	ComfyUI         = "ComfyUI"
	SwarmUI         = "SwarmUI"
	StabilityMatrix = "Stability Matrix"
	A1111           = "A1111 WebUI"
	Forge           = "Forge"
	InvokeAI        = "InvokeAI"
	Fooocus         = "Fooocus"
)

// Install is one detected tool.
type Install struct {
	Tool string `json:"tool"`
	Path string `json:"path"`

	// ModelRoots are directories worth scanning. Nested roots are collapsed, so
	// this can be passed straight to the scanner.
	ModelRoots []string `json:"model_roots"`

	// Notes records anything the user should know, such as extra roots picked up
	// from a config file.
	Notes []string `json:"notes,omitempty"`
}

// marker is a file or directory whose presence identifies a tool.
type detector struct {
	tool string

	// markers must all be present for a directory to be that tool. Requiring a
	// positive marker is what stops a folder called "ComfyUI" that holds
	// something else from being scanned as one.
	markers []string

	// modelSubdirs are checked relative to the install root.
	modelSubdirs []string
}

var detectors = []detector{
	{
		tool:         ComfyUI,
		markers:      []string{"comfy", "folder_paths.py"},
		modelSubdirs: []string{"models"},
	},
	{
		tool: SwarmUI,
		// SwarmUI capitalizes Models; the launch script distinguishes it from a
		// bare directory someone happened to name the same thing.
		markers:      []string{"Models"},
		modelSubdirs: []string{"Models"},
	},
	{
		tool: Forge,
		// Forge is an A1111 fork, so it must be tested first -- it satisfies
		// every A1111 marker as well as its own.
		markers:      []string{"modules_forge", "webui.py"},
		modelSubdirs: []string{"models"},
	},
	{
		tool:         A1111,
		markers:      []string{"webui.py", "modules"},
		modelSubdirs: []string{"models"},
	},
	{
		tool:         InvokeAI,
		markers:      []string{"invokeai.yaml"},
		modelSubdirs: []string{"models", "autoimport"},
	},
	{
		tool:         Fooocus,
		markers:      []string{"fooocus_version.py"},
		modelSubdirs: []string{"models"},
	},
}

// swarmMarkers are additional files that confirm a SwarmUI directory, since
// "Models" alone is far too weak a signal.
var swarmMarkers = []string{"SwarmUI.sln", "SwarmUI.csproj", "launch-linux.sh",
	"launch-windows.bat", "SwarmUI.exe", "src"}

// Detect searches the usual locations and returns what it finds.
func Detect() []Install {
	var found []Install
	seen := map[string]bool{}

	for _, base := range searchRoots() {
		for _, candidate := range candidatesUnder(base) {
			abs, err := filepath.Abs(candidate)
			if err != nil || seen[abs] {
				continue
			}
			if install, ok := identify(abs); ok {
				seen[abs] = true
				found = append(found, install)
			}
		}
	}

	found = append(found, detectStabilityMatrix(seen)...)

	sort.Slice(found, func(i, j int) bool {
		if found[i].Tool != found[j].Tool {
			return found[i].Tool < found[j].Tool
		}
		return found[i].Path < found[j].Path
	})
	return found
}

// identify decides whether a directory is a known tool.
func identify(dir string) (Install, bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return Install{}, false
	}

	for _, d := range detectors {
		if !hasAll(dir, d.markers) {
			continue
		}
		if d.tool == SwarmUI && !hasAny(dir, swarmMarkers) {
			// A bare `Models` directory is not SwarmUI.
			continue
		}

		install := Install{Tool: d.tool, Path: dir}
		for _, sub := range d.modelSubdirs {
			candidate := filepath.Join(dir, sub)
			if isDir(candidate) {
				install.ModelRoots = append(install.ModelRoots, candidate)
			}
		}
		if d.tool == ComfyUI {
			extra, notes := comfyExtraPaths(dir)
			install.ModelRoots = append(install.ModelRoots, extra...)
			install.Notes = append(install.Notes, notes...)
		}
		if len(install.ModelRoots) == 0 {
			// A tool with no model directory is a source checkout, not an
			// installation worth scanning.
			continue
		}
		install.ModelRoots = collapseRoots(install.ModelRoots)
		return install, true
	}
	return Install{}, false
}

// detectStabilityMatrix handles the one tool that keeps its models in a data
// directory rather than beside its own code.
func detectStabilityMatrix(seen map[string]bool) []Install {
	var out []Install
	for _, dir := range stabilityMatrixDirs() {
		if dir == "" || seen[dir] {
			continue
		}
		models := filepath.Join(dir, "Models")
		if !isDir(models) {
			continue
		}
		seen[dir] = true
		out = append(out, Install{
			Tool:       StabilityMatrix,
			Path:       dir,
			ModelRoots: []string{models},
		})
	}
	return out
}

// comfyExtraPaths reads extra_model_paths.yaml, which is where users point
// ComfyUI at a library that lives somewhere else entirely -- frequently the only
// place the real model tree is named.
func comfyExtraPaths(dir string) (roots []string, notes []string) {
	data, err := os.ReadFile(filepath.Join(dir, "extra_model_paths.yaml"))
	if err != nil {
		return nil, nil
	}

	for _, section := range parseSimpleYAML(string(data)) {
		base := section.values["base_path"]
		if base != "" && isDir(base) {
			roots = append(roots, base)
			notes = append(notes, "extra_model_paths.yaml: "+section.name+" -> "+base)
			continue
		}
		// Without a base_path the entries are absolute paths in their own right.
		for key, val := range section.values {
			if key == "base_path" || val == "" {
				continue
			}
			if filepath.IsAbs(val) && isDir(val) {
				roots = append(roots, val)
				notes = append(notes, "extra_model_paths.yaml: "+section.name+"."+key+" -> "+val)
			}
		}
	}
	return roots, notes
}

// yamlSection is one top-level key and its immediate children.
type yamlSection struct {
	name   string
	values map[string]string
}

// parseSimpleYAML handles the two-level shape ComfyUI's extra_model_paths.yaml
// actually uses:
//
//	comfyui:
//	    base_path: /mnt/models
//	    loras: models/loras
//
// A real YAML parser would be a dependency carried solely for this one file, and
// this format is fixed enough that hand-parsing it is the smaller risk. Anything
// it cannot understand is skipped rather than guessed at.
func parseSimpleYAML(content string) []yamlSection {
	var sections []yamlSection
	var current *yamlSection

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimRight(rawLine, " \t\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		trimmed := strings.TrimSpace(line)
		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])
		value = strings.Trim(value, `"'`)

		if !indented {
			sections = append(sections, yamlSection{name: key, values: map[string]string{}})
			current = &sections[len(sections)-1]
			continue
		}
		if current == nil || value == "" {
			continue
		}
		// A multi-line value list is not a path; take only the first entry.
		if i := strings.IndexAny(value, "\n"); i >= 0 {
			value = value[:i]
		}
		current.values[key] = expandHome(value)
	}
	return sections
}

// ModelRoots flattens every detected install's roots into one deduplicated,
// non-overlapping list ready for the scanner.
func ModelRoots(installs []Install) []string {
	var all []string
	for _, i := range installs {
		all = append(all, i.ModelRoots...)
	}
	return collapseRoots(all)
}

// collapseRoots removes duplicates and any root nested inside another.
//
// The scanner rejects overlapping roots outright (its per-root sweep would be
// ambiguous), and two tools sharing a model directory is the normal case rather
// than the exception -- so this has to be resolved here, before the scanner ever
// sees the list.
func collapseRoots(roots []string) []string {
	cleaned := map[string]bool{}
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		cleaned[filepath.Clean(abs)] = true
	}

	list := make([]string, 0, len(cleaned))
	for r := range cleaned {
		list = append(list, r)
	}
	sort.Strings(list)

	var out []string
	for _, candidate := range list {
		nested := false
		for _, kept := range out {
			if isUnder(candidate, kept) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, candidate)
		}
	}
	return out
}

func isUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

func hasAll(dir string, names []string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return false
		}
	}
	return len(names) > 0
}

func hasAny(dir string, names []string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// candidatesUnder returns base plus its immediate children.
//
// The search is deliberately shallow. Walking deeply from a home directory would
// be slow, would wander into network mounts, and would turn a first run into an
// unexplained multi-minute pause -- which is the setup-screen failure §3 warns
// about, arriving by a different route.
func candidatesUnder(base string) []string {
	if !isDir(base) {
		return nil
	}
	out := []string{base}

	entries, err := os.ReadDir(base)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		child := filepath.Join(base, e.Name())
		out = append(out, child)

		// One more level, but only for directories whose name suggests they hold
		// tools. This catches ~/apps/ComfyUI without walking the whole home
		// directory.
		if looksLikeToolContainer(e.Name()) {
			if grandchildren, err := os.ReadDir(child); err == nil {
				for _, g := range grandchildren {
					if g.IsDir() && !strings.HasPrefix(g.Name(), ".") {
						out = append(out, filepath.Join(child, g.Name()))
					}
				}
			}
		}
	}
	return out
}

func looksLikeToolContainer(name string) bool {
	switch strings.ToLower(name) {
	case "apps", "ai", "tools", "sd", "stable-diffusion", "src", "git",
		"projects", "programs", "opt", "data":
		return true
	}
	return false
}

// searchRoots is where installations plausibly live on this platform.
func searchRoots() []string {
	var roots []string
	home, err := os.UserHomeDir()
	if err == nil {
		roots = append(roots, home)
	}

	switch runtime.GOOS {
	case "windows":
		if home != "" {
			roots = append(roots, filepath.Join(home, "Documents"), filepath.Join(home, "Desktop"))
		}
		// Drive letters, because a model library is exactly the kind of thing
		// that lives on the second disk.
		for _, drive := range []string{"C:\\", "D:\\", "E:\\", "F:\\"} {
			if isDir(drive) {
				roots = append(roots, drive)
			}
		}
	case "darwin":
		if home != "" {
			roots = append(roots, filepath.Join(home, "Applications"), filepath.Join(home, "Documents"))
		}
		roots = append(roots, "/Applications")
	default:
		if home != "" {
			roots = append(roots, filepath.Join(home, "apps"))
		}
		roots = append(roots, "/opt", "/srv", "/mnt", "/media")
	}

	// An explicit override always wins, for anyone whose layout this cannot
	// guess.
	if extra := os.Getenv("MM_SEARCH_PATHS"); extra != "" {
		roots = append(roots, filepath.SplitList(extra)...)
	}
	return roots
}

// stabilityMatrixDirs is where Stability Matrix keeps its data directory.
func stabilityMatrixDirs() []string {
	var out []string
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			out = append(out, filepath.Join(appData, "StabilityMatrix"))
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out, filepath.Join(local, "StabilityMatrix"))
		}
	case "darwin":
		if home != "" {
			out = append(out, filepath.Join(home, "Library", "Application Support", "StabilityMatrix"))
		}
	default:
		if home != "" {
			out = append(out,
				filepath.Join(home, ".local", "share", "StabilityMatrix"),
				filepath.Join(home, "StabilityMatrix"))
		}
	}

	// The portable build keeps its data beside the executable, so the same
	// shallow sweep used for everything else applies.
	for _, base := range searchRoots() {
		out = append(out, filepath.Join(base, "StabilityMatrix", "Data"),
			filepath.Join(base, "Data"))
	}
	return out
}
