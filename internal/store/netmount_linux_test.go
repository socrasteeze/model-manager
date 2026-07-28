//go:build linux

package store

import (
	"path/filepath"
	"testing"
)

func TestClassifyMountLocalTempDir(t *testing.T) {
	kind, fstype := classifyMount(t.TempDir())
	if kind == MountNetwork {
		t.Fatalf("temp dir classified as network (%s)", fstype)
	}
}

// The database file does not exist before the first Open, so classification has
// to fall back to the deepest existing ancestor rather than giving up.
func TestClassifyMountNonexistentFile(t *testing.T) {
	kind, _ := classifyMount(filepath.Join(t.TempDir(), "sub", "master.db"))
	if kind == MountUnknown {
		t.Fatal("classification gave up on a not-yet-created database path")
	}
}

func TestPathUnder(t *testing.T) {
	cases := []struct {
		abs, mount string
		want       bool
	}{
		{"/mnt/share/db", "/", true},
		{"/mnt/share/db", "/mnt/share", true},
		{"/mnt/share", "/mnt/share", true},
		// The prefix must be path-segment aware: /mnt/share2 is not under
		// /mnt/share, and a naive HasPrefix says it is.
		{"/mnt/share2/db", "/mnt/share", false},
		{"/mnt", "/mnt/share", false},
	}
	for _, c := range cases {
		if got := pathUnder(c.abs, c.mount); got != c.want {
			t.Errorf("pathUnder(%q, %q) = %v, want %v", c.abs, c.mount, got, c.want)
		}
	}
}

func TestUnescapeMountField(t *testing.T) {
	cases := map[string]string{
		`/mnt/plain`:         `/mnt/plain`,
		`/mnt/with\040space`: `/mnt/with space`,
		`/mnt/tab\011here`:   "/mnt/tab\there",
		`/mnt/back\134slash`: `/mnt/back\slash`,
		`/mnt/trailing\`:     `/mnt/trailing\`,
		`/mnt/not\9octal`:    `/mnt/not\9octal`,
	}
	for in, want := range cases {
		if got := unescapeMountField(in); got != want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNetworkFSTypeDetection(t *testing.T) {
	for _, fs := range []string{"nfs4", "cifs", "smb3", "fuse.sshfs"} {
		if !isNetworkFSType(fs) {
			t.Errorf("%s not recognized as a network filesystem", fs)
		}
	}
	for _, fs := range []string{"btrfs", "ext4", "xfs", "zfs", "ntfs3", "overlay"} {
		if isNetworkFSType(fs) {
			t.Errorf("%s wrongly classified as a network filesystem", fs)
		}
	}
}
