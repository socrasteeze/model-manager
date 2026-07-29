package api

import "github.com/socrasteeze/model-manager/internal/blobstore"

// blobstoreMIME re-sniffs bytes on the way out.
//
// The stored MIME came from whatever produced the blob, sometimes a sidecar. A
// preview served as text/html would execute in the UI's own origin, so the type
// is decided from the bytes at serve time rather than trusted from the database.
func blobstoreMIME(data []byte) string {
	return blobstore.DetectMIME(data)
}
