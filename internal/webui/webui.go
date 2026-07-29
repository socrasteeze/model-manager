// Package webui embeds the built front-end.
//
// The compiled assets are committed to the repository rather than built on
// demand. A distributable tool has to satisfy `go build` on a machine with no
// Node toolchain (spec §3: "download, run, works"), and the alternative is
// making every contributor and every CI job install npm to produce a binary.
//
// Rebuild with `make ui` after changing anything under web/.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI rooted at its index, or nil if this build embeds only
// the placeholder. A nil return makes the daemon serve the API alone rather than
// serving a broken page.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
