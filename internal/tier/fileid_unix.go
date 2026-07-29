//go:build !windows

package tier

import (
	"io/fs"
	"syscall"
)

// fileIdentity returns the (device, inode) pair the index keys its cache on.
func fileIdentity(info fs.FileInfo) (device, inode uint64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino)
}
