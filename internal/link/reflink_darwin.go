//go:build darwin

package link

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// reflink uses APFS clonefile, which is the same copy-on-write idea as btrfs
// reflinks.
func reflink(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return fmt.Errorf("clonefile: %w", err)
	}
	return nil
}

func blockClone(src, dst string) error {
	return fmt.Errorf("block cloning is a Windows ReFS feature")
}

func filesystemType(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return ""
	}
	return unix.ByteSliceToString(st.Fstypename[:])
}
