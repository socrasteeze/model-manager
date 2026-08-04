package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/origin"
)

func cmdEnrich(ctx context.Context, args []string) error {
	fs := newFlagSet("enrich", `mm enrich

Looks models up on Civitai by SHA256 and merges what comes back.

The hash that identifies a file locally is the same key Civitai indexes by, so
this is an exact lookup rather than a name match -- nothing can bind the wrong
record.

Every response is archived in full and never expired. Models are removed from
Civitai regularly, and once gone the metadata is unrecoverable anywhere; for a
taken-down model this local copy may be the only one left.

Throttled and resumable. A large library against a rate-limited public API takes
a while, so stop it whenever you like and re-run to continue.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	blobDir := fs.String("blobs", "", "preview blob directory (default: alongside the database)")
	limit := fs.Int("limit", 0, "stop after this many models (0 for all)")
	apiKey := fs.String("api-key", os.Getenv("CIVITAI_API_KEY"), "Civitai API key (or set CIVITAI_API_KEY)")
	refresh := fs.Bool("refresh", false, "re-query models that already have a cached answer")
	skipImages := fs.Bool("no-images", false, "do not download preview images")
	maxImages := fs.Int("max-images", 4, "preview images to keep per model")
	intervalMs := fs.Int("interval-ms", 350, "minimum milliseconds between requests")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	client := origin.NewClient()
	client.APIKey = *apiKey
	client.MinInterval = time.Duration(*intervalMs) * time.Millisecond

	opts := origin.EnrichOptions{
		Client:     client,
		Limit:      *limit,
		Refresh:    *refresh,
		SkipImages: *skipImages,
		MaxImages:  *maxImages,
		Logf:       func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		Progress: func(done, total int, _ origin.EnrichStats) {
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r  %d/%d models   ", done, total)
			}
		},
	}
	if !*skipImages {
		blobs, err := blobstore.New(orDefault(*blobDir, defaultBlobDir(st.Path())))
		if err != nil {
			return err
		}
		opts.Blobs = blobs
	}

	stats, err := origin.Enrich(ctx, st, opts)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Println(stats.Summary())

	archive, err := origin.NewCache(st).Stats()
	if err == nil {
		fmt.Printf("archive: %d response(s) preserved, %s\n",
			archive.Positive, humanBytes(int64(archive.Bytes)))
	}
	return nil
}

func cmdGet(ctx context.Context, args []string) error {
	fs := newFlagSet("get", `mm get URL [URL ...]

Downloads a model and places it in a directory you choose.

Resumable: an interrupted multi-gigabyte transfer picks up where it stopped.
Partial files are quarantined outside the destination, because a half-written
model sitting in a tool's models folder is one that tool will happily load.

When the expected hash is known, it is verified before the file is admitted.
Nothing is ever overwritten.`)

	dest := fs.String("dest", "", "destination directory (required)")
	apiKey := fs.String("api-key", os.Getenv("CIVITAI_API_KEY"), "Civitai API key (or set CIVITAI_API_KEY)")
	expect := fs.String("sha256", "", "expected SHA256; the file is quarantined if it does not match")
	workDir := fs.String("work-dir", "", "where partial files live (default: alongside the database)")
	dbPath := fs.String("db", defaultDBPath(), "database, used only to pick a default work directory")

	if err := fs.Parse(args); err != nil {
		return err
	}
	urls := fs.Args()
	if len(urls) == 0 {
		return errors.New("get: at least one URL is required")
	}
	if *dest == "" {
		return errors.New("get: --dest is required — this tool never guesses where to put a model")
	}
	if *expect != "" && len(urls) > 1 {
		return errors.New("get: --sha256 applies to a single URL")
	}

	work := *workDir
	if work == "" {
		work = filepath.Join(filepath.Dir(*dbPath), "downloads")
	}
	mgr, err := download.NewManager(work)
	if err != nil {
		return err
	}
	// Host-scoped credentials, not one key for every request: a Civitai key
	// sent to huggingface.co is a leaked credential. Building an origin client
	// also picks up HF_TOKEN from the environment, so mm get can fetch gated
	// HuggingFace repos — something a single-key design could never do right.
	tokens := origin.NewClient()
	if *apiKey != "" {
		tokens.APIKey = *apiKey
	}
	mgr.TokenFor = tokens.TokenFor

	var failed int
	for _, url := range urls {
		fmt.Fprintf(os.Stderr, "fetching %s\n", url)

		done := make(chan struct{})
		job := download.Job{URL: url, DestDir: *dest, ExpectedSHA256: *expect}
		go reportProgress(mgr, download.Job{URL: url, DestDir: *dest}, done)

		result, err := mgr.Fetch(ctx, job)
		close(done)
		fmt.Fprintln(os.Stderr)

		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  %s: %v\n", result.State, err)
			if result.State == download.StateQuarantined {
				fmt.Fprintf(os.Stderr,
					"  the partial file is kept for inspection under %s\n", mgr.WorkDir)
			}
			continue
		}
		fmt.Printf("%s\n  %s\n", result.FinalPath, result.ActualSHA)
		if *expect == "" {
			fmt.Fprintln(os.Stderr,
				"  note: no expected hash was given, so this file was accepted on arrival.\n"+
					"        Run `mm scan` on the destination to index and verify it.")
		}
	}

	if failed > 0 {
		return fmt.Errorf("get: %d of %d download(s) failed", failed, len(urls))
	}
	return nil
}

func reportProgress(mgr *download.Manager, probe download.Job, done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			for _, j := range mgr.Jobs() {
				if j.URL != probe.URL || j.State != download.StateDownloading {
					continue
				}
				if j.Total > 0 {
					fmt.Fprintf(os.Stderr, "\r  %5.1f%%  %s / %s   ",
						j.Progress(), humanBytes(j.Downloaded), humanBytes(j.Total))
				} else {
					fmt.Fprintf(os.Stderr, "\r  %s   ", humanBytes(j.Downloaded))
				}
			}
		}
	}
}
