package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/socrasteeze/model-manager/internal/modelformat"
)

// candidate is one file the walk found worth considering.
type candidate struct {
	path    string
	root    string
	device  uint64
	inode   uint64
	size    int64
	mtimeNs int64
}

// skipDirs are directories that never contain model files and are expensive or
// pointless to descend into.
var skipDirs = map[string]bool{
	".git":                      true,
	".svn":                      true,
	"node_modules":              true,
	".cache":                    true,
	"@eaDir":                    true, // Synology NAS index directory
	".Trash":                    true,
	"$RECYCLE.BIN":              true,
	"System Volume Information": true,
}

// walkRoot collects every model-looking file under root.
//
// Symlinks are not followed. Once the presentation layer starts generating views
// (spec §9), a symlink farm pointing back into the library is an expected part of
// the tree, and following it would report every view entry as a second copy of a
// model that was already counted. A reflink, by contrast, is an ordinary file and
// is scanned normally -- correctly, since it genuinely is a second path on the
// same content.
func walkRoot(ctx context.Context, root string, onError func(path, kind string, err error)) ([]candidate, error) {
	var out []candidate

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// An unreadable subdirectory is recorded and stepped over. Aborting
			// a multi-hour walk of 7.5TB because one directory denied permission
			// would be the wrong trade.
			onError(path, "stat", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}

		// Type() reflects lstat, so a symlink is identified without following it.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !modelformat.IsModelExt(path) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			onError(path, "stat", err)
			return nil
		}
		device, inode, err := fileID(path, info)
		if err != nil {
			onError(path, "stat", err)
			return nil
		}

		out = append(out, candidate{
			path:    path,
			root:    root,
			device:  device,
			inode:   inode,
			size:    info.Size(),
			mtimeNs: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return out, fmt.Errorf("scan: walking %s: %w", root, err)
	}
	return out, err
}

// prepareRoots cleans, absolutizes, deduplicates and validates the root list.
//
// Nested roots are rejected rather than silently merged. The present-sweep is
// scoped per root (spec §6.2), so a path reachable under two roots would be
// swept by whichever root did not claim it, flapping present between scans. A
// clear error up front beats an index that quietly disagrees with the disk.
func prepareRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, errors.New("scan: no roots given")
	}

	seen := make(map[string]bool, len(roots))
	var cleaned []string
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("scan: resolving root %s: %w", r, err)
		}
		abs = filepath.Clean(abs)

		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("scan: root %s: %w", abs, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("scan: root %s is not a directory", abs)
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		cleaned = append(cleaned, abs)
	}

	sort.Strings(cleaned)
	for i := 0; i < len(cleaned); i++ {
		for j := i + 1; j < len(cleaned); j++ {
			if isUnder(cleaned[j], cleaned[i]) {
				return nil, fmt.Errorf(
					"scan: root %s is nested inside root %s; overlapping roots make "+
						"the per-root present-sweep ambiguous. Scan the outer root alone",
					cleaned[j], cleaned[i])
			}
		}
	}
	return cleaned, nil
}

// isUnder reports whether child is at or below parent, comparing whole path
// segments so /models2 is not treated as living under /models.
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
