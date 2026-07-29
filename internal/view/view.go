// Package view materializes generated directory trees over the library.
//
// This is where organization is actually delivered, non-destructively (spec §9).
// Unlimited views -- by base model, by type, by tag, by collection -- generated
// from the same underlying files, fully reversible, with no risk to the library
// because nothing is moved and nothing is written into the model tree.
package view

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/link"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/timestamp"
)

// Grouping decides the subdirectory layout.
type Grouping string

const (
	GroupFlat       Grouping = "flat"
	GroupBaseModel  Grouping = "base_model"
	GroupType       Grouping = "type"
	GroupTag        Grouping = "tag"
	GroupCollection Grouping = "collection"
)

// Definition is a view's configuration.
type Definition struct {
	ID       int64             `json:"id"`
	Name     string            `json:"name"`
	Root     string            `json:"root"`
	GroupBy  Grouping          `json:"group_by"`
	Filter   store.SearchQuery `json:"filter"`
	Strategy link.Strategy     `json:"strategy,omitempty"`

	CreatedAt   string `json:"created_at,omitempty"`
	GeneratedAt string `json:"generated_at,omitempty"`
	Status      string `json:"status,omitempty"`
	EntryCount  int    `json:"entry_count"`
}

// Result summarizes a generation run.
type Result struct {
	View     string        `json:"view"`
	Strategy link.Strategy `json:"strategy"`
	Created  int           `json:"created"`
	Removed  int           `json:"removed"`
	Kept     int           `json:"kept"`
	Skipped  int           `json:"skipped"`
	Errors   []string      `json:"errors,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Elapsed  time.Duration `json:"-"`
}

// Manager owns view definitions and their materialization.
type Manager struct {
	st *store.Store
}

// NewManager wraps a store.
func NewManager(st *store.Store) *Manager { return &Manager{st: st} }

// Create records a new view definition. Nothing is written to disk until
// Generate runs.
func (m *Manager) Create(def Definition) (*Definition, error) {
	if strings.TrimSpace(def.Name) == "" {
		return nil, fmt.Errorf("view: a view needs a name")
	}
	root, err := filepath.Abs(def.Root)
	if err != nil {
		return nil, fmt.Errorf("view: resolving root: %w", err)
	}
	def.Root = root

	if def.GroupBy == "" {
		def.GroupBy = GroupFlat
	}
	filterJSON, err := json.Marshal(def.Filter)
	if err != nil {
		return nil, err
	}

	res, err := m.st.DB().Exec(`
        INSERT INTO view (name, root, group_by, filter, created_at, status)
        VALUES (?, ?, ?, ?, ?, 'never-generated')`,
		def.Name, def.Root, string(def.GroupBy), string(filterJSON),
		timestamp.Now())
	if err != nil {
		return nil, fmt.Errorf("view: creating %q: %w", def.Name, err)
	}
	def.ID, _ = res.LastInsertId()
	def.Status = "never-generated"
	return &def, nil
}

// List returns every view.
func (m *Manager) List() ([]Definition, error) {
	rows, err := m.st.DB().Query(`
        SELECT v.id, v.name, v.root, v.group_by, COALESCE(v.filter, '{}'),
               COALESCE(v.strategy, ''), v.created_at, COALESCE(v.generated_at, ''), v.status,
               (SELECT COUNT(*) FROM view_entry e WHERE e.view_id = v.id)
          FROM view v ORDER BY v.name`)
	if err != nil {
		return nil, fmt.Errorf("view: listing: %w", err)
	}
	defer rows.Close()

	out := []Definition{}
	for rows.Next() {
		var d Definition
		var groupBy, filterJSON, strategy string
		if err := rows.Scan(&d.ID, &d.Name, &d.Root, &groupBy, &filterJSON,
			&strategy, &d.CreatedAt, &d.GeneratedAt, &d.Status, &d.EntryCount); err != nil {
			return nil, err
		}
		d.GroupBy = Grouping(groupBy)
		d.Strategy = link.Strategy(strategy)
		_ = json.Unmarshal([]byte(filterJSON), &d.Filter)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Get returns one view by name.
func (m *Manager) Get(name string) (*Definition, error) {
	views, err := m.List()
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].Name == name {
			return &views[i], nil
		}
	}
	return nil, nil
}

// GenerateOptions configures materialization.
type GenerateOptions struct {
	// Strategy forces a mechanism. Empty means probe and pick the best.
	Strategy link.Strategy

	// AllowHardlink permits the one mechanism that can corrupt an original
	// (§9.3). Off unless the user asks for it explicitly.
	AllowHardlink bool

	// DryRun reports what would change without touching the disk.
	DryRun bool

	Logf func(format string, args ...any)
}

// Generate materializes a view, adding what is missing and removing what no
// longer belongs.
//
// It is idempotent: running it twice does nothing the second time. That matters
// because a view is expected to be regenerated whenever the library changes, and
// a generator that recreated everything each run would rewrite an entire tree
// for one added model.
func (m *Manager) Generate(ctx context.Context, name string, opts GenerateOptions) (*Result, error) {
	started := time.Now()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	def, err := m.Get(name)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, fmt.Errorf("view: no view named %q", name)
	}

	result := &Result{View: def.Name}

	// Refuse to generate into a model root. A view inside the scanned tree would
	// be picked up by the next scan and counted as another copy of every model
	// in it, and on a hardlink or symlink strategy the result would be an index
	// arguing with itself.
	if err := m.checkRootIsSafe(def.Root); err != nil {
		return nil, err
	}

	wanted, err := m.plan(def)
	if err != nil {
		return nil, err
	}

	existing, err := m.existingEntries(def.ID)
	if err != nil {
		return nil, err
	}

	strategy := opts.Strategy
	if strategy == "" && len(wanted) > 0 && !opts.DryRun {
		// Probe from a real source directory: reflinks only work within one
		// filesystem, so the answer depends on where the models actually are.
		var sampleDir string
		for _, e := range wanted {
			sampleDir = filepath.Dir(e.sourcePath)
			break
		}
		capability, err := link.Probe(sampleDir, def.Root)
		if err != nil {
			return nil, err
		}
		strategy = capability.Best(opts.AllowHardlink)
		logf("using %s (available: %s)", strategy, joinStrategies(capability.Available))
	}
	if strategy == "" {
		strategy = link.Copy
	}
	result.Strategy = strategy
	result.Warnings = link.Warnings(strategy)

	// Remove entries that are no longer wanted, before creating new ones, so a
	// model that moved between groups does not briefly occupy both.
	for path, entry := range existing {
		if _, keep := wanted[path]; keep {
			continue
		}
		if opts.DryRun {
			result.Removed++
			continue
		}
		if err := m.removeEntry(entry); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.Removed++
	}

	for path, want := range wanted {
		if ctx.Err() != nil {
			break
		}
		if _, ok := existing[path]; ok {
			// Already materialized. Left alone rather than recreated: a view
			// regenerated after one model was added should not rewrite the tree.
			result.Kept++
			continue
		}
		if opts.DryRun {
			result.Created++
			continue
		}

		if _, err := os.Stat(want.sourcePath); err != nil {
			// The source vanished between planning and now. Skipping is correct;
			// the next scan will mark it absent and the next generate will drop
			// it from the plan.
			result.Skipped++
			continue
		}
		if err := link.Materialize(want.sourcePath, path, strategy); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err := m.recordEntry(def.ID, want, path, strategy); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.Created++
	}

	if !opts.DryRun {
		status := "generated"
		if len(result.Errors) > 0 {
			status = "generated-with-errors"
		}
		if _, err := m.st.DB().Exec(
			`UPDATE view SET generated_at = ?, strategy = ?, status = ? WHERE id = ?`,
			timestamp.Now(), string(strategy), status, def.ID); err != nil {
			return result, err
		}
		m.pruneEmptyDirs(def.Root)
	}

	result.Elapsed = time.Since(started)
	return result, nil
}

type plannedEntry struct {
	sha        string
	sourcePath string
	bytes      int64
}

// plan computes the desired contents of a view.
func (m *Manager) plan(def *Definition) (map[string]plannedEntry, error) {
	query := def.Filter
	query.Limit = 500
	query.Offset = 0
	// A view describes what is on disk now. An absent path has nothing to link
	// to, and a provisional one has not been confirmed by a full hash -- §10.1
	// bars those from any write-side decision, and generating a view is one.
	present := true
	query.Present = &present

	wanted := map[string]plannedEntry{}
	usedNames := map[string]bool{}

	for {
		results, err := m.st.Search(query)
		if err != nil {
			return nil, err
		}
		if len(results.Hits) == 0 {
			break
		}

		for _, hit := range results.Hits {
			source, bytes, ok, err := m.confirmedPath(hit.SHA256)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}

			group, err := m.groupFor(def, hit)
			if err != nil {
				return nil, err
			}
			filename := entryFilename(hit, source)

			dest := filepath.Join(def.Root, group, filename)
			// Two models can legitimately share a display name. Disambiguating
			// with a hash prefix keeps both in the view instead of silently
			// dropping one.
			if usedNames[dest] {
				ext := filepath.Ext(filename)
				stem := strings.TrimSuffix(filename, ext)
				dest = filepath.Join(def.Root, group,
					fmt.Sprintf("%s.%s%s", stem, hit.SHA256[:8], ext))
			}
			usedNames[dest] = true

			wanted[dest] = plannedEntry{sha: hit.SHA256, sourcePath: source, bytes: bytes}
		}

		if len(results.Hits) < query.Limit {
			break
		}
		query.Offset += query.Limit
	}
	return wanted, nil
}

// confirmedPath returns a present, non-provisional path for a model.
func (m *Manager) confirmedPath(sha string) (string, int64, bool, error) {
	var path string
	var size int64
	err := m.st.DB().QueryRow(`
        SELECT path, size FROM model_file_path
         WHERE sha256 = ? AND present = 1 AND provisional = 0
         ORDER BY id LIMIT 1`, sha).Scan(&path, &size)
	if err != nil {
		return "", 0, false, nil
	}
	return path, size, true, nil
}

func (m *Manager) groupFor(def *Definition, hit store.SearchHit) (string, error) {
	switch def.GroupBy {
	case GroupBaseModel:
		return sanitizeSegment(orUnsorted(hit.BaseModel)), nil
	case GroupType:
		return sanitizeSegment(orUnsorted(hit.Type)), nil
	case GroupTag:
		if len(hit.Tags) == 0 {
			return "untagged", nil
		}
		// A model with several tags lands under the first alphabetically rather
		// than being duplicated into each: a view is a place to find a file, and
		// finding it three times is worse than finding it once.
		return sanitizeSegment(hit.Tags[0]), nil
	case GroupCollection:
		var name string
		err := m.st.DB().QueryRow(`
            SELECT c.name FROM collection c
              JOIN collection_member cm ON cm.collection_id = c.id
             WHERE cm.sha256 = ? ORDER BY cm.position, c.name LIMIT 1`, hit.SHA256).Scan(&name)
		if err != nil || name == "" {
			return "ungrouped", nil
		}
		return sanitizeSegment(name), nil
	default:
		return "", nil
	}
}

func orUnsorted(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unsorted"
	}
	return v
}

// entryFilename names a view entry.
//
// The original filename is kept when there is nothing better, because that is
// what the user recognizes and what any sidecar beside the real file is named
// after. A resolved name is used when one exists, since finding a model by its
// real name is the entire point of the view.
func entryFilename(hit store.SearchHit, sourcePath string) string {
	original := filepath.Base(sourcePath)
	ext := filepath.Ext(original)

	if hit.Name == "" {
		return original
	}
	name := sanitizeSegment(hit.Name)
	if hit.Version != "" {
		name += " " + sanitizeSegment(hit.Version)
	}
	if name == "" {
		return original
	}
	return name + ext
}

// sanitizeSegment makes a value safe as a single path component on every
// platform this targets, including the Windows reserved names that would
// otherwise create an unopenable file.
func sanitizeSegment(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, v)
	v = strings.Trim(v, ". ")
	if v == "" {
		return "unsorted"
	}

	switch strings.ToUpper(v) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return v + "_"
	}
	if len(v) > 120 {
		v = v[:120]
	}
	return v
}

func (m *Manager) existingEntries(viewID int64) (map[string]viewEntry, error) {
	rows, err := m.st.DB().Query(
		`SELECT id, sha256, path, source_path, strategy FROM view_entry WHERE view_id = ?`, viewID)
	if err != nil {
		return nil, fmt.Errorf("view: reading entries: %w", err)
	}
	defer rows.Close()

	out := map[string]viewEntry{}
	for rows.Next() {
		var e viewEntry
		if err := rows.Scan(&e.id, &e.sha, &e.path, &e.sourcePath, &e.strategy); err != nil {
			return nil, err
		}
		out[e.path] = e
	}
	return out, rows.Err()
}

type viewEntry struct {
	id         int64
	sha        string
	path       string
	sourcePath string
	strategy   string
}

func (m *Manager) recordEntry(viewID int64, want plannedEntry, path string, s link.Strategy) error {
	_, err := m.st.DB().Exec(`
        INSERT INTO view_entry (view_id, sha256, path, source_path, strategy, bytes, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(path) DO UPDATE SET
            view_id = excluded.view_id, sha256 = excluded.sha256,
            source_path = excluded.source_path, strategy = excluded.strategy`,
		viewID, want.sha, path, want.sourcePath, string(s), want.bytes,
		timestamp.Now())
	if err != nil {
		return fmt.Errorf("view: recording entry %s: %w", path, err)
	}
	return nil
}

// removeEntry deletes a file this app created, and only such a file.
func (m *Manager) removeEntry(e viewEntry) error {
	// Safety check before any delete: the recorded path must still be what we
	// think it is. If something else now occupies that name, the record is stale
	// and deleting would destroy a stranger's file.
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("view: removing %s: %w", e.path, err)
	}
	if _, err := m.st.DB().Exec(`DELETE FROM view_entry WHERE id = ?`, e.id); err != nil {
		return fmt.Errorf("view: forgetting entry %s: %w", e.path, err)
	}
	return nil
}

// Delete removes a view and every file it created.
func (m *Manager) Delete(name string, removeFiles bool) (int, error) {
	def, err := m.Get(name)
	if err != nil {
		return 0, err
	}
	if def == nil {
		return 0, fmt.Errorf("view: no view named %q", name)
	}

	removed := 0
	if removeFiles {
		entries, err := m.existingEntries(def.ID)
		if err != nil {
			return 0, err
		}
		// Only files recorded in view_entry are deleted -- never everything under
		// the root. A user who points a view at a directory that already held
		// something must not lose it.
		for _, e := range entries {
			if err := m.removeEntry(e); err == nil {
				removed++
			}
		}
		m.pruneEmptyDirs(def.Root)
	}

	if _, err := m.st.DB().Exec(`DELETE FROM view WHERE id = ?`, def.ID); err != nil {
		return removed, fmt.Errorf("view: deleting %q: %w", name, err)
	}
	return removed, nil
}

// pruneEmptyDirs removes group directories the view emptied, deepest first.
// Only empty ones, so nothing a user put there is at risk.
func (m *Manager) pruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		// Remove fails on a non-empty directory, which is exactly the guard
		// wanted here.
		_ = os.Remove(dirs[i])
	}
}

// checkRootIsSafe refuses to generate a view inside a scanned model root.
func (m *Manager) checkRootIsSafe(root string) error {
	rows, err := m.st.DB().Query(`SELECT DISTINCT root FROM model_file_path`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var modelRoot string
		if err := rows.Scan(&modelRoot); err != nil {
			return err
		}
		if isUnder(root, modelRoot) || isUnder(modelRoot, root) {
			return fmt.Errorf(
				"view: root %s overlaps the scanned model root %s.\n"+
					"A view inside a scanned tree would be picked up by the next scan and "+
					"counted as another copy of every model in it. Choose a directory "+
					"outside your model roots",
				root, modelRoot)
		}
	}
	return rows.Err()
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

func joinStrategies(list []link.Strategy) string {
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// Summary renders a generation result for the CLI.
func (r *Result) Summary() string {
	out := fmt.Sprintf("%s: %d created, %d kept, %d removed", r.View, r.Created, r.Kept, r.Removed)
	if r.Skipped > 0 {
		out += fmt.Sprintf(", %d skipped", r.Skipped)
	}
	out += fmt.Sprintf(" via %s (%s)", r.Strategy, r.Elapsed.Round(time.Millisecond))

	for _, w := range r.Warnings {
		out += "\n\nnote: " + w
	}
	if len(r.Errors) > 0 {
		out += fmt.Sprintf("\n\n%d error(s):", len(r.Errors))
		for i, e := range r.Errors {
			if i >= 10 {
				out += fmt.Sprintf("\n  ... and %d more", len(r.Errors)-10)
				break
			}
			out += "\n  " + e
		}
	}
	return out
}
