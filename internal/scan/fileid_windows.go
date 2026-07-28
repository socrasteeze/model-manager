//go:build windows

package scan

import (
	"fmt"
	"io/fs"

	"golang.org/x/sys/windows"
)

// fileID returns a (device, inode) equivalent on Windows: the volume serial
// number paired with the 64-bit file index.
//
// Windows exposes no inode through a plain stat, so this costs a handle open per
// file. That is deliberate -- microseconds against the seconds a full hash would
// take, and skipping it would mean falling back to a path-keyed cache, which
// misses on precisely the moved files the cache exists to serve.
func fileID(path string, _ fs.FileInfo) (device, inode uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("scan: encoding path %s: %w", path, err)
	}

	// Zero desired access asks for metadata only, so this succeeds on files the
	// scanner has no read permission for. BACKUP_SEMANTICS lets the same call
	// work if the target turns out to be a directory.
	h, err := windows.CreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("scan: opening %s for identity: %w", path, err)
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, fmt.Errorf("scan: reading file identity for %s: %w", path, err)
	}

	device = uint64(info.VolumeSerialNumber)
	inode = uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return device, inode, nil
}
