//go:build !linux && !darwin && !windows

package diskspace

// avail has no implementation on this platform.
//
// ErrUnsupported rather than an error the caller might treat as a refusal, and
// the caller is expected to proceed: this follows the mount classifier's policy
// rather than the reflink probe's, because of what the answer is used for.
// Reflink is asked "clone this file", where "I cannot" is the only honest reply.
// This is asked "is there room?", and refusing a download because we could not
// measure a disk that probably has plenty is the worse error by a wide margin.
func avail(string) (int64, error) { return 0, ErrUnsupported }
