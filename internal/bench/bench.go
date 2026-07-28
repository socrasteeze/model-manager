// Package bench measures hashing throughput at different worker counts.
//
// Spec §17 asks for this explicitly, and asks for it before assuming parallelism
// wins: on spinning rust, concurrent readers seek-thrash and frequently lose to
// sequential large-buffer reads. The right worker count is a property of the
// array, not of the code, so it has to be measured on the array.
package bench

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/scan"
)

// Options configures a benchmark.
type Options struct {
	Roots []string

	// WorkerCounts are the concurrency levels to compare.
	WorkerCounts []int

	// MaxFiles caps how many files are sampled. Zero means no cap.
	MaxFiles int

	// MaxBytes caps the total sampled size. Zero means no cap.
	MaxBytes int64

	BufferSize int

	// Seed makes the sample reproducible across runs.
	Seed int64

	Logf func(format string, args ...any)
}

// Run is one worker count's result.
type Run struct {
	Workers     int
	Files       int
	Bytes       int64
	Elapsed     time.Duration
	BytesPerSec float64
	Errors      int64
}

// Result is the whole comparison.
type Result struct {
	SampleFiles int
	SampleBytes int64
	Runs        []Run
}

// Execute samples files under the roots and hashes them once per worker count.
//
// Nothing is written to any database: this measures the disk and the hasher, and
// mixing SQLite into the timing would measure the wrong thing.
func Execute(ctx context.Context, opts Options) (*Result, error) {
	if len(opts.WorkerCounts) == 0 {
		opts.WorkerCounts = []int{1, 2, 4}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	files, err := scan.ListFiles(ctx, opts.Roots)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("bench: no model files found under the given roots")
	}

	rng := rand.New(rand.NewSource(opts.Seed))
	rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })

	sample := files
	if opts.MaxFiles > 0 && len(sample) > opts.MaxFiles {
		sample = sample[:opts.MaxFiles]
	}
	if opts.MaxBytes > 0 {
		var total int64
		cut := len(sample)
		for i, f := range sample {
			total += f.Size
			if total >= opts.MaxBytes {
				cut = i + 1
				break
			}
		}
		sample = sample[:cut]
	}

	var sampleBytes int64
	for _, f := range sample {
		sampleBytes += f.Size
	}
	logf("sampling %d files, %s", len(sample), humanBytes(sampleBytes))

	result := &Result{SampleFiles: len(sample), SampleBytes: sampleBytes}

	counts := append([]int(nil), opts.WorkerCounts...)
	sort.Ints(counts)

	for _, workers := range counts {
		if ctx.Err() != nil {
			break
		}
		if workers < 1 {
			continue
		}
		logf("running %d worker(s)...", workers)
		run := hashSample(ctx, sample, workers, opts.BufferSize)
		result.Runs = append(result.Runs, run)
	}
	return result, nil
}

func hashSample(ctx context.Context, files []scan.FileRef, workers, bufferSize int) Run {
	work := make(chan scan.FileRef)
	var bytes, errs atomic.Int64
	var done atomic.Int64

	started := time.Now()

	go func() {
		defer close(work)
		for _, f := range files {
			select {
			case <-ctx.Done():
				return
			case work <- f:
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hashing.New(bufferSize, 0)
			for f := range work {
				if ctx.Err() != nil {
					return
				}
				res, err := h.Full(f.Path)
				if err != nil {
					errs.Add(1)
					continue
				}
				bytes.Add(res.BytesRead)
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(started)
	run := Run{
		Workers: workers,
		Files:   int(done.Load()),
		Bytes:   bytes.Load(),
		Elapsed: elapsed,
		Errors:  errs.Load(),
	}
	if elapsed > 0 {
		run.BytesPerSec = float64(run.Bytes) / elapsed.Seconds()
	}
	return run
}

// Text renders the comparison, including the caveat that decides whether the
// numbers mean anything.
func (r *Result) Text() string {
	var b fmtBuilder
	b.printf("Hashing concurrency benchmark\n")
	b.printf("sample: %d files, %s\n\n", r.SampleFiles, humanBytes(r.SampleBytes))
	b.printf("  WORKERS   THROUGHPUT      ELAPSED      FILES   ERRORS\n")

	var best Run
	for _, run := range r.Runs {
		b.printf("  %7d   %10s/s   %10s   %6d   %6d\n",
			run.Workers, humanBytes(int64(run.BytesPerSec)),
			run.Elapsed.Round(time.Millisecond), run.Files, run.Errors)
		if run.BytesPerSec > best.BytesPerSec {
			best = run
		}
	}

	if best.Workers > 0 {
		b.printf("\nFastest: %d worker(s) at %s/s.\n", best.Workers, humanBytes(int64(best.BytesPerSec)))
		b.printf("Use it with:  mm scan --workers %d\n", best.Workers)
	}

	// Without this caveat the numbers are worse than no numbers: a sample that
	// fits in RAM measures the page cache on every run after the first, which
	// makes higher worker counts look free.
	b.printf("\nCaveat: after the first run the sample may be served from the OS page\n" +
		"cache, which flatters every later run. For a result that reflects the\n" +
		"actual array, use a sample larger than RAM (--max-gib), or drop caches\n" +
		"between runs (Linux: sync; echo 3 > /proc/sys/vm/drop_caches).\n")
	b.printf("Worker counts apply per storage device, so measure each device that\n" +
		"holds models separately.\n")

	return b.String()
}

type fmtBuilder struct{ s []byte }

func (b *fmtBuilder) printf(format string, args ...any) {
	b.s = append(b.s, fmt.Sprintf(format, args...)...)
}

func (b *fmtBuilder) String() string { return string(b.s) }

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
