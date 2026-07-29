package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/project"
	"github.com/socrasteeze/model-manager/internal/tier"
)

func cmdTier(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("tier: expected one of: status, stage, unstage, pin, unpin")
	}

	fs := newFlagSet("tier", `mm tier <status|stage|unstage|pin|unpin> [SHA256 ...]

Stages hot models onto fast storage.

An SSD copy is just a second path on the same hash: verifiable, disposable, and
re-derivable at any time. The original is never touched, and unstaging removes
only the copy.

Size the cache from `+"`mm report`"+` — the distinct-model count and size
distribution are exactly the input that decides whether a working set is 150
models or 600.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	cacheDir := fs.String("cache", "", "fast-storage directory (required for staging)")
	capacityGiB := fs.Float64("capacity-gib", 0, "cache size limit in GiB (0 for unbounded)")
	noVerify := fs.Bool("no-verify", false, "skip re-hashing the staged copy (not recommended)")
	pin := fs.Bool("pin", false, "pin staged models so they are never evicted")

	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	dir := *cacheDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(st.Path()), "tier-cache")
	}
	mgr, err := tier.NewManager(st, dir, int64(*capacityGiB*float64(1<<30)))
	if err != nil {
		return err
	}

	switch sub {
	case "status":
		status, err := mgr.Status()
		if err != nil {
			return err
		}
		fmt.Println(status.Summary())
		for _, e := range status.Entries {
			marker := " "
			if e.Pinned {
				marker = "*"
			}
			name := e.Name
			if name == "" {
				name = filepath.Base(e.SourcePath)
			}
			fmt.Printf("  %s %s  %s\n", marker, name, humanBytes(e.Bytes))
		}
		return nil

	case "stage":
		if fs.NArg() == 0 {
			return errors.New("tier stage: which model? Pass one or more SHA256 values")
		}
		for _, sha := range fs.Args() {
			entry, err := mgr.Stage(ctx, sha, tier.StageOptions{
				Pin:    *pin,
				Verify: !*noVerify,
				Logf:   func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
				Progress: func(copied, total int64) {
					if total > 0 {
						fmt.Fprintf(os.Stderr, "\r  %5.1f%%   ", 100*float64(copied)/float64(total))
					}
				},
			})
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return err
			}
			fmt.Println(entry.CachePath)
		}
		return nil

	case "unstage":
		if fs.NArg() == 0 {
			return errors.New("tier unstage: which model?")
		}
		for _, sha := range fs.Args() {
			if err := mgr.Unstage(sha); err != nil {
				return err
			}
			fmt.Printf("unstaged %s\n", sha)
		}
		return nil

	case "pin", "unpin":
		if fs.NArg() == 0 {
			return fmt.Errorf("tier %s: which model?", sub)
		}
		for _, sha := range fs.Args() {
			if err := mgr.SetPinned(sha, sub == "pin"); err != nil {
				return err
			}
			fmt.Printf("%sned %s\n", sub, sha)
		}
		return nil
	}
	return fmt.Errorf("tier: unknown subcommand %q", sub)
}

func cmdProject(ctx context.Context, args []string) error {
	fs := newFlagSet("project", `mm project --target TOOL

Writes master metadata back out as tool sidecars.

Sidecars become derived artifacts: if a tool mangles one, regenerate it. Their
"pull metadata" button stops being a threat and becomes a no-op overwritten on
the next projection.

Start with one tool and verify the result before adding others -- there is
deliberately no "everything" default. A sidecar this app did not write is left
alone unless you pass --overwrite, because it may hold something master never
captured.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	blobDir := fs.String("blobs", "", "preview blob directory (default: alongside the database)")
	dryRun := fs.Bool("dry-run", false, "report what would be written without touching the disk")
	overwrite := fs.Bool("overwrite", false, "replace sidecars this app did not write")
	previews := fs.Bool("previews", false, "also write a preview image beside each model")
	onlySHA := fs.String("sha256", "", "project a single model, to verify a dialect first")
	var targets rootList
	fs.Var(&targets, "target", "stability-matrix | a1111 | swarmui | lora-manager (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf(
			"project: --target is required.\nAvailable: %s\nStart with one and verify it before adding others",
			strings.Join(targetNames(), ", "))
	}

	var list []project.Target
	for _, t := range targets {
		list = append(list, project.Target(t))
	}
	if conflict := project.ConflictingTargets(list); conflict != nil {
		return errors.New(
			"project: swarmui and a1111 both write <model>.json, so writing both would " +
				"have the second silently overwrite the first. Pick one")
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	opts := project.Options{
		Targets:       list,
		DryRun:        *dryRun,
		Overwrite:     *overwrite,
		WritePreviews: *previews,
		OnlySHA:       *onlySHA,
		Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	if *previews {
		blobs, err := blobstore.New(orDefault(*blobDir, defaultBlobDir(st.Path())))
		if err != nil {
			return err
		}
		opts.Blobs = blobs
	}

	stats, err := project.Run(ctx, st, opts)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println("(dry run — nothing was written)")
	}
	fmt.Println(stats.Summary())
	return nil
}

func targetNames() []string {
	out := make([]string, len(project.AllTargets))
	for i, t := range project.AllTargets {
		out[i] = string(t)
	}
	return out
}
