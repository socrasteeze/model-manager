// Package evict removes a local copy of a model that can be fetched again.
//
// This is the only code in the program that deletes a model file, and it is a
// package rather than a handler for exactly that reason: the HTTP endpoint and
// the CLI both need it, and a destructive operation with this many
// preconditions must not have two implementations that can drift.
//
// The standing guarantee elsewhere is that this tool never modifies, moves,
// renames or deletes an existing model file -- RemoveRoot goes out of its way
// to honour it, marking paths absent and leaving the disk alone. Eviction
// narrows that promise rather than abandoning it: it deletes a file this daemon
// wrote, at a destination the user chose, which a recorded upstream can hand
// back. Take away any one of those three and it refuses.
//
// What survives is the point. The model keeps its name, tags, previews,
// provenance and the user's own edits; only the claim "these bytes are on this
// disk" goes away. That falls out of the schema for free, because every one of
// those tables is keyed on the content hash and none on a path -- which is also
// why this must never touch the model_file row, whose deletion would cascade
// all of them away.
package evict

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Request names a copy to remove.
type Request struct {
	SHA256 string

	// Path selects among several copies. Required when the model has more than
	// one present, confirmed path: a bare hash would be asking which file to
	// delete, and this does not guess.
	Path string

	// Upstream disambiguates a model pulled from more than one upstream.
	Upstream string

	// HeldBy names an unfinished transfer for these bytes, or "" if none.
	//
	// A hook rather than a dependency on the download manager, because the CLI
	// links this package and has no daemon behind it: nil is the honest answer
	// there, not a Manager constructed so a check can pass. It also keeps the
	// ordered guard list in one place instead of half here and half in an HTTP
	// handler -- and the order matters, because a download in flight for a hash
	// usually means that hash is not in model_file yet, so asking first would
	// answer "a download holds this" for a model that does not exist.
	HeldBy func(sha256 string) string
}

// Result describes what was removed.
type Result struct {
	Path       string
	FreedBytes int64
	Upstream   string

	// AlreadyGone means the file was not there to remove. The bookkeeping still
	// ran: the user asked for this file not to be present, and it is not.
	AlreadyGone bool
}

// Refusals. Each is a different problem with a different fix, so callers can
// map them to a status code or an exit code without parsing a message.
var (
	// ErrUnknownModel means no model has that hash.
	ErrUnknownModel = errors.New("evict: no model with that hash")

	// ErrNotPulled means nothing recorded fetching this copy from an upstream,
	// so it is not provably re-fetchable. The load-bearing refusal.
	ErrNotPulled = errors.New(
		"evict: this copy was not pulled from an upstream, so it will not be deleted")

	// ErrAmbiguous means several copies are present and none was named.
	ErrAmbiguous = errors.New("evict: more than one copy is present; name the one to remove")

	// ErrNotThePulledCopy means the named path is a different copy of the same
	// bytes -- a tier-staged one, or an original -- which this cannot delete on
	// the strength of a row describing another file.
	ErrNotThePulledCopy = errors.New("evict: that path is not the copy this daemon pulled")

	// ErrChanged means the file no longer matches what was indexed.
	ErrChanged = errors.New("evict: the file has changed since it was indexed")

	// ErrViewLinked means a generated view still links to these bytes.
	ErrViewLinked = errors.New("evict: a generated view still links to this model")

	// ErrDownloadInFlight means a transfer is still writing these bytes.
	ErrDownloadInFlight = errors.New("evict: a download is still transferring these bytes")
)

