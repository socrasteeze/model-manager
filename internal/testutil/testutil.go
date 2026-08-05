// Package testutil holds helpers shared by tests across packages.
//
// Imported only from _test.go files. It exists so that a correction like the
// one below is made once rather than rediscovered per package.
package testutil

import (
	"path/filepath"
	"testing"
)

// TempDir is t.TempDir() with the path canonicalized the same way the daemon
// canonicalizes any directory an operator points it at.
//
// On Windows t.TempDir() hands back a path containing an 8.3 short name --
// C:\Users\SOCRAS~1\... for C:\Users\socrasteeze\... -- because TMP itself is
// usually stored that way. Every production path that takes a directory from
// outside runs it through filepath.EvalSymlinks (store.CanonicalRoot,
// api.confineToDir, api.settingDir), which expands that to the long form. That
// is correct and deliberate: a containment check has to compare canonical
// paths, or the same directory under two spellings reads as two directories.
//
// The consequence for tests is that passing a raw t.TempDir() into that code
// and then comparing the result against t.TempDir() compares two spellings of
// one path and fails -- with a diff that looks alarmingly like a path-traversal
// escape, since the two strings genuinely differ. Ten tests failed this way on
// Windows while passing on Linux, where TMPDIR has no short form.
//
// Canonicalizing here rather than loosening the comparison keeps the assertions
// exact: a real escape still fails, because both sides are canonical.
func TempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// Nothing to canonicalize against -- use what we were given rather
		// than failing the test for a directory that plainly exists.
		return dir
	}
	return filepath.Clean(resolved)
}
