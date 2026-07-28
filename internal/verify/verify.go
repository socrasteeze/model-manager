// Package verify re-reads files and checks the index against what is actually on
// disk.
//
// It serves the two verification duties spec §17 assigns to Phase 0 -- prove the
// recorded hashes are the real hashes, and prove the index is trustworthy before
// anything downstream is allowed to act on it -- plus one the scanner cannot do
// itself: confirming paths that were bound by sampled probe rather than by a
// full read (spec §10.1).
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Options configures a verification pass.
type Options struct {
	// Sample caps how many paths are checked. Zero or less means all of them.
	Sample int

	// ProvisionalOnly restricts the pass to paths bound by probe.
	ProvisionalOnly bool

	Workers        int
	BufferSize     int
	MaxHeaderBytes int

	Logf func(format string, args ...any)
}

// Mismatch is a path whose contents are not what the index says.
type Mismatch struct {
	Path        string
	RecordedSHA string
	ActualSHA   string
	WasProbe    bool
}

// Result is the outcome of a verification pass.
type Result struct {
	Checked    int64
	Matched    int64
	Mismatched int64
	Missing    int64
	Errors     int64

	// Confirmed counts provisional paths promoted to confirmed.
	Confirmed int64

	// ProbeMisbindings counts provisional paths whose sampled match turned out to
	// be wrong. A non-zero value here is the empirical argument for why a probe
	// match never confers identity.
	ProbeMisbindings int64

	Mismatches []Mismatch
	Elapsed    time.Duration
}

// Run verifies paths by re-reading them.
func Run(ctx context.Context, st *store.Store, opts Options) (*Result, error) {
	started := time.Now()
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	paths, err := st.ListPathsForVerify(opts.ProvisionalOnly, opts.Sample)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return &Result{Elapsed: time.Since(started)}, nil
	}
	logf("verifying %d path(s) by full re-read", len(paths))

	var (
		checked, matched, mismatched atomic.Int64
		missing, errCount            atomic.Int64
		confirmed, probeMisbindings  atomic.Int64
		mu                           sync.Mutex
		mismatches                   []Mismatch
	)

	work := make(chan store.FilePath)
	go func() {
		defer close(work)
		for _, p := range paths {
			select {
			case <-ctx.Done():
				return
			case work <- p:
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hashing.New(opts.BufferSize, opts.MaxHeaderBytes)

			for p := range work {
				if ctx.Err() != nil {
					return
				}
				res, err := h.Full(p.Path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						// The file went away between the scan and now. That is a
						// fact about the world, not a failure.
						if err := st.SetPathAbsent(p.ID); err != nil {
							logf("marking %s absent: %v", p.Path, err)
						}
						missing.Add(1)
						continue
					}
					errCount.Add(1)
					logf("verifying %s: %v", p.Path, err)
					continue
				}
				checked.Add(1)

				if err := st.UpsertFile(store.ModelFile{
					SHA256:          res.SHA256,
					WeightsSHA256:   res.WeightsSHA256,
					WeightsOffset:   res.WeightsOffset,
					ProbeSHA256:     res.ProbeSHA256,
					Size:            res.Size,
					Format:          res.Format,
					HeaderBlob:      res.Header.Blob,
					HeaderOffset:    res.Header.BlobOffset,
					HeaderTruncated: res.Header.Truncated,
				}); err != nil {
					errCount.Add(1)
					logf("recording %s: %v", p.Path, err)
					continue
				}

				if res.SHA256 == p.SHA256 {
					matched.Add(1)
				} else {
					mismatched.Add(1)
					if p.Provisional {
						probeMisbindings.Add(1)
					}
					mu.Lock()
					mismatches = append(mismatches, Mismatch{
						Path:        p.Path,
						RecordedSHA: p.SHA256,
						ActualSHA:   res.SHA256,
						WasProbe:    p.Provisional,
					})
					mu.Unlock()
				}

				// Rebind unconditionally: for a matching hash this only clears the
				// provisional flag and refreshes the instance facts, and for a
				// mismatch it corrects a binding that is now known to be wrong.
				if err := st.RebindPath(p.ID, res.SHA256, res.Size, res.MtimeNs); err != nil {
					errCount.Add(1)
					logf("rebinding %s: %v", p.Path, err)
					continue
				}
				if p.Provisional {
					confirmed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	return &Result{
		Checked:          checked.Load(),
		Matched:          matched.Load(),
		Mismatched:       mismatched.Load(),
		Missing:          missing.Load(),
		Errors:           errCount.Load(),
		Confirmed:        confirmed.Load(),
		ProbeMisbindings: probeMisbindings.Load(),
		Mismatches:       mismatches,
		Elapsed:          time.Since(started),
	}, nil
}

// Summary renders the result as human-readable lines.
func (r *Result) Summary() string {
	s := fmt.Sprintf(
		"checked %d  matched %d  mismatched %d  missing %d  errors %d  (%s)",
		r.Checked, r.Matched, r.Mismatched, r.Missing, r.Errors,
		r.Elapsed.Round(time.Millisecond))
	if r.Confirmed > 0 {
		s += fmt.Sprintf("\nconfirmed %d provisional path(s) by full hash", r.Confirmed)
	}
	if r.ProbeMisbindings > 0 {
		s += fmt.Sprintf("\n%d probe-bound path(s) were bound to the WRONG hash and have been corrected",
			r.ProbeMisbindings)
	}
	return s
}
