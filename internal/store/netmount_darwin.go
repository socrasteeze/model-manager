//go:build darwin

package store

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// classifyMount uses statfs, which on darwin reports the filesystem type name
// directly (smbfs, nfs, webdav, ...).
func classifyMount(path string) (MountKind, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return MountUnknown, ""
	}
	abs = deepestExisting(abs)

	var st unix.Statfs_t
	if err := unix.Statfs(abs, &st); err != nil {
		return MountUnknown, ""
	}
	fstype := unix.ByteSliceToString(st.Fstypename[:])
	if isNetworkFSType(fstype) {
		return MountNetwork, fstype
	}
	// MNT_LOCAL is authoritative when the type name is unfamiliar.
	if st.Flags&unix.MNT_LOCAL == 0 {
		return MountNetwork, fstype
	}
	return MountLocal, fstype
}

func deepestExisting(abs string) string {
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs
		}
		abs = parent
	}
}
