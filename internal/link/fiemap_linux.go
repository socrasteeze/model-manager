//go:build linux

package link

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Shared-extent detection (spec §9.4).
//
// Each reflink reports its full size to du and to a naive scan, so a duplicate
// report that is not shared-extent aware will loudly announce every intentional
// view as wasted space. Detecting sharing rather than comparing apparent sizes
// is what keeps the duplication figure meaningful once views exist.

const (
	fsIocFiemap = 0xC020660B

	// FIEMAP_FLAG_SYNC forces a flush before mapping, so unwritten extents are
	// not reported as holes.
	fiemapFlagSync = 0x0001

	// FIEMAP_EXTENT_SHARED marks an extent shared with another file. This is the
	// bit the whole feature rests on.
	fiemapExtentShared = 0x2000
	fiemapExtentLast   = 0x0001
)

type fiemapExtent struct {
	Logical   uint64
	Physical  uint64
	Length    uint64
	Reserved  [2]uint64
	Flags     uint32
	Reserved2 [3]uint32
}

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

// ExtentInfo is what FIEMAP reported about a file.
type ExtentInfo struct {
	// Supported is false when the filesystem or platform cannot answer, in which
	// case every other field is meaningless and the caller must say so rather
	// than presenting a zero as a measurement.
	Supported bool

	// ApparentBytes is what the file claims to be.
	ApparentBytes int64

	// SharedBytes is how much is shared with at least one other file.
	SharedBytes int64

	// ExclusiveBytes is what this file alone occupies -- the number a duplicate
	// report should actually show.
	ExclusiveBytes int64
}

// maxExtentsPerCall bounds one ioctl. A heavily fragmented multi-gigabyte model
// can have a great many extents, and asking for all of them in one allocation
// would be a way to run out of memory on a file the tool is only measuring.
const maxExtentsPerCall = 512

// Extents reports how much of a file is shared with other files.
//
// Per-file syscall work across a large library is not free, so callers are
// expected to cache the result keyed on inode and mtime, exactly like the hash
// cache (§9.4).
func Extents(path string) (ExtentInfo, error) {
	info := ExtentInfo{}

	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return info, err
	}
	info.ApparentBytes = stat.Size()
	if info.ApparentBytes == 0 {
		info.Supported = true
		return info, nil
	}

	buf := make([]byte, int(unsafe.Sizeof(fiemapHeader{}))+
		maxExtentsPerCall*int(unsafe.Sizeof(fiemapExtent{})))

	var logicalStart uint64
	for {
		header := (*fiemapHeader)(unsafe.Pointer(&buf[0]))
		*header = fiemapHeader{
			Start:       logicalStart,
			Length:      ^uint64(0) - logicalStart,
			Flags:       fiemapFlagSync,
			ExtentCount: maxExtentsPerCall,
		}

		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap,
			uintptr(unsafe.Pointer(&buf[0])))
		if errno != 0 {
			// ENOTTY or EOPNOTSUPP means this filesystem has no FIEMAP. That is
			// a normal answer on plenty of filesystems, not an error worth
			// failing the caller over -- it just means sharing is unknowable
			// here.
			if errno == unix.ENOTTY || errno == unix.EOPNOTSUPP || errno == unix.ENOSYS {
				return ExtentInfo{ApparentBytes: info.ApparentBytes}, nil
			}
			return info, fmt.Errorf("link: FIEMAP on %s: %w", path, errno)
		}

		mapped := int(header.MappedExtents)
		if mapped == 0 {
			break
		}

		extents := unsafe.Slice(
			(*fiemapExtent)(unsafe.Pointer(&buf[unsafe.Sizeof(fiemapHeader{})])), mapped)

		var last fiemapExtent
		for _, e := range extents {
			if e.Flags&fiemapExtentShared != 0 {
				info.SharedBytes += int64(e.Length)
			} else {
				info.ExclusiveBytes += int64(e.Length)
			}
			last = e
		}

		if last.Flags&fiemapExtentLast != 0 {
			break
		}
		next := last.Logical + last.Length
		if next <= logicalStart {
			// No forward progress: stop rather than loop forever on a
			// filesystem reporting something we do not understand.
			break
		}
		logicalStart = next
	}

	info.Supported = true
	return info, nil
}

// SharedExtentsSupported reports whether this platform can answer at all.
func SharedExtentsSupported() bool { return true }
