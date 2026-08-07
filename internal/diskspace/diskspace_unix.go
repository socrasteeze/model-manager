//go:build linux || darwin

package diskspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// avail asks statfs for the space this caller may actually use.
//
// Bavail, not Bfree. Bfree counts the blocks reserved for root, which this
// process cannot write into -- so it is optimistic by exactly the reserve, on
// exactly the full filesystems where the answer decides something.
//
// The conversions are required rather than cosmetic: Bsize is int64 on Linux and
// uint32 on Darwin, and Bavail is unsigned on one and signed on the other.
func avail(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("diskspace: %s: %w", dir, err)
	}
	blocks := int64(st.Bavail)
	size := int64(st.Bsize)
	if blocks < 0 || size <= 0 {
		return 0, fmt.Errorf("diskspace: %s: implausible statfs result", dir)
	}
	// Overflow guard for the same reason the Windows path has one: a very large
	// filesystem must not wrap into a negative and read as "full".
	if blocks > (1<<62)/size {
		return 1 << 62, nil
	}
	return blocks * size, nil
}
