package scan

// Re-observing a file's cache key without hashing it.
//
// The walk builds this tuple for every file it visits and the store records it
// on every path row, but until now nothing outside this package could ask for
// it. Eviction needs to: deleting a file on the strength of a database row
// means first checking the row still describes the file that is actually there.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Identity is the (device, inode, size, mtime) tuple the incremental scan cache
// is keyed on.
//
// Using the same four values here rather than inventing a freshness check is
// the point. If this tuple is unchanged the scanner would skip re-hashing the
// file, which is precisely the claim "these are still the bytes we indexed" --
// so anything acting on that claim should be trusting exactly what the scanner
// trusts, no more and no less.
type Identity struct {
	Device  uint64
	Inode   uint64
	Size    int64
	MtimeNs int64
}

// StatIdentity reads a file's cache key without opening or hashing it.
func StatIdentity(path string) (Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	if info.IsDir() {
		return Identity{}, fmt.Errorf("scan: %s is a directory", path)
	}
	device, inode, err := fileID(path, info)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Device:  device,
		Inode:   inode,
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
	}, nil
}

// ErrOutsideRoots means a recorded path resolved outside every given root.
var ErrOutsideRoots = errors.New("scan: path is outside every enabled model root")

// ResolveWithinRoots turns a recorded path into a real one under a known root.
//
// Not traversal defence -- the path comes from the database, not from a
// request. It guards against the database being stale in a way the write side
// already assumes it cannot be: RemoveRoot deliberately leaves path rows behind
// (only the claim "these are present" goes away), so a row can outlive the root
// that justified it, and a root re-added under a different spelling strands the
// old rows. Anything that reads or removes a file on the strength of such a row
// should first confirm a root still vouches for it.
//
// Both sides are resolved through symlinks before comparing, so a link into a
// root counts and a link out of one does not.
func ResolveWithinRoots(roots []string, recorded string) (string, error) {
	abs, err := filepath.Abs(recorded)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// A missing file is the ordinary "gone since the last scan", and the
		// caller decides what that means. Any other failure means a component
		// could not be inspected, and continuing would walk past exactly the
		// symlink this call exists to follow.
		if errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("%w: %v", ErrOutsideRoots, err)
	}

	for _, root := range roots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			// A root that cannot be resolved cannot vouch for anything below it.
			continue
		}
		if WithinRoot(rootResolved, resolved) {
			return resolved, nil
		}
	}
	return "", ErrOutsideRoots
}

// WithinRoot reports whether candidate sits at or below root.
func WithinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// Matches reports whether a recorded path row still describes this file.
//
// Device and inode are compared only when the row has them. A row written
// before the identity was obtainable -- or on a filesystem that reports zero --
// carries 0/0, and treating that as a mismatch would make an otherwise
// unchanged file look modified.
func (id Identity) Matches(device, inode uint64, size, mtimeNs int64) bool {
	if size != id.Size || mtimeNs != id.MtimeNs {
		return false
	}
	if device != 0 || inode != 0 {
		return device == id.Device && inode == id.Inode
	}
	return true
}
