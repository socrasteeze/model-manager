//go:build linux

package link

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reflink performs a btrfs/XFS copy-on-write clone via the FICLONE ioctl.
//
// This is the preferred mechanism on the target array (§9.2): a real file
// sharing blocks with the original, at near-zero disk cost, indistinguishable
// from an ordinary file to ComfyUI, Swarm, Stability Matrix, and anything
// reading over SMB.
func reflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}

	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		// Remove the empty destination: a zero-byte file where a model should be
		// is worse than no file, because a consuming tool will try to load it.
		os.Remove(dst)
		return fmt.Errorf("FICLONE: %w", err)
	}
	return out.Close()
}

// blockClone is the Windows ReFS mechanism; it does not exist here.
func blockClone(src, dst string) error {
	return fmt.Errorf("block cloning is a Windows ReFS feature")
}

// filesystemType reports the filesystem backing a path, which is useful context
// even though every capability decision is made empirically.
func filesystemType(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return ""
	}
	switch st.Type {
	case 0x9123683E:
		return "btrfs"
	case 0x58465342:
		return "xfs"
	case 0xEF53:
		return "ext2/3/4"
	case 0x2FC12FC1:
		return "zfs"
	case 0xFF534D42:
		return "cifs/smb"
	case 0x6969:
		return "nfs"
	case 0x01021994:
		return "tmpfs"
	case 0x794C7630:
		return "overlayfs"
	case 0x5346544E:
		return "ntfs"
	case 0x4d44:
		return "vfat"
	}
	return fmt.Sprintf("unknown (0x%x)", st.Type)
}
