// Package scan walks model roots and records what it finds.
//
// Phase 0's job is to answer one question -- how many distinct models exist
// across the library, and how much of it is duplication -- and to do so in a way
// that survives being interrupted, restarted, and run again while a migration is
// still moving files underneath it.
package scan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Options configures a scan.
type Options struct {
	// Roots are the directories to walk. Nested roots are rejected.
	Roots []string

	// WorkersPerDevice is the hashing concurrency applied to each distinct
	// storage device.
	//
	// Scaling per device rather than globally is deliberate (spec §17): on
	// spinning rust, concurrent readers seek-thrash and frequently lose to
	// sequential large-buffer reads, so the right number for a HDD array and for
	// an NVMe drive are different, and a global pool would apply one to both.
	WorkersPerDevice int

	// BufferSize is the read chunk size per worker.
	BufferSize int

	// MaxHeaderBytes caps stored header blobs.
	MaxHeaderBytes int

	// UseProbe enables the second-tier sampled-probe cache fallback (spec §10.1).
	//
	// Off by default. It trades a full read for a sampled one on files that look
	// like a known model at both ends, which is valuable on a rescan after a
	// cross-volume migration -- but it binds those paths as provisional, and
	// Phase 0's entire output is a distinct-hash count that should be real rather
	// than inferred. Turn it on for rescans, not for the first pass.
	UseProbe bool

	// Progress, if set, is called periodically with a snapshot.
	Progress func(Snapshot)

	// ProgressInterval defaults to two seconds.
	ProgressInterval time.Duration

	// Logf receives human-readable notices. May be nil.
	Logf func(format string, args ...any)
}

// Snapshot is the live state of a running scan.
type Snapshot struct {
	FilesTotal  int64
	FilesDone   int64
	FilesHashed int64
	FilesCached int64
	FilesProbed int64
	Errors      int64
	BytesTotal  int64
	BytesDone   int64
	Elapsed     time.Duration
}

// RootResult is the outcome for one root.
type RootResult struct {
	Root        string
	ScanRunID   int64
	Status      string
	Counters    store.ScanCounters
	SweptAbsent int64
}

// Result is the outcome of a scan.
type Result struct {
	Roots     []RootResult
	Elapsed   time.Duration
	Cancelled bool
}

type counters struct {
	seen   atomic.Int64
	hashed atomic.Int64
	cached atomic.Int64
	probed atomic.Int64
	bytes  atomic.Int64
	errs   atomic.Int64
}

func (c *counters) snapshot() store.ScanCounters {
	return store.ScanCounters{
		FilesSeen:   c.seen.Load(),
		FilesHashed: c.hashed.Load(),
		FilesCached: c.cached.Load(),
		FilesProbed: c.probed.Load(),
		BytesHashed: c.bytes.Load(),
		Errors:      c.errs.Load(),
	}
}

type rootState struct {
	root  string
	runID int64
	c     counters
}

