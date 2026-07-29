// Command mm is the Model Manager CLI.
//
// Phase 0 exposes the scanner and the reports built on it. There is no daemon,
// no UI and no API yet, and nothing here writes anywhere outside its own
// database (spec §14).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/socrasteeze/model-manager/internal/bench"
	"github.com/socrasteeze/model-manager/internal/interpret"
	"github.com/socrasteeze/model-manager/internal/report"
	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/verify"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `mm — Model Manager (Phase 0)

Metadata binds to file content, not to path. This command walks model roots,
hashes what it finds, and records raw uninterpreted facts in a single SQLite
file. It never modifies, moves, renames, or deletes a model file.

USAGE
    mm <command> [flags]

COMMANDS
    scan       Walk model roots and record what is there
    interpret  Turn stored headers into typed metadata (reads no model files)
    report     Summarize the index: distinct models, duplication, size spread
    verify     Re-read files and check the index against the disk
    bench      Compare hashing throughput at different worker counts
    version    Print the version

Run "mm <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signalContext()
	defer stop()

	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(ctx, os.Args[2:])
	case "interpret":
		err = cmdInterpret(ctx, os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "bench":
		err = cmdBench(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("mm %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "mm: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "mm: %v\n", err)
		os.Exit(1)
	}
}

// signalContext cancels on the first interrupt so a scan can close out its run
// rows cleanly, and hard-exits on the second so a wedged process is still
// killable without escalating to SIGKILL by hand.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-ch
		fmt.Fprintln(os.Stderr,
			"\nmm: interrupt received — finishing in-flight files and closing out cleanly "+
				"(interrupt again to force quit)")
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "mm: forced quit")
		os.Exit(130)
	}()

	return ctx, func() { signal.Stop(ch); cancel() }
}

// --- scan --------------------------------------------------------------------

type rootList []string

func (r *rootList) String() string     { return strings.Join(*r, ", ") }
func (r *rootList) Set(v string) error { *r = append(*r, v); return nil }

func cmdScan(ctx context.Context, args []string) error {
	fs := newFlagSet("scan", `mm scan --root DIR [--root DIR ...]

Walks each root, hashes every model file, and records paths, sizes, dev/inode,
both hashes, and the format header captured verbatim.

Commits per file, so an interrupted scan resumes rather than restarting. A
rescan of an unchanged tree costs a stat pass, not a hash pass.`)

	var roots rootList
	fs.Var(&roots, "root", "model root to scan (repeatable)")
	dbPath := fs.String("db", defaultDBPath(), "path to the master database (must be on a local disk)")
	workers := fs.Int("workers", 1, "hashing workers per storage device — measure with `mm bench` before raising")
	probe := fs.Bool("probe", false, "allow sampled-probe binding instead of a full read (binds paths provisionally)")
	bufferMiB := fs.Int("buffer-mib", 4, "read buffer size per worker, in MiB")
	maxHeaderMiB := fs.Int("max-header-mib", 8, "cap on stored header blobs, in MiB")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem (unsafe: SQLite locking is broken there)")
	quiet := fs.Bool("quiet", false, "suppress progress output")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 {
		return errors.New("scan: at least one --root is required")
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	logf := func(format string, a ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}
	logf("database: %s", st.Path())

	opts := scan.Options{
		Roots:            roots,
		WorkersPerDevice: *workers,
		BufferSize:       *bufferMiB << 20,
		MaxHeaderBytes:   *maxHeaderMiB << 20,
		UseProbe:         *probe,
		Logf:             logf,
	}
	if !*quiet {
		opts.Progress = func(s scan.Snapshot) { fmt.Fprintf(os.Stderr, "\r%s", progressLine(s)) }
	}

	res, err := scan.Run(ctx, st, opts)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Fprintln(os.Stderr)
	}

	for _, r := range res.Roots {
		fmt.Printf("%s: %s — %d seen, %d hashed, %d cached, %d probed, %d errors",
			r.Root, r.Status, r.Counters.FilesSeen, r.Counters.FilesHashed,
			r.Counters.FilesCached, r.Counters.FilesProbed, r.Counters.Errors)
		if r.SweptAbsent > 0 {
			fmt.Printf(", %d path(s) no longer present", r.SweptAbsent)
		}
		fmt.Println()
	}
	fmt.Printf("elapsed %s\n", res.Elapsed.Round(time.Second))

	if res.Cancelled {
		fmt.Println("\nScan was interrupted. Nothing was swept, and everything already " +
			"committed is kept — rerun the same command to continue.")
		return nil
	}
	fmt.Printf("\nNext:  mm report --db %s\n", st.Path())
	return nil
}

