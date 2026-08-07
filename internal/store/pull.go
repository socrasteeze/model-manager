package store

// Copies fetched from an upstream model-manager.
//
// Every other table here describes the library. This one describes this
// machine's relationship to another machine's library, which is why it is the
// only table whose rows are meaningless if the database is copied elsewhere.
//
// It exists to answer one question honestly: can this file be deleted and got
// back? Nothing else in the schema can answer it. A model_origin row says the
// bytes were once published somewhere, which is not the same as "this daemon
// can fetch them again"; a path row says a file is here, not where it came
// from. Eviction is the first operation in this program that removes data, so
// the precondition for it is a row written by the code that did the fetching.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PulledCopy is one file this daemon fetched from an upstream.
type PulledCopy struct {
	SHA256    string `json:"sha256"`
	Upstream  string `json:"upstream"`
	Path      string `json:"path"`
	Root      string `json:"root"`
	SizeBytes int64  `json:"size_bytes"`
	PulledAt  string `json:"pulled_at"`

	// EvictedAt is empty while the copy is resident.
	EvictedAt string `json:"evicted_at,omitempty"`
}

// Resident reports whether the copy is still on this disk.
func (p PulledCopy) Resident() bool { return p.EvictedAt == "" }

// ErrNoPulledCopy means nothing recorded fetching this model from an upstream.
var ErrNoPulledCopy = errors.New("store: no upstream copy recorded")

// PutPulledCopy records a fetch, clearing any earlier eviction.
//
// Upserting rather than inserting because re-pulling an evicted model is the
// normal way to get it back, and that has to make the copy resident again.
//
// The path is part of the key, so a re-pull landing somewhere else is a second
// row rather than a replacement -- which is right, because both files then
// exist and each has to be evictable on its own. Only a re-pull to the *same*
// path is the same copy, and that is the one this refreshes.
func (s *Store) PutPulledCopy(p PulledCopy) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	_, err := s.db.Exec(`
        INSERT INTO pulled_file (sha256, upstream, path, root, size_bytes, pulled_at, evicted_at)
        VALUES (?, ?, ?, ?, ?, ?, NULL)
        ON CONFLICT(sha256, upstream, path) DO UPDATE SET
            root       = excluded.root,
            size_bytes = excluded.size_bytes,
            pulled_at  = excluded.pulled_at,
            evicted_at = NULL`,
		strings.ToLower(p.SHA256), p.Upstream, p.Path, p.Root, p.SizeBytes, nowUTC())
	if err != nil {
		return fmt.Errorf("store: recording pulled copy of %s: %w", p.SHA256, err)
	}
	return nil
}

