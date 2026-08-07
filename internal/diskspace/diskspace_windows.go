package diskspace

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// avail asks Windows for the space this caller may actually use.
//
// GetDiskFreeSpaceEx reports two different numbers and the difference matters
// here: freeBytesAvailableToCaller respects a per-user quota, totalFreeBytes
// does not. A mapped network drive -- which is exactly where a model library
// tends to live -- is routinely quota'd, so the second would promise room this
// process cannot write into.
func avail(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("diskspace: encoding path %s: %w", dir, err)
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("diskspace: %s: %w", dir, err)
	}
	// Clamped rather than returned raw: the API is unsigned and a volume larger
	// than 8 EiB would wrap into a negative, which would read as "no space" and
	// refuse every download on the largest arrays this tool is built for.
	if freeToCaller > 1<<62 {
		return 1 << 62, nil
	}
	return int64(freeToCaller), nil
}
