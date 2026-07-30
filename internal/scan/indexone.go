package scan

// Indexing a single file.
//
// A freshly downloaded model has to enter the index the same way a scanned one
// does, or it acquires a second, subtly different identity: a different notion
// of what its weights hash is, or a path row missing the (device, inode) key
// that makes re-scans cheap. Both would surface later as a duplicate or a
// needless re-hash.
//
// So this is the scan pipeline's tier-3 path applied to one file, sharing the
// same hasher, the same ModelFile construction and the same UpsertFileAndPath
// call rather than reimplementing them.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/store"
)

// IndexFile hashes one file and records it under root.
//
// Always a full read: the caller has just written these bytes, so there is no
// prior cache entry to trust and a sampled probe would only risk a provisional
// binding for a file whose identity is about to be checked against an expected
// hash anyway.
//
// Returns the recorded SHA256.
func IndexFile(st *store.Store, path, root string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("scan: resolving %s: %w", path, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("scan: %s: %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("scan: %s is a directory", abs)
	}

	device, inode, err := fileID(abs, info)
	if err != nil {
		return "", fmt.Errorf("scan: identifying %s: %w", abs, err)
	}

	h := hashing.New(0, 0)
	res, err := h.Full(abs)
	if err != nil {
		return "", fmt.Errorf("scan: hashing %s: %w", abs, err)
	}

	// A scan run so the path row is attributable, and so this file is not
	// mistaken for one left behind by an earlier run of a different root.
	runID, err := st.BeginScanRun(root)
	if err != nil {
		return "", err
	}

	file := store.ModelFile{
		SHA256:          res.SHA256,
		WeightsSHA256:   res.WeightsSHA256,
		WeightsOffset:   res.WeightsOffset,
		ProbeSHA256:     res.ProbeSHA256,
		Size:            res.Size,
		Format:          res.Format,
		HeaderBlob:      res.Header.Blob,
		HeaderOffset:    res.Header.BlobOffset,
		HeaderTruncated: res.Header.Truncated,
	}
	row := store.FilePath{
		SHA256:  res.SHA256,
		Path:    abs,
		Root:    root,
		Device:  device,
		Inode:   inode,
		Size:    res.Size,
		MtimeNs: res.MtimeNs,
		Present: true,
		// Never provisional: this is a full read, not a probe match.
		Provisional: false,
		ScanRunID:   runID,
	}

	if err := st.UpsertFileAndPath(file, row); err != nil {
		return "", err
	}
	if err := st.FinishScanRun(runID, store.StatusCompleted, store.ScanCounters{
		FilesSeen:   1,
		FilesHashed: 1,
		BytesHashed: res.Size,
	}); err != nil {
		return res.SHA256, err
	}
	return res.SHA256, nil
}
