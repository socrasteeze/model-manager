package ingest

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Options configures an ingest pass.
type Options struct {
	// Blobs receives preview images. Optional -- without it, previews are
	// skipped and everything else still ingests.
	Blobs *blobstore.Store

	// SkipPreviews ignores images even when a blob store is available, for a
	// first pass where the metadata matters and tens of gigabytes of thumbnails
	// do not.
	SkipPreviews bool

	Logf func(format string, args ...any)
}

// Stats summarizes a pass.
type Stats struct {
	PathsExamined int
	SidecarsFound int
	ModelsUpdated int
	Previews      int
	BySource      map[string]int
	Errors        int
	Elapsed       time.Duration
}

// Run reads every tool sidecar beside every known model path and merges what it
// finds into master.
//
// Only present paths are examined. A sidecar beside a path that no longer exists
// describes a file that is gone, and ingesting it would resurrect claims about
// something the index has already established is not there.
func Run(ctx context.Context, st *store.Store, opts Options) (*Stats, error) {
	started := time.Now()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	stats := &Stats{BySource: map[string]int{}}

	paths, err := st.PresentPaths()
	if err != nil {
		return nil, err
	}

	// Several paths can share one hash. Ingesting each of them is correct --
	// different copies may carry different sidecars, and each is a separate
	// observation -- but the resolve is done once per model at the end.
	touched := map[string]bool{}

	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		stats.PathsExamined++

		for _, parsed := range Discover(p.Path) {
			if len(parsed.Observations) == 0 && len(parsed.Tags) == 0 &&
				len(parsed.PreviewData) == 0 && len(parsed.PreviewPaths) == 0 {
				continue
			}
			stats.SidecarsFound++
			stats.BySource[parsed.Source]++

			if len(parsed.Observations) > 0 {
				if err := st.RecordObservations(p.SHA256, parsed.Source, parsed.Observations); err != nil {
					stats.Errors++
					logf("recording %s from %s: %v", p.Path, parsed.Source, err)
					continue
				}
			}
			if len(parsed.Tags) > 0 {
				if err := st.SetTags(p.SHA256, parsed.Source, parsed.Tags); err != nil {
					stats.Errors++
					logf("tagging %s from %s: %v", p.Path, parsed.Source, err)
				}
			}
			if parsed.Training != nil {
				parsed.Training.SHA256 = p.SHA256
				if err := st.UpsertTrainingRecord(*parsed.Training); err != nil {
					stats.Errors++
					logf("training record for %s: %v", p.Path, err)
				}
			}

			if opts.Blobs != nil && !opts.SkipPreviews {
				n, err := storePreviews(st, opts.Blobs, p.SHA256, parsed)
				if err != nil {
					stats.Errors++
					logf("previews for %s: %v", p.Path, err)
				}
				stats.Previews += n
			}

			touched[p.SHA256] = true
		}
	}

	for sha := range touched {
		if ctx.Err() != nil {
			break
		}
		if _, err := st.ResolveModel(sha); err != nil {
			return stats, err
		}
		stats.ModelsUpdated++
	}

	stats.Elapsed = time.Since(started)
	return stats, nil
}

func storePreviews(st *store.Store, blobs *blobstore.Store, sha string, parsed Parsed) (int, error) {
	stored := 0

	admit := func(data []byte, position int) error {
		// Sniff before storing. A "preview" that is not an image is either a
		// mistake or a file that would be dangerous to serve back as one.
		if !blobstore.IsImage(data) {
			return nil
		}
		blob, err := blobs.Put(data)
		if err != nil {
			return err
		}
		if err := st.AddPreviewImage(store.PreviewImage{
			SHA256:      sha,
			ImageSHA256: blob.SHA256,
			MIME:        blob.MIME,
			Bytes:       blob.Bytes,
			Source:      parsed.Source,
			Position:    position,
		}); err != nil {
			return err
		}
		stored++
		return nil
	}

	for i, data := range parsed.PreviewData {
		if err := admit(data, i); err != nil {
			return stored, err
		}
	}
	for i, path := range parsed.PreviewPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // a preview that vanished mid-pass is not worth failing over
		}
		if int64(len(data)) > blobstore.MaxBlobBytes {
			continue
		}
		if err := admit(data, len(parsed.PreviewData)+i); err != nil {
			return stored, err
		}
	}
	return stored, nil
}

// Summary renders the stats for the CLI.
func (s *Stats) Summary() string {
	out := fmt.Sprintf("examined %d path(s), found %d sidecar(s), updated %d model(s)",
		s.PathsExamined, s.SidecarsFound, s.ModelsUpdated)
	if s.Previews > 0 {
		out += fmt.Sprintf(", stored %d preview image(s)", s.Previews)
	}
	out += fmt.Sprintf(" (%s)", s.Elapsed.Round(time.Millisecond))

	if len(s.BySource) > 0 {
		out += "\nby source:"
		for _, src := range sortedKeys(s.BySource) {
			out += fmt.Sprintf("\n  %-22s %d", src, s.BySource[src])
		}
	}
	if s.Errors > 0 {
		out += fmt.Sprintf("\n%d error(s)", s.Errors)
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
