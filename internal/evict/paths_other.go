//go:build !windows && !darwin

package evict

// See paths_windows.go. Everywhere else, two names that differ in case are two
// different files, and a call that deletes one must not treat them as one.
const caseInsensitivePaths = false
