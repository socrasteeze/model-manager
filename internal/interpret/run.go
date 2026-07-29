package interpret

import (
	"context"
	"fmt"
	"time"

	"github.com/socrasteeze/model-manager/internal/modelformat"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// Options configures an interpretation pass.
type Options struct {
	// SkipPathHeuristics disables the filename/directory source entirely, for a
	// user whose layout is meaningless and who would rather have empty fields
	// than confident-looking guesses.
	SkipPathHeuristics bool

	Logf func(format string, args ...any)
}

// Stats summarizes a pass.
type Stats struct {
	Models          int
	Interpreted     int
	TrainingRecords int
	Warnings        int
	Elapsed         time.Duration
}

// Run interprets every stored header and materializes the results.
//
// This reads no model files. Phase 0 captured the headers verbatim precisely so
// that this pass costs a database scan rather than another walk over terabytes,
// which is what makes it safe to re-run whenever the interpretation rules
// improve (spec §15).
func Run(ctx context.Context, st *store.Store, opts Options) (*Stats, error) {
	started := time.Now()
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	stats := &Stats{}

	type pending struct {
		sha      string
		bySource map[string][]store.FieldObservation
		training *store.TrainingRecord
	}
	var work []pending

	// The iteration holds a read cursor, so writes are collected and applied
	// afterwards rather than interleaved with it.
	n, err := st.IterHeaders(func(row store.HeaderRow) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		p := pending{sha: row.SHA256, bySource: map[string][]store.FieldObservation{}}

		var headerRes Result
		var headerSource string
		switch row.Format {
		case modelformat.Safetensors:
			headerRes = Safetensors(row.Blob, row.Truncated)
			headerSource = provenance.SourceSafetensorsHeader
		case modelformat.GGUF:
			headerRes = GGUF(row.Blob, row.Truncated)
			headerSource = provenance.SourceGGUFHeader
		default:
			// .ckpt and .pt have no parseable header by design (§10.4). They are
			// not skipped -- the path heuristics below are the only metadata
			// they will ever have before enrichment.
		}

		if len(headerRes.Observations) > 0 {
			p.bySource[headerSource] = headerRes.Observations
		}
		if headerRes.Training != nil {
			headerRes.Training.SHA256 = row.SHA256
			p.training = headerRes.Training
		}
		for _, w := range headerRes.Warnings {
			stats.Warnings++
			logf("%s: %s", shortSHA(row.SHA256), w)
		}

		if !opts.SkipPathHeuristics && row.AnyPath != "" {
			pathRes := FromPath(row.AnyPath)
			if v := VersionFromFilename(row.AnyPath); v != "" {
				pathRes.add(provenance.FieldVersion, v)
			}
			if len(pathRes.Observations) > 0 {
				p.bySource[provenance.SourcePathHeuristic] = pathRes.Observations
			}
		}

		if len(p.bySource) > 0 || p.training != nil {
			work = append(work, p)
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	stats.Models = n

	for _, p := range work {
		if ctx.Err() != nil {
			break
		}
		// Replace rather than merge. These are our own derived sources, computed
		// from complete input every run, so a field this pass no longer produces
		// is a stale artifact of an older rule rather than a surviving opinion.
		// Merging would mean an interpretation bug could never be fully fixed.
		for source, obs := range p.bySource {
			if err := st.ReplaceObservations(p.sha, source, obs); err != nil {
				return stats, err
			}
		}
		if p.training != nil {
			if err := st.UpsertTrainingRecord(*p.training); err != nil {
				return stats, err
			}
			stats.TrainingRecords++
		}
		if _, err := st.ResolveModel(p.sha); err != nil {
			return stats, err
		}
		stats.Interpreted++
	}

	stats.Elapsed = time.Since(started)
	return stats, nil
}

// Summary renders the stats for the CLI.
func (s *Stats) Summary() string {
	out := fmt.Sprintf("interpreted %d of %d model(s), %d training record(s) (%s)",
		s.Interpreted, s.Models, s.TrainingRecords, s.Elapsed.Round(time.Millisecond))
	if s.Warnings > 0 {
		out += fmt.Sprintf("\n%d header(s) could not be fully interpreted", s.Warnings)
	}
	return out
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
