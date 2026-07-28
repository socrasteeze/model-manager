//go:build !windows

package scan

import (
	"fmt"
	"io/fs"
	"syscall"
)

// fileID returns the (device, inode) pair that keys the incremental cache.
//
// A move within a filesystem preserves the inode, which is the entire reason the
// cache is keyed on it rather than on path: the spec's premise is that paths
// churn constantly, and a path-keyed cache would re-hash every migrated file --
// the exact workload the cache exists to avoid (spec §10.1).
func fileID(_ string, info fs.FileInfo) (device, inode uint64, err error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("scan: no stat data available for %s", info.Name())
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}
