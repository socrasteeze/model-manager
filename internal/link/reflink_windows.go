//go:build windows

package link

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// reflink has no NTFS equivalent. ReFS block cloning is the Windows mechanism
// and lives in blockClone; keeping them separate means a probe reports honestly
// which one is actually available rather than conflating the two.
func reflink(src, dst string) error {
	return fmt.Errorf("reflinks are not an NTFS feature; ReFS block cloning is probed separately")
}

const (
	fsctlDuplicateExtentsToFile  = 0x00098344
	fileSupportsBlockRefcounting = 0x08000000
)

// duplicateExtentsData is DUPLICATE_EXTENTS_DATA.
type duplicateExtentsData struct {
	FileHandle       windows.Handle
	SourceFileOffset int64
	TargetFileOffset int64
	ByteCount        int64
}

// blockClone performs a ReFS / Dev Drive copy-on-write clone.
//
// This is what §9.3 asks for as the first Windows preference: genuinely
// copy-on-write, unlike a hardlink, so a tool rewriting a header in place
// diverges instead of mutating the original.
func blockClone(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()

	// The destination must exist and be sized before extents can be duplicated
	// into it; the call maps ranges, it does not extend the file.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	cleanup := func() {
		out.Close()
		// A zero-length or half-mapped file where a model should be is worse
		// than no file: a consuming tool will try to load it.
		os.Remove(dst)
	}
	if err := out.Truncate(size); err != nil {
		cleanup()
		return err
	}

	// Ranges must be cluster-aligned. 64KiB covers every cluster size ReFS uses,
	// and the final partial chunk is rounded up to it -- the file was already
	// truncated to the real length, so the tail beyond EOF is not addressable.
	const chunk = 64 << 10
	clusterAlign := func(n int64) int64 { return (n + chunk - 1) / chunk * chunk }

	for offset := int64(0); offset < size; offset += chunk {
		remaining := size - offset
		count := int64(chunk)
		if remaining < chunk {
			count = clusterAlign(remaining)
		}

		data := duplicateExtentsData{
			FileHandle:       windows.Handle(in.Fd()),
			SourceFileOffset: offset,
			TargetFileOffset: offset,
			ByteCount:        count,
		}
		var returned uint32
		err := windows.DeviceIoControl(
			windows.Handle(out.Fd()),
			fsctlDuplicateExtentsToFile,
			(*byte)(unsafe.Pointer(&data)),
			uint32(unsafe.Sizeof(data)),
			nil, 0,
			&returned, nil,
		)
		if err != nil {
			cleanup()
			return fmt.Errorf("DUPLICATE_EXTENTS_TO_FILE at offset %d: %w", offset, err)
		}
	}

	return out.Close()
}

// filesystemType reports the volume's filesystem name, which is how ReFS is
// distinguished from NTFS -- and that distinction decides the whole Windows
// strategy, since NTFS has no copy-on-write equivalent at all (§9.3).
func filesystemType(path string) string {
	abs, err := windows.UTF16PtrFromString(volumeRoot(path))
	if err != nil {
		return ""
	}
	var (
		nameBuf   [windows.MAX_PATH + 1]uint16
		fsNameBuf [windows.MAX_PATH + 1]uint16
		serial    uint32
		maxLen    uint32
		flags     uint32
	)
	err = windows.GetVolumeInformation(
		abs,
		&nameBuf[0], uint32(len(nameBuf)),
		&serial, &maxLen, &flags,
		&fsNameBuf[0], uint32(len(fsNameBuf)),
	)
	if err != nil {
		return ""
	}
	name := windows.UTF16ToString(fsNameBuf[:])

	// The capability flag is more reliable than the filesystem name: a Dev Drive
	// is ReFS, but so are volumes where block refcounting is unavailable.
	if flags&fileSupportsBlockRefcounting != 0 {
		return name + " (block cloning supported)"
	}
	return name
}

func volumeRoot(path string) string {
	abs, err := windows.FullPath(path)
	if err != nil {
		abs = path
	}
	if len(abs) >= 2 && abs[1] == ':' {
		return abs[:2] + `\`
	}
	return abs
}