// Run executes a scan. It returns a Result even when the context is cancelled:
// an interrupted scan still recorded everything it committed, and the caller
// needs to be told what that was.
func Run(ctx context.Context, st *store.Store, opts Options) (*Result, error) {
	started := time.Now()

	roots, err := prepareRoots(opts.Roots)
	if err != nil {
		return nil, err
	}
	if opts.WorkersPerDevice <= 0 {
		opts.WorkersPerDevice = 1
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = 2 * time.Second
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// A run left in `running` by a killed process must not be mistaken for one
	// that completed, since the sweep keys off completed runs.
	if n, err := st.MarkInterruptedRuns(); err != nil {
		return nil, err
	} else if n > 0 {
		logf("marked %d scan run(s) from a previous process as interrupted", n)
	}

	states := make(map[string]*rootState, len(roots))
	var all []candidate

	for _, root := range roots {
		runID, err := st.BeginScanRun(root)
		if err != nil {
			return nil, err
		}
		state := &rootState{root: root, runID: runID}
		states[root] = state

		logf("walking %s", root)
		found, walkErr := walkRoot(ctx, root, func(path, kind string, err error) {
			state.c.errs.Add(1)
			_ = st.RecordError(runID, path, kind, err.Error())
		})
		if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
			// Record and continue to the next root: one unreadable root should
			// not discard the work available in the others.
			state.c.errs.Add(1)
			_ = st.RecordError(runID, root, "stat", walkErr.Error())
			logf("walk of %s failed: %v", root, walkErr)
		}
		state.c.seen.Add(int64(len(found)))
		all = append(all, found...)

		if ctx.Err() != nil {
			break
		}
	}

	var bytesTotal int64
	for _, c := range all {
		bytesTotal += c.size
	}
	logf("found %d model files, %s across %d root(s)",
		len(all), humanBytes(bytesTotal), len(roots))

	// Group by device so each storage device gets its own worker pool.
	byDevice := make(map[uint64][]candidate)
	for _, c := range all {
		byDevice[c.device] = append(byDevice[c.device], c)
	}
	// Ascending inode order approximates on-disk layout closely enough to matter
	// on a spinning array, where the alternative is a directory-order walk that
	// seeks across the platter.
	for dev := range byDevice {
		list := byDevice[dev]
		sort.Slice(list, func(i, j int) bool { return list[i].inode < list[j].inode })
	}

	var filesDone, bytesDone atomic.Int64

	progressDone := make(chan struct{})
	if opts.Progress != nil {
		go func() {
			t := time.NewTicker(opts.ProgressInterval)
			defer t.Stop()
			for {
				select {
				case <-progressDone:
					return
				case <-t.C:
					opts.Progress(buildSnapshot(states, filesDone.Load(), bytesDone.Load(),
						int64(len(all)), bytesTotal, time.Since(started)))
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for _, list := range byDevice {
		work := make(chan candidate)

		go func(list []candidate) {
			defer close(work)
			for _, c := range list {
				select {
				case <-ctx.Done():
					return
				case work <- c:
				}
			}
		}(list)

		for i := 0; i < opts.WorkersPerDevice; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := hashing.New(opts.BufferSize, opts.MaxHeaderBytes)
				for c := range work {
					if ctx.Err() != nil {
						return
					}
					state := states[c.root]
					processFile(st, h, c, state, opts.UseProbe)
					filesDone.Add(1)
					bytesDone.Add(c.size)
				}
			}()
		}
	}
	wg.Wait()
	close(progressDone)

	cancelled := ctx.Err() != nil

	result := &Result{Cancelled: cancelled, Elapsed: time.Since(started)}
	for _, root := range roots {
		state, ok := states[root]
		if !ok {
			continue
		}
		rr := RootResult{Root: root, ScanRunID: state.runID, Counters: state.c.snapshot()}

		switch {
		case cancelled:
			// The sweep marks every path this run did not observe as absent. After
			// an interrupted walk, that is most of the library -- so it is skipped
			// entirely rather than run against partial evidence (spec §6.2).
			rr.Status = store.StatusInterrupted
		default:
			swept, err := st.SweepAbsentPaths(root, state.runID)
			if err != nil {
				logf("sweeping %s: %v", root, err)
				rr.Status = store.StatusFailed
			} else {
				rr.SweptAbsent = swept
				rr.Status = store.StatusCompleted
			}
		}

		if err := st.FinishScanRun(state.runID, rr.Status, rr.Counters); err != nil {
			logf("finishing scan run %d: %v", state.runID, err)
		}
		result.Roots = append(result.Roots, rr)
	}

	if opts.Progress != nil {
		opts.Progress(buildSnapshot(states, filesDone.Load(), bytesDone.Load(),
			int64(len(all)), bytesTotal, result.Elapsed))
	}
	return result, nil
}

// processFile resolves one candidate through the cache tiers, falling back to a
// full read only when nothing cheaper can answer.
func processFile(st *store.Store, h *hashing.Hasher, c candidate, state *rootState, useProbe bool) {
	fail := func(kind string, err error) {
		state.c.errs.Add(1)
		_ = st.RecordError(state.runID, c.path, kind, err.Error())
	}

	// Tier 1: the file is already known at this exact (device, inode, size,
	// mtime). Nothing to read.
	if sha, ok, err := st.LookupByCacheKey(c.device, c.inode, c.size, c.mtimeNs); err != nil {
		fail("stat", err)
		return
	} else if ok {
		if err := st.TouchPath(pathRow(c, sha, state.runID, false)); err != nil {
			fail("stat", err)
			return
		}
		state.c.cached.Add(1)
		return
	}

	// Tier 2: a sampled probe over both ends. Cheap, and deliberately weak.
	if useProbe {
		probe, err := h.Probe(c.path)
		if err == nil && probe.Size == c.size {
			if sha, ok, err := st.LookupByProbe(c.size, probe.ProbeSHA256); err == nil && ok {
				// Provisional: a sampled match is a hint. Binding it as confirmed
				// would let a false positive assign a wrong identity permanently,
				// which is the failure class this whole design exists to remove.
				if err := st.TouchPath(pathRow(c, sha, state.runID, true)); err != nil {
					fail("stat", err)
					return
				}
				state.c.probed.Add(1)
				return
			}
		}
	}

	// Tier 3: read the whole file.
	res, err := h.Full(c.path)
	if err != nil {
		kind := "read"
		if errors.Is(err, hashing.ErrChangedDuringHash) {
			// Expected during a migration. The file is left unrecorded and will be
			// picked up by the next pass, which is the correct outcome: a
			// partially-written file hashed into an identity is permanent.
			kind = "race"
		}
		fail(kind, err)
		return
	}

	file := store.ModelFile{
		SHA256:        res.SHA256,
		WeightsSHA256: res.WeightsSHA256,
		WeightsOffset: res.WeightsOffset,
		ProbeSHA256:   res.ProbeSHA256,
		Size:          res.Size,
		Format:        res.Format,
		HeaderBlob:    res.Header.Blob,
		HeaderOffset:  res.Header.BlobOffset,

		HeaderTruncated: res.Header.Truncated,
	}
	row := pathRow(c, res.SHA256, state.runID, false)
	// Prefer the facts observed through the descriptor that was actually read
	// over the ones the walk saw earlier.
	row.Size = res.Size
	row.MtimeNs = res.MtimeNs

	if err := st.UpsertFileAndPath(file, row); err != nil {
		fail("read", err)
		return
	}
	if res.Header.ParseErr != nil && res.Header.HasWeightsRegion {
		// The offset was found but the blob capture failed. Worth recording: the
		// record is usable, the stored header is not.
		_ = st.RecordError(state.runID, c.path, "header", res.Header.ParseErr.Error())
	}

	state.c.hashed.Add(1)
	state.c.bytes.Add(res.Size)
}

func pathRow(c candidate, sha string, runID int64, provisional bool) store.FilePath {
	return store.FilePath{
		SHA256:      sha,
		Path:        c.path,
		Root:        c.root,
		Device:      c.device,
		Inode:       c.inode,
		Size:        c.size,
		MtimeNs:     c.mtimeNs,
		Present:     true,
		Provisional: provisional,
		ScanRunID:   runID,
	}
}

func buildSnapshot(states map[string]*rootState, filesDone, bytesDone, filesTotal, bytesTotal int64, elapsed time.Duration) Snapshot {
	s := Snapshot{
		FilesTotal: filesTotal,
		FilesDone:  filesDone,
		BytesTotal: bytesTotal,
		BytesDone:  bytesDone,
		Elapsed:    elapsed,
	}
	for _, st := range states {
		s.FilesHashed += st.c.hashed.Load()
		s.FilesCached += st.c.cached.Load()
		s.FilesProbed += st.c.probed.Load()
		s.Errors += st.c.errs.Load()
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