// MarkPulledEvicted records that one copy was removed from this disk.
//
// The path is required, not optional. Without it this marks every copy of the
// model evicted, including ones still sitting on disk -- and the library would
// then offer to re-pull a file it already has while claiming not to have it.
func (s *Store) MarkPulledEvicted(sha, upstream, path string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	res, err := s.db.Exec(
		`UPDATE pulled_file SET evicted_at = ?
          WHERE sha256 = ? AND upstream = ? AND path = ?`,
		nowUTC(), strings.ToLower(sha), upstream, path)
	if err != nil {
		return fmt.Errorf("store: marking %s evicted: %w", sha, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPulledCopy
	}
	return nil
}

// PulledCopies lists every recorded fetch of one model, resident first.
func (s *Store) PulledCopies(sha string) ([]PulledCopy, error) {
	rows, err := s.db.Query(`
        SELECT sha256, upstream, path, root, size_bytes, pulled_at, evicted_at
          FROM pulled_file WHERE sha256 = ?
         ORDER BY (evicted_at IS NULL) DESC, upstream, path`, strings.ToLower(sha))
	if err != nil {
		return nil, fmt.Errorf("store: listing pulled copies of %s: %w", sha, err)
	}
	defer rows.Close()
	return scanPulledCopies(rows)
}

// ResidentPullsFor lists the still-present copies of a model, newest last.
//
// Returns every candidate rather than picking one, because choosing between two
// files that both exist is a decision about which one gets deleted, and the
// caller has a path to match against while this does not.
func (s *Store) ResidentPullsFor(sha, upstream string) ([]PulledCopy, error) {
	all, err := s.PulledCopies(sha)
	if err != nil {
		return nil, err
	}
	out := []PulledCopy{}
	for _, p := range all {
		if p.Resident() && (upstream == "" || p.Upstream == upstream) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ResidentPull returns the one still-present copy of a model.
//
// Reports ambiguity rather than choosing, because the choice decides which file
// gets deleted. Callers that hold a path should use ResidentPullsFor and match
// it; this is for the ones that only have a hash -- the CLI's confirmation
// prompt, which has to name a real file before asking.
func (s *Store) ResidentPull(sha, upstream string) (PulledCopy, error) {
	resident, err := s.ResidentPullsFor(sha, upstream)
	if err != nil {
		return PulledCopy{}, err
	}
	switch len(resident) {
	case 0:
		return PulledCopy{}, ErrNoPulledCopy
	case 1:
		return resident[0], nil
	}

	// Two shapes of ambiguity with two different fixes, and one message for both
	// was a false statement about the data pointing at a flag that could not
	// resolve it: several copies from one upstream are not several upstreams,
	// and naming that upstream narrows nothing.
	upstreams := map[string]bool{}
	paths := make([]string, 0, len(resident))
	for _, p := range resident {
		upstreams[p.Upstream] = true
		paths = append(paths, p.Path)
	}
	if len(upstreams) > 1 {
		return PulledCopy{}, fmt.Errorf(
			"store: %s was pulled from %d upstreams; name one", sha, len(upstreams))
	}
	return PulledCopy{}, fmt.Errorf(
		"store: %s has %d pulled copies from %s; name the path to remove: %s",
		sha, len(resident), resident[0].Upstream, strings.Join(paths, ", "))
}

// ResidentPulls lists every copy still on this disk, largest first.
//
// Largest first because the caller is answering "what can I delete to get space
// back", and the answer to that starts at the top.
func (s *Store) ResidentPulls() ([]PulledCopy, error) {
	rows, err := s.db.Query(`
        SELECT sha256, upstream, path, root, size_bytes, pulled_at, evicted_at
          FROM pulled_file WHERE evicted_at IS NULL
         ORDER BY size_bytes DESC, sha256`)
	if err != nil {
		return nil, fmt.Errorf("store: listing resident pulls: %w", err)
	}
	defer rows.Close()
	return scanPulledCopies(rows)
}

// ViewReference is one generated tree that links to a model.
type ViewReference struct {
	ViewName string `json:"view"`
	Path     string `json:"path"`
	Strategy string `json:"strategy"`
}

// ViewsReferencing lists the generated trees that link to a model's bytes.
//
// The reason this has to be asked before deleting anything is that a view's
// links are not all alike. A symlinked view left pointing at a removed file
// dangles, and the tool that follows it fails somewhere confusing. A hardlinked
// view is worse and quieter: removing the model drops one link, the inode
// survives because the view still holds a reference, and the delete frees
// nothing at all while reporting the file's full size as reclaimed.
func (s *Store) ViewsReferencing(sha string) ([]ViewReference, error) {
	rows, err := s.db.Query(`
        SELECT COALESCE(v.name, ''), e.path, e.strategy
          FROM view_entry e LEFT JOIN view v ON v.id = e.view_id
         WHERE e.sha256 = ?
         ORDER BY e.id`, strings.ToLower(sha))
	if err != nil {
		return nil, fmt.Errorf("store: listing views referencing %s: %w", sha, err)
	}
	defer rows.Close()

	out := []ViewReference{}
	for rows.Next() {
		var r ViewReference
		if err := rows.Scan(&r.ViewName, &r.Path, &r.Strategy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanPulledCopies(rows *sql.Rows) ([]PulledCopy, error) {
	out := []PulledCopy{}
	for rows.Next() {
		var p PulledCopy
		var evicted sql.NullString
		if err := rows.Scan(&p.SHA256, &p.Upstream, &p.Path, &p.Root,
			&p.SizeBytes, &p.PulledAt, &evicted); err != nil {
			return nil, err
		}
		if evicted.Valid {
			p.EvictedAt = evicted.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
