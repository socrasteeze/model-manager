//go:build !linux

package link

import "os"

// ExtentInfo mirrors the Linux type so callers compile everywhere.
type ExtentInfo struct {
	Supported      bool
	ApparentBytes  int64
	SharedBytes    int64
	ExclusiveBytes int64
}

// Extents cannot detect sharing on this platform.
//
// Windows and macOS fall back to reporting apparent duplicates with a caveat
// rather than blocking the feature (§9.4). Supported stays false so the caller
// says "unknown" instead of presenting a zero as a measurement.
func Extents(path string) (ExtentInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ExtentInfo{}, err
	}
	return ExtentInfo{Supported: false, ApparentBytes: info.Size()}, nil
}

// SharedExtentsSupported reports whether this platform can answer at all.
func SharedExtentsSupported() bool { return false }
