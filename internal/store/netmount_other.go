//go:build !linux && !windows && !darwin

package store

// classifyMount has no implementation on this platform. Returning MountUnknown
// means Open proceeds with a warning rather than refusing: blocking a user on a
// local disk because we could not identify their filesystem is the worse error.
func classifyMount(path string) (MountKind, string) {
	return MountUnknown, ""
}
