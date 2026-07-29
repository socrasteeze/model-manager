//go:build windows

package tier

import "io/fs"

// fileIdentity has no cheap answer on Windows: obtaining a file index needs a
// handle open, which the scanner does but is not worth here. Zeroes mean the
// staged path simply misses the inode cache and gets hashed on the next scan,
// which is correct if slightly slower.
func fileIdentity(info fs.FileInfo) (device, inode uint64) {
	return 0, 0
}
