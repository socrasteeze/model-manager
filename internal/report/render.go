package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Text renders a human-readable report.
func (r *Report) Text(w io.Writer) error {
	bw := &errWriter{w: w}

	bw.printf("Model Manager — Phase 0 report\n")
	bw.printf("database: %s\n", r.Database)
	bw.printf("generated: %s\n\n", r.GeneratedAt.Format("2006-01-02 15:04:05 UTC"))

	if len(r.Runs) == 0 {
		bw.printf("No scans have been run yet. Start with:\n\n    mm scan --db %s --root /path/to/models\n", r.Database)
		return bw.err
	}

	bw.printf("SCAN RUNS (latest per root)\n")
	tw := tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  ROOT\tSTATUS\tSEEN\tHASHED\tCACHED\tPROBED\tERRORS\tFINISHED\n")
	for _, run := range r.Runs {
		finished := run.FinishedAt
		if finished == "" {
			finished = "—"
		} else if len(finished) > 19 {
			finished = finished[:19]
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
			run.Root, run.Status, run.FilesSeen, run.FilesHashed,
			run.FilesCached, run.FilesProbed, run.Errors, finished)
	}
	tw.Flush()

	for _, run := range r.Runs {
		if run.Status != "completed" {
			bw.printf("\n  ! %s did not complete (%s). Its numbers below are partial,\n"+
				"    and no absent-path sweep was applied to it.\n", run.Root, run.Status)
		}
	}

	t := r.Totals
	bw.printf("\nLIBRARY\n")
	bw.printf("  distinct models      %s\n", comma(t.DistinctModels))
	bw.printf("  files on disk        %s   (distinct device+inode; hardlinks counted once)\n", comma(t.FileInstances))
	bw.printf("  indexed paths        %s\n", comma(t.PathsPresent))
	bw.printf("\n")
	bw.printf("  size on disk         %s\n", humanBytes(t.BytesOnDisk))
	bw.printf("  size if deduped      %s\n", humanBytes(t.BytesDistinct))
	bw.printf("  duplication          %s  (%s)\n", humanBytes(t.BytesDuplicated), percent(t.BytesDuplicated, t.BytesOnDisk))

	if t.DistinctModels > 0 {
		ratio := float64(t.FileInstances) / float64(t.DistinctModels)
		bw.printf("  copies per model     %.2f\n", ratio)
	}

	// Spec §9.4: a reflink reports its full size to any naive scan, so a
	// duplicate report that is not shared-extent aware will loudly report every
	// intentional view as wasted space. Phase 0 has no FIEMAP, so the honest move
	// is to say so rather than to present the number as settled.
	if t.BytesDuplicated > 0 {
		bw.printf("\n  Note: duplication is computed from apparent file sizes. Reflinked or\n" +
			"  block-cloned copies share extents on disk but report full size here, so\n" +
			"  this figure is an upper bound on what deduplication would actually\n" +
			"  reclaim. Shared-extent detection arrives with the presentation layer.\n")
	}

	if len(r.Formats) > 0 {
		bw.printf("\nBY FORMAT\n")
		tw = tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  FORMAT\tMODELS\tSIZE\tNO REBINDING KEY\n")
		for _, f := range r.Formats {
			note := ""
			switch {
			case f.NoWeightsHash == 0:
				note = "—"
			case f.Format == "ckpt" || f.Format == "pt":
				note = fmt.Sprintf("%s (expected: pickle is never parsed)", comma(f.NoWeightsHash))
			default:
				note = fmt.Sprintf("%s (framing did not parse)", comma(f.NoWeightsHash))
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", f.Format, comma(f.DistinctModels), humanBytes(f.Bytes), note)
		}
		tw.Flush()
	}

	bw.printf("\nSIZE DISTRIBUTION (distinct models — the input for sizing an SSD tier)\n")
	tw = tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  BUCKET\tMODELS\tSIZE\n")
	for _, b := range r.Sizes {
		if b.DistinctModels == 0 {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", b.Label, comma(b.DistinctModels), humanBytes(b.Bytes))
	}
	tw.Flush()

	if len(r.Duplicates) > 0 {
		bw.printf("\nTOP DUPLICATES (report only — this tool never deletes)\n")
		tw = tabwriter.NewWriter(bw, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  WASTED\tCOPIES\tEACH\tEXAMPLE PATH\n")
		for _, d := range r.Duplicates {
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\n",
				humanBytes(d.WastedBytes), d.Instances, humanBytes(d.Size), d.ExamplePath)
		}
		tw.Flush()
	}

	bw.printf("\nHEALTH\n")
	h := r.Health
	bw.printf("  provisional paths    %s\n", comma(h.ProvisionalPaths))
	if h.ProvisionalPaths > 0 {
		bw.printf("      bound by sampled probe, not confirmed by a full hash. Not usable for\n" +
			"      projection, dedup, or tiering until confirmed:  mm verify --provisional\n")
	}
	bw.printf("  absent paths         %s   (seen before, not present in the latest scan)\n", comma(h.AbsentPaths))
	bw.printf("  truncated headers    %s\n", comma(h.TruncatedHeaders))
	bw.printf("  framing failures     %s   (safetensors/GGUF with no rebinding key)\n", comma(h.FramingFailures))

	if len(h.ErrorsByKind) > 0 {
		kinds := make([]string, 0, len(h.ErrorsByKind))
		for k := range h.ErrorsByKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)

		bw.printf("  errors in latest scans:\n")
		for _, k := range kinds {
			bw.printf("      %-8s %s%s\n", k, comma(h.ErrorsByKind[k]), errorKindNote(k))
		}
	} else {
		bw.printf("  errors               none\n")
	}

	return bw.err
}

func errorKindNote(kind string) string {
	switch kind {
	case "race":
		return "  (file changed mid-hash — expected during a migration; rescan to pick up)"
	case "stat":
		return "  (could not stat or identify the file)"
	case "open", "read":
		return "  (could not read the file — check permissions)"
	case "header":
		return "  (weights offset found, header blob not captured)"
	default:
		return ""
	}
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return len(p), nil
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" + humanBytes(-n)
	}
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

func percent(part, whole int64) string {
	if whole == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(whole))
}

func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
