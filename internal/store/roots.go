package store

// Managed model roots.
//
// A "root" is a directory the library indexes. Before Phase 7 the set of roots
// was whatever `SELECT DISTINCT root FROM model_file_path` happened to return,
// which works only for roots that already contain indexed files. Adding a
// directory from the UI has to work before anything under it has been scanned,
// so roots are now first-class rows.
//
// The invariant this file exists to protect: the string stored in
// model_root.path is the *same* string that ends up in model_file_path.root.
// SweepAbsentPaths matches that column exactly (records.go), so a second
// spelling of one directory -- a trailing slash, an unresolved symlink, a
// relative path -- forks the root, and every file under the fork stays
// present=1 forever no matter what the disk says. Canonicalization here is
// therefore correctness, not neatness.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is a managed model directory.
type Root struct {
	ID            int64  `json:"id"`
	Path          string `json:"path"`
	Label         string `json:"label,omitempty"`
	Tool          string `json:"tool,omitempty"`
	Enabled       bool   `json:"enabled"`
	AddedAt       string `json:"added_at"`
	LastScannedAt string `json:"last_scanned_at,omitempty"`

	// Files is the number of present indexed paths under this root. Reported
	// for display; not part of the root's identity.
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// ErrRootNested is returned when a candidate root overlaps one already
// managed. Overlap is refused rather than merged because the present-sweep is
// scoped per root: a file reachable under two roots gets swept by whichever
// root did not claim it, and `present` flaps between scans.
var ErrRootNested = errors.New("store: root overlaps an existing root")

// ErrRootExists is returned when the root is already managed.
var ErrRootExists = errors.New("store: root already managed")

// CanonicalRoot reduces a user-supplied directory to the one spelling this
// database will use for it: absolute, cleaned, and with symlinks resolved.
//
// Symlink resolution is the part that is easy to skip and expensive to skip.
// The user's stated direction of travel is a single master library symlinked
// across machines; if one machine records the link and another records the
// target, the two databases disagree about a directory that is in fact the
// same, and neither sweep is correct.
func CanonicalRoot(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("store: empty root path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("store: resolving %s: %w", path, err)
	}
	abs = filepath.Clean(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("store: root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("store: root %s is not a directory", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	return abs, nil
}

// PathIsUnder reports whether child is at or below parent, comparing whole
// path segments so /models2 is not treated as living under /models.
func PathIsUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// AddRoot registers a directory, canonicalizing it first and refusing any
// overlap with a root already managed.
func (s *Store) AddRoot(path, label, tool string) (*Root, error) {
	canonical, err := CanonicalRoot(path)
	if err != nil {
		return nil, err
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	existing, err := s.listRoots()
	if err != nil {
		return nil, err
	}
	for _, r := range existing {
		if r.Path == canonical {
			return nil, fmt.Errorf("%w: %s", ErrRootExists, canonical)
		}
		if PathIsUnder(canonical, r.Path) {
			return nil, fmt.Errorf("%w: %s is inside %s", ErrRootNested, canonical, r.Path)
		}
		if PathIsUnder(r.Path, canonical) {
			return nil, fmt.Errorf("%w: %s contains the managed root %s",
				ErrRootNested, canonical, r.Path)
		}
	}

	now := nowUTC()
	res, err := s.db.Exec(
		`INSERT INTO model_root (path, label, tool, enabled, added_at)
		 VALUES (?, ?, ?, 1, ?)`,
		canonical, strings.TrimSpace(label), strings.TrimSpace(tool), now)
	if err != nil {
		return nil, fmt.Errorf("store: adding root %s: %w", canonical, err)
	}
	id, _ := res.LastInsertId()
	return &Root{
		ID:      id,
		Path:    canonical,
		Label:   strings.TrimSpace(label),
		Tool:    strings.TrimSpace(tool),
		Enabled: true,
		AddedAt: now,
	}, nil
}

// ListRoots returns every managed root with its present file counts.
func (s *Store) ListRoots() ([]Root, error) {
	roots, err := s.listRoots()
	if err != nil {
		return nil, err
	}
	counts, sizes, err := s.rootCounts()
	if err != nil {
		return nil, err
	}
	for i := range roots {
		roots[i].Files = counts[roots[i].Path]
		roots[i].Bytes = sizes[roots[i].Path]
	}
	return roots, nil
}

func (s *Store) listRoots() ([]Root, error) {
	rows, err := s.db.Query(
		`SELECT id, path, label, tool, enabled, added_at, COALESCE(last_scanned_at, '')
		   FROM model_root ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("store: listing roots: %w", err)
	}
	defer rows.Close()

	out := []Root{}
	for rows.Next() {
		var r Root
		var enabled int64
		if err := rows.Scan(&r.ID, &r.Path, &r.Label, &r.Tool, &enabled,
			&r.AddedAt, &r.LastScannedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) rootCounts() (map[string]int64, map[string]int64, error) {
	rows, err := s.db.Query(`
        SELECT p.root, COUNT(*), COALESCE(SUM(p.size), 0)
          FROM model_file_path p
         WHERE p.present = 1 AND p.root <> ''
         GROUP BY p.root`)
	if err != nil {
		return nil, nil, fmt.Errorf("store: counting roots: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	sizes := map[string]int64{}
	for rows.Next() {
		var root string
		var n, b int64
		if err := rows.Scan(&root, &n, &b); err != nil {
			return nil, nil, err
		}
		counts[root] = n
		sizes[root] = b
	}
	return counts, sizes, rows.Err()
}

// RootByID looks up one managed root.
func (s *Store) RootByID(id int64) (*Root, error) {
	roots, err := s.ListRoots()
	if err != nil {
		return nil, err
	}
	for i := range roots {
		if roots[i].ID == id {
			return &roots[i], nil
		}
	}
	return nil, fmt.Errorf("store: no root with id %d", id)
}

// RootByPath looks up a managed root by its canonical path.
func (s *Store) RootByPath(path string) (*Root, error) {
	roots, err := s.ListRoots()
	if err != nil {
		return nil, err
	}
	for i := range roots {
		if roots[i].Path == path {
			return &roots[i], nil
		}
	}
	return nil, fmt.Errorf("store: %s is not a managed root", path)
}

// SetRootEnabled enables or disables a root without forgetting it.
func (s *Store) SetRootEnabled(id int64, enabled bool) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE model_root SET enabled = ? WHERE id = ?`,
		boolInt(enabled), id)
	return err
}

// UpdateRootMeta changes a root's label and/or tool. A nil field is left as it
// was; the path is never editable, because changing it would be a different
// root wearing this one's history.
func (s *Store) UpdateRootMeta(id int64, label, tool *string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	if label != nil {
		if _, err := s.db.Exec(`UPDATE model_root SET label = ? WHERE id = ?`,
			strings.TrimSpace(*label), id); err != nil {
			return err
		}
	}
	if tool != nil {
		if _, err := s.db.Exec(`UPDATE model_root SET tool = ? WHERE id = ?`,
			strings.TrimSpace(*tool), id); err != nil {
			return err
		}
	}
	return nil
}

// MarkRootScanned records when a root last completed a scan.
func (s *Store) MarkRootScanned(path string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE model_root SET last_scanned_at = ? WHERE path = ?`,
		nowUTC(), path)
	return err
}

// RemoveRoot forgets a root and marks every path under it absent.
//
// Nothing on disk is touched -- that is the standing guarantee, and it is also
// the useful behaviour: the model_file rows and all their metadata survive, so
// re-adding the folder later restores the library rather than re-deriving it.
// Only the claim "these paths are present" goes away, which is exactly what
// stops being true once the app is no longer watching the directory.
func (s *Store) RemoveRoot(id int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var path string
	if err := s.db.QueryRow(`SELECT path FROM model_root WHERE id = ?`, id).
		Scan(&path); err != nil {
		return fmt.Errorf("store: no root with id %d: %w", id, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE model_file_path SET present = 0 WHERE root = ?`, path); err != nil {
		return fmt.Errorf("store: marking paths absent under %s: %w", path, err)
	}
	if _, err := tx.Exec(`DELETE FROM model_root WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: removing root %s: %w", path, err)
	}
	return tx.Commit()
}

// EnabledRootPaths returns the canonical paths of every enabled root, which is
// what a scan should walk.
func (s *Store) EnabledRootPaths() ([]string, error) {
	roots, err := s.listRoots()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range roots {
		if r.Enabled {
			out = append(out, r.Path)
		}
	}
	return out, nil
}
