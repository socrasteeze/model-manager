// Package diskspace reports free space on the filesystem holding a directory.
//
// It exists so a multi-gigabyte download fails in the first second with "this
// needs 14 GB and there is 3 GB free" instead of in the fortieth minute with
// ENOSPC and a half-written file. The download manager already gives a disk-full
// condition its own error type, precisely because "retry" is the wrong advice
// for it; this asks the same question before the transfer rather than after.
//
// Every answer here is a snapshot and something else may fill the disk a moment
// later. That is fine: this is a courtesy that catches the case where the space
// was never there, not a reservation.
package diskspace

import "errors"

// ErrUnsupported means this platform has no implementation.
//
// A sentinel rather than a zero, because a caller must be able to tell "there is
// no space" from "I could not find out" -- the first is a refusal and the second
// must not be.
var ErrUnsupported = errors.New("diskspace: not supported on this platform")

// Avail returns the bytes available to this process on the filesystem holding
// dir.
//
// dir must exist. A destination subdirectory that has not been created yet
// cannot be asked about, so callers walk up to the nearest existing ancestor
// first -- that is the filesystem the write actually lands on anyway.
func Avail(dir string) (int64, error) { return avail(dir) }

// Margin is the headroom to require beyond the transfer itself: 512 MiB or 5% of
// the file, whichever is larger.
//
// A flat number alone is wrong at both ends -- 512 MiB is most of a small volume
// and nothing beside a 40 GB checkpoint -- and a pure percentage is wrong for a
// 200 MB LoRA onto a nearly full array. The larger of the two is the only one of
// the three that is never absurd.
func Margin(size int64) int64 {
	const floor = 512 << 20
	if pct := size / 20; pct > floor {
		return pct
	}
	return floor
}