func progressLine(s scan.Snapshot) string {
	pct := 0.0
	if s.FilesTotal > 0 {
		pct = 100 * float64(s.FilesDone) / float64(s.FilesTotal)
	}
	rate := ""
	if s.Elapsed > time.Second && s.BytesDone > 0 {
		bps := float64(s.BytesDone) / s.Elapsed.Seconds()
		rate = fmt.Sprintf("  %s/s", humanBytes(int64(bps)))
		if s.BytesTotal > s.BytesDone && bps > 0 {
			eta := time.Duration(float64(s.BytesTotal-s.BytesDone)/bps) * time.Second
			rate += fmt.Sprintf("  eta %s", eta.Round(time.Second))
		}
	}
	errs := ""
	if s.Errors > 0 {
		errs = fmt.Sprintf("  %d errors", s.Errors)
	}
	return fmt.Sprintf("  %5.1f%%  %d/%d files  %s read, %d cached%s%s   ",
		pct, s.FilesDone, s.FilesTotal, humanBytes(s.BytesDone), s.FilesCached, rate, errs)
}

// --- interpret ---------------------------------------------------------------

func cmdInterpret(ctx context.Context, args []string) error {
	fs := newFlagSet("interpret", `mm interpret

Turns the format headers captured during the scan into typed metadata: model
type, base model, name, trigger words, and the training record for self-trained
LoRAs.

Reads no model files. Phase 0 stored the headers verbatim precisely so this pass
costs a database scan rather than another walk over terabytes, which is what
makes it safe to re-run whenever the interpretation rules improve.

Manual values are never touched.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	skipPaths := fs.Bool("no-path-heuristics", false,
		"ignore filenames and directory names as a metadata source")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := interpret.Run(ctx, st, interpret.Options{
		SkipPathHeuristics: *skipPaths,
		Logf:               func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Println(stats.Summary())
	return nil
}

// --- report ------------------------------------------------------------------

func cmdReport(args []string) error {
	fs := newFlagSet("report", `mm report

Summarizes the index. The headline numbers are the ones Phase 0 exists to
produce: how many distinct models there are, how much of the library is
duplication, and how the sizes are distributed.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	top := fs.Int("top", 20, "how many duplicate groups to list")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	rep, err := report.Generate(st.DB(), st.Path(), *top)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return rep.Text(os.Stdout)
}

// --- verify ------------------------------------------------------------------

func cmdVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("verify", `mm verify

Re-reads files and compares them against the index. Use it to prove the index
before anything downstream acts on it, and to confirm paths that were bound by
sampled probe rather than by a full read.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	sample := fs.Int("sample", 25, "how many paths to check (0 for all of them)")
	provisional := fs.Bool("provisional", false, "check only probe-bound paths, and confirm them")
	workers := fs.Int("workers", 1, "hashing workers")
	bufferMiB := fs.Int("buffer-mib", 4, "read buffer size per worker, in MiB")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	// Confirming provisional paths means reading all of them. Sampling would
	// leave the remainder unusable for projection, dedup and tiering, which is
	// the whole reason to run this.
	effectiveSample := *sample
	if *provisional {
		effectiveSample = 0
	}

	res, err := verify.Run(ctx, st, verify.Options{
		Sample:          effectiveSample,
		ProvisionalOnly: *provisional,
		Workers:         *workers,
		BufferSize:      *bufferMiB << 20,
		Logf:            func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}

	fmt.Println(res.Summary())

	if len(res.Mismatches) > 0 {
		fmt.Println("\nMISMATCHES (the index has been corrected to match the disk):")
		for _, m := range res.Mismatches {
			source := "recorded by full hash"
			if m.WasProbe {
				source = "bound by sampled probe"
			}
			fmt.Printf("  %s\n    was %s (%s)\n    now %s\n", m.Path, m.RecordedSHA, source, m.ActualSHA)
		}
		fmt.Println("\nA mismatch on a path that had been hashed in full means the file changed\n" +
			"on disk since the scan — a tool rewrote it, or the bytes rotted. Compare\n" +
			"weights_sha256 for the same file: if that is unchanged, only the header\n" +
			"was rewritten and the weights themselves are intact.")
	}
	if res.Mismatched > 0 {
		os.Exit(1)
	}
	return nil
}

// --- bench -------------------------------------------------------------------

func cmdBench(ctx context.Context, args []string) error {
	fs := newFlagSet("bench", `mm bench --root DIR

Compares hashing throughput at several worker counts against real files, so the
scan's concurrency is measured on the array rather than assumed. Writes nothing
to any database.`)

	var roots rootList
	fs.Var(&roots, "root", "model root to sample from (repeatable)")
	workerSpec := fs.String("workers", "1,2,4", "comma-separated worker counts to compare")
	maxFiles := fs.Int("max-files", 200, "cap on sampled files (0 for no cap)")
	maxGiB := fs.Float64("max-gib", 8, "cap on sampled bytes, in GiB (0 for no cap)")
	bufferMiB := fs.Int("buffer-mib", 4, "read buffer size per worker, in MiB")
	seed := fs.Int64("seed", 1, "sample seed, so runs are comparable")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(roots) == 0 {
		return errors.New("bench: at least one --root is required")
	}

	counts, err := parseWorkerCounts(*workerSpec)
	if err != nil {
		return err
	}

	res, err := bench.Execute(ctx, bench.Options{
		Roots:        roots,
		WorkerCounts: counts,
		MaxFiles:     *maxFiles,
		MaxBytes:     int64(*maxGiB * float64(1<<30)),
		BufferSize:   *bufferMiB << 20,
		Seed:         *seed,
		Logf:         func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Print(res.Text())
	return nil
}

func parseWorkerCounts(spec string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bench: %q is not a worker count", part)
		}
		if n < 1 {
			return nil, fmt.Errorf("bench: worker count %d is not positive", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("bench: no worker counts given")
	}
	return out, nil
}

// --- shared ------------------------------------------------------------------

func openStore(path string, allowNetwork bool) (*store.Store, error) {
	st, err := store.Open(path, store.Options{
		AllowNetworkPath: allowNetwork,
		Warnf:            func(f string, a ...any) { fmt.Fprintf(os.Stderr, "mm: warning: "+f+"\n", a...) },
	})

	// The refusal is the point (spec §10.5), but someone who hits it needs to be
	// told what to do rather than only what was refused -- especially since the
	// natural reading is that the whole tool cannot work off a NAS, which is
	// wrong.
	var netErr *store.ErrNetworkMount
	if errors.As(err, &netErr) {
		return nil, fmt.Errorf("%w\n\n"+
			"Only the database has to be local — the model files themselves can stay\n"+
			"on the network share. Try:  --db %s", err, defaultDBPath())
	}
	return st, err
}

// defaultDBPath puts the database under the user's config directory, which is on
// a local disk on every platform this targets.
func defaultDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "model-manager.db"
	}
	return filepath.Join(dir, "model-manager", "master.db")
}

func newFlagSet(name, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n\nFLAGS\n", help)
		fs.PrintDefaults()
	}
	return fs
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
