package store

import "fmt"

// SQLite's locking is broken over network filesystems, and putting the database
// on one is a well-known corruption vector. The daemon refuses to start if its
// database path is on a network mount (spec §10.5).
//
// The check is advisory-by-necessity on platforms where we cannot determine the
// filesystem type: an unknown answer must not block a user on an ordinary local
// disk, but a *known* network answer is fatal.

// MountKind is the classification of the filesystem a path lives on.
type MountKind int

const (
	// MountUnknown means the platform gave us no usable answer.
	MountUnknown MountKind = iota
	// MountLocal means the path is on a locally attached filesystem.
	MountLocal
	// MountNetwork means the path is on a network filesystem.
	MountNetwork
)

// ErrNetworkMount is returned by Open when the database path resolves to a
// network filesystem.
type ErrNetworkMount struct {
	Path   string
	FSType string
}

func (e *ErrNetworkMount) Error() string {
	return fmt.Sprintf(
		"refusing to open database on network filesystem %q (path %s): "+
			"SQLite locking is unreliable over network mounts and will corrupt the database. "+
			"Put the database on a locally attached disk (--db)",
		e.FSType, e.Path)
}

// networkFSTypes are filesystem type names that are network-backed. Matched
// exactly, or by prefix for the FUSE families whose names carry a transport
// suffix.
var networkFSTypes = map[string]bool{
	"nfs":             true,
	"nfs4":            true,
	"cifs":            true,
	"smb":             true,
	"smb2":            true,
	"smb3":            true,
	"smbfs":           true,
	"afpfs":           true,
	"9p":              true,
	"ncpfs":           true,
	"coda":            true,
	"afs":             true,
	"ceph":            true,
	"glusterfs":       true,
	"lustre":          true,
	"beegfs":          true,
	"webdav":          true,
	"davfs":           true,
	"fuse.sshfs":      true,
	"fuse.davfs2":     true,
	"fuse.rclone":     true,
	"fuse.s3fs":       true,
	"fuse.gvfsd-fuse": true,
	"osxfuse.smbfs":   true,
}

func isNetworkFSType(fstype string) bool {
	return networkFSTypes[fstype]
}