// Do removes one pulled copy, after establishing that it is safe to.
//
// The order of the checks is deliberate: identity before selection, selection
// before containment, containment before freshness, freshness before the views
// check, and only then the single irreversible line.
func Do(st *store.Store, req Request) (Result, error) {
	sha := strings.ToLower(strings.TrimSpace(req.SHA256))

	if ok, err := st.ModelExists(sha); err != nil {
		return Result{}, err
	} else if !ok {
		return Result{}, ErrUnknownModel
	}

	// The precondition. Without a row written by the code that performed the
	// fetch, nothing knows this file can be got back -- and a file that cannot
	// be got back is not a cache entry.
	//
	// Every candidate rather than one, because a model pulled into two roots has
	// two copies and both are real. Asking the store to choose would have it
	// refuse as ambiguous even when the request named a path, since the path is
	// known here and not there.
	candidates, err := st.ResidentPullsFor(sha, strings.TrimSpace(req.Upstream))
	if err != nil {
		return Result{}, err
	}
	if len(candidates) == 0 {
		return Result{}, ErrNotPulled
	}

	target, err := selectPath(st, sha, req.Path)
	if err != nil {
		return Result{}, err
	}
	// The path row and the pull record must describe the same file. Without this
	// an evict could remove a tier-staged copy, or an original that happens to
	// share the hash, while reporting that it removed the pulled one.
	pull, ok := pickPull(candidates, target.Path)
	if !ok {
		paths := make([]string, 0, len(candidates))
		for _, c := range candidates {
			paths = append(paths, c.Path)
		}
		return Result{}, fmt.Errorf("%w: the pulled copies are at %s",
			ErrNotThePulledCopy, strings.Join(paths, ", "))
	}

	// Nothing may still be writing these bytes. Asked after identity and
	// selection because it is a question about the model, not about this file on
	// disk -- and asking it first would answer "a download holds this" for a hash
	// that is not in the library yet, which is the ordinary state of a transfer
	// in progress and where 404 is the correct answer.
	//
	// Advisory rather than a lock, and there need be no lock: publish never
	// overwrites, and the identity check below catches a file that moved
	// underneath us. This exists so the daemon does not fight itself.
	if req.HeldBy != nil {
		if id := req.HeldBy(sha); id != "" {
			return Result{}, fmt.Errorf("%w: download %s", ErrDownloadInFlight, id)
		}
	}

	roots, err := st.EnabledRootPaths()
	if err != nil {
		return Result{}, err
	}
	resolved, err := scan.ResolveWithinRoots(roots, target.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Already gone. Run the bookkeeping anyway, so the index stops
			// claiming a file that is not there.
			if err := finish(st, sha, pull, target); err != nil {
				return Result{}, err
			}
			return Result{Path: target.Path, Upstream: pull.Upstream, AlreadyGone: true}, nil
		}
		return Result{}, err
	}

	// The bytes must still be the bytes the index describes, checked with the
	// same four-tuple the incremental scan is keyed on -- so this trusts exactly
	// what the scanner trusts rather than inventing a freshness rule of its own.
	id, err := scan.StatIdentity(resolved)
	if err != nil {
		return Result{}, err
	}
	if !id.Matches(target.Device, target.Inode, target.Size, target.MtimeNs) {
		return Result{}, fmt.Errorf(
			"%w: rescan this root first, so the copy being deleted is the one that was recorded",
			ErrChanged)
	}

	// No generated view may be holding these bytes. With a hardlink strategy the
	// removal would drop one link, leave the inode alive, and free nothing at
	// all while reporting the file's full size as reclaimed. There is no correct
	// way to do that quietly.
	views, err := st.ViewsReferencing(sha)
	if err != nil {
		return Result{}, err
	}
	if len(views) > 0 {
		names := make([]string, 0, len(views))
		for _, v := range views {
			names = append(names, fmt.Sprintf("%s (%s)", v.ViewName, v.Strategy))
		}
		return Result{}, fmt.Errorf(
			"%w: regenerate or remove these first, or the space would not actually be freed: %s",
			ErrViewLinked, strings.Join(names, ", "))
	}

	freed := id.Size
	if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("evict: removing %s: %w", resolved, err)
	}
	if err := finish(st, sha, pull, target); err != nil {
		return Result{}, err
	}
	return Result{Path: resolved, FreedBytes: freed, Upstream: pull.Upstream}, nil
}

// finish records the removal.
//
// The path row is kept and marked absent, not deleted -- the same thing
// RemoveRoot does, for the same reason: only the claim "this is present"
// stopped being true. Nothing keyed on the content hash is touched.
func finish(st *store.Store, sha string, pull store.PulledCopy, target store.FilePath) error {
	if err := st.SetPathAbsent(target.ID); err != nil {
		return fmt.Errorf("evict: the file was removed but the index still lists it: %w", err)
	}
	if err := st.MarkPulledEvicted(sha, pull.Upstream, pull.Path); err != nil &&
		!errors.Is(err, store.ErrNoPulledCopy) {
		return fmt.Errorf("evict: the file was removed but the record was not updated: %w", err)
	}
	return nil
}

// pickPull finds the recorded fetch that describes a given file.
//
// Compared with PathsEqual rather than by string, because one side was written
// by the downloader and the other by the scanner: on Windows they can differ in
// separator and in case while naming the same file.
func pickPull(candidates []store.PulledCopy, path string) (store.PulledCopy, bool) {
	for _, c := range candidates {
		if PathsEqual(c.Path, path) {
			return c, true
		}
	}
	return store.PulledCopy{}, false
}

// selectPath picks the path row to remove.
func selectPath(st *store.Store, sha, wanted string) (store.FilePath, error) {
	rows, err := st.PathsFor(sha)
	if err != nil {
		return store.FilePath{}, err
	}
	var candidates []store.FilePath
	for _, p := range rows {
		if p.Present && !p.Provisional {
			candidates = append(candidates, p)
		}
	}

	if wanted = strings.TrimSpace(wanted); wanted != "" {
		for _, p := range candidates {
			if PathsEqual(p.Path, wanted) {
				return p, nil
			}
		}
		return store.FilePath{}, fmt.Errorf("evict: no present copy of this model at %s", wanted)
	}

	switch len(candidates) {
	case 0:
		return store.FilePath{}, errors.New("evict: no present copy of this model is recorded")
	case 1:
		return candidates[0], nil
	default:
		// A tier-staged copy is a real second present path, so this is a state
		// that happens rather than a formality.
		paths := make([]string, 0, len(candidates))
		for _, p := range candidates {
			paths = append(paths, p.Path)
		}
		return store.FilePath{}, fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(paths, ", "))
	}
}

// PathsEqual reports whether two spellings name the same file.
//
// Absolute and cleaned, then compared the way the platform's filesystems
// compare names. It matters because one of the two strings was written by the
// downloader and the other by the scanner, and on Windows they can differ in
// both separator and case. Insensitive only where the filesystem is: being
// permissive on Linux would let two genuinely different files compare equal,
// which for a call that deletes one of them is not a trade worth making.
func PathsEqual(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		aa, bb = a, b
	}
	aa, bb = filepath.Clean(aa), filepath.Clean(bb)
	if caseInsensitivePaths {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}
