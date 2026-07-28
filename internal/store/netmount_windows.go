//go:build windows

package store

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const driveRemote = 4 // DRIVE_REMOTE

// classifyMount treats two things as network on Windows: a UNC path, and a drive
// letter that GetDriveTypeW reports as DRIVE_REMOTE (a mapped network drive).
// Checking only for UNC would miss `Z:\` pointing at the same share, which is how
// most people actually reach a NAS on Windows.
func classifyMount(path string) (MountKind, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return MountUnknown, ""
	}
	abs = filepath.Clean(abs)

	if strings.HasPrefix(abs, `\\`) && !strings.HasPrefix(abs, `\\?\`) {
		return MountNetwork, "unc"
	}
	// Long-path form of a UNC: \\?\UNC\server\share
	if strings.HasPrefix(strings.ToUpper(abs), `\\?\UNC\`) {
		return MountNetwork, "unc"
	}

	vol := filepath.VolumeName(abs)
	if len(vol) != 2 || vol[1] != ':' {
		return MountUnknown, ""
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return MountUnknown, ""
	}
	switch windows.GetDriveType(root) {
	case driveRemote:
		return MountNetwork, "mapped-network-drive"
	case 0, 1: // DRIVE_UNKNOWN, DRIVE_NO_ROOT_DIR
		return MountUnknown, ""
	default:
		return MountLocal, "local"
	}
}
