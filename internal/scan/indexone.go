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
	"github.com/socrasteeze/model-manager/internal/modelformat"
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
	return IndexPublished(st, path, root, nil)
}

// IndexPublished records a file, reusing a content identity the caller has
// already established instead of reading the file a second time.
//
// pre is what the caller's own read established about the *bytes*. It is never
// trusted about the *file*. Device and inode always come from a stat performed
// here, after whatever move put the file where it now is: a rename preserves an
// inode and a cross-filesystem copy does not, and cross-filesystem is the
// ordinary shape of this deployment, since the download work directory sits
// beside the database while the models live on the array. The four-tuple on the
// path row is what the next incremental scan and every eviction compare
// against, so a tuple describing the staging file would make the next scan
// re-hash every downloaded model and make eviction refuse to remove any of
// them -- both silently.
//
// Two facts are re-derived and must agree with pre before it is used: the size
// on disk, and the format the final name detects as. Disagreement means pre
// describes something other than the file at path, so pre is discarded and the
// file is read in full. Falling back costs one read; a wrong row is permanent.
//
// pre == nil is the ordinary single-file path: read everything here.
func IndexPublished(st *store.Store, path, root string, pre *hashing.Result) (string, error) {
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

	var res hashing.Result
	reused := pre != nil &&
		pre.SHA256 != "" &&
		pre.Size == info.Size() &&
		pre.Format == modelformat.DetectFormat(abs)

	if reused {
		res = *pre
		// The one thing pre cannot know: where the file ended up. Its size and
		// mtime describe the staging file, and a cross-filesystem publish gives
		// the published copy a new mtime -- so these two come from the stat
		// above, alongside the device and inode taken from the same observation.
		//
		// Scoped to this branch deliberately. On the fallback below, Size comes
		// from a read that verifyStable has certified describes the bytes it
		// hashed; overwriting it with a stat taken before that read would put a
		// size and a hash from two different instants in one row. That row would
		// never be corrected, because model_file.size is written on insert and
		// is not in the upsert's update list -- and the file endpoint refuses to
		// serve anything whose size disagrees with the index, so the model would
		// be permanently unservable with no diagnosable cause.
		res.Size = info.Size()
		res.MtimeNs = info.ModTime().UnixNano()
	} else {
		res, err = hashing.New(0, 0).Full(abs)
		if err != nil {
			return "", fmt.Errorf("scan: hashing %s: %w", abs, err)
		}
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
	// Counted as cached rather than hashed when the identity was handed in.
	// Those bytes were read by the caller, not here, and reporting them as
	// hashed would have the one change whose whole purpose is "we stopped
	// reading twelve gigabytes" still report twelve gigabytes read -- the same
	// distinction a tier-1 cache hit already makes during a walk.
	counters := store.ScanCounters{FilesSeen: 1, FilesHashed: 1, BytesHashed: res.Size}
	if reused {
		counters = store.ScanCounters{FilesSeen: 1, FilesCached: 1}
	}
	if err := st.FinishScanRun(runID, store.StatusCompleted, counters); err != nil {
		return res.SHA256, err
	}
	return res.SHA256, nil
}
