package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/socrasteeze/model-manager/internal/origin"
)

func cmdBrowse(ctx context.Context, args []string) error {
	fs := newFlagSet("browse", `mm browse [flags] [query]

Searches Civitai, CivArchive and HuggingFace for models to download.

Results are matched against the library by content hash, so each one is marked
have, update or new. That comparison is exact rather than a filename guess: a
file you renamed is still recognised, and a different file that happens to share
a name is not.

CivArchive mirrors records removed from Civitai, which is the only place the
metadata for a taken-down model can still be found.

Nothing a search returns is recorded against a local model. A listing is a claim
about a file that is not here yet; metadata is only attributed once the bytes
are downloaded and hashed.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	providers := fs.String("provider", "", "comma-separated providers (default: all)")
	modelType := fs.String("type", "", "comma-separated model types (lora, checkpoint, ...)")
	baseModel := fs.String("base-model", "", "comma-separated base models (SDXL, Pony, ...)")
	sortBy := fs.String("sort", "", "relevance | downloads | newest | updated | rating")
	limit := fs.Int("limit", 20, "results per provider")
	page := fs.Int("page", 1, "page number")
	nsfw := fs.Bool("nsfw", false, "include adult content")
	newOnly := fs.Bool("new-only", false, "hide anything already in the library")
	noResolve := fs.Bool("no-resolve", false, "skip the extra lookup that gets HuggingFace file hashes")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	apiKey := fs.String("api-key", "", "Civitai API key (default: CIVITAI_API_KEY)")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	client := origin.NewClient()
	if *apiKey != "" {
		client.APIKey = *apiKey
	}

	q := origin.Query{
		Text:       strings.Join(fs.Args(), " "),
		Types:      splitList(*modelType),
		BaseModels: splitList(*baseModel),
		NSFW:       *nsfw,
		Sort:       *sortBy,
		Page:       *page,
		Limit:      *limit,
	}

	registry := origin.NewRegistry(client)
	items, errs := registry.SearchAll(ctx, splitList(*providers), q)

	// A provider being down must not look like "no results from that provider".
	for id, err := range errs {
		fmt.Fprintf(os.Stderr, "mm: %s unavailable: %v\n", id, err)
	}

	// HuggingFace search returns filenames but no hashes, so without a second
	// call per listing every HuggingFace result would report as new even when
	// the file is already on disk. Costs a request each, hence the opt-out.
	if !*noResolve {
		registry.ResolveFiles(ctx, items, *limit)
	}

	// Annotating needs the library. A failure to open it is an error, not a
	// silent degradation: without the index everything prints as "new",
	// --new-only hides nothing, and the suggested-commands block invites
	// re-downloading models already owned. A user who genuinely has no
	// database yet points --db somewhere writable and gets an empty one.
	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return fmt.Errorf("browse: opening the library for have/update marking: %w", err)
	}
	defer st.Close()
	if idx, err := origin.BuildLocalIndex(st); err == nil {
		idx.Annotate(items)
	} else {
		fmt.Fprintf(os.Stderr, "mm: could not index the library: %v\n", err)
	}

	if *newOnly {
		items = filterListings(items, func(l origin.Listing) bool {
			return l.Local == nil || l.Local.Status == origin.MatchNew
		})
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	if len(items) == 0 {
		fmt.Println("no results")
		return nil
	}
	printListings(items)
	return nil
}

func printListings(items []origin.Listing) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tPROVIDER\tTYPE\tBASE\tSIZE\tNAME")

	for _, l := range items {
		status := "new"
		if l.Local != nil {
			switch l.Local.Status {
			case origin.MatchHave:
				status = "have"
			case origin.MatchOutdated:
				status = "update"
			}
		}

		var size int64
		if f := l.PrimaryFile(); f != nil {
			size = f.SizeBytes
		}
		sizeStr := "-"
		if size > 0 {
			sizeStr = humanBytes(size)
		}

		name := l.Name
		if l.VersionName != "" {
			name += " (" + l.VersionName + ")"
		}
		if l.NSFW {
			name += " [nsfw]"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			status, l.Provider, orDash(l.Type), orDash(l.BaseModel), sizeStr, name)
	}
	w.Flush()

	// The download URL is the actionable part, and a table column would either
	// truncate it or wreck the layout.
	fmt.Println()
	for _, l := range items {
		if l.Local != nil && l.Local.Status == origin.MatchHave {
			continue
		}
		if f := l.PrimaryFile(); f != nil && f.DownloadURL != "" {
			auth := ""
			if f.RequiresAuth {
				auth = "  (needs an API key)"
			}
			fmt.Printf("  mm get %s%s\n", f.DownloadURL, auth)
		}
	}
}

func cmdUpdates(ctx context.Context, args []string) error {
	fs := newFlagSet("updates", `mm updates

Reports which models in the library have a newer version published.

Answerable only because enrichment archived which remote model each local file
came from, so the check asks those models directly rather than searching. A
model that has since been removed upstream is not an error: the local copy may
be the last one in existence.

An update that retargets a different base model is flagged, because a LoRA
rebuilt from SD1.5 onto SDXL is published as a new version of the same model but
is not a drop-in replacement.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	limit := fs.Int("limit", 0, "check at most this many models (0 for all)")
	apiKey := fs.String("api-key", "", "Civitai API key (default: CIVITAI_API_KEY)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	stored := fs.Bool("stored", false, "report what the last check found, without asking the provider")
	maxAge := fs.Duration("max-age", 0, "skip models checked more recently than this")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	// The sweep records into the same tables the UI reads, rather than
	// computing an answer that lives only in this process. Otherwise running
	// this would warm nothing and the library's badge would stay empty --
	// exactly the two-sources-of-truth split this project avoids elsewhere.
	stats := &origin.UpdateStats{}
	if !*stored {
		client := origin.NewClient()
		if *apiKey != "" {
			client.APIKey = *apiKey
		}
		stats, err = origin.SweepUpdates(ctx, st, origin.SweepOptions{
			Client: client,
			Limit:  *limit,
			MaxAge: *maxAge,
			Logf:   func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
			Progress: func(done, total int, _ origin.UpdateStats) {
				if total > 0 {
					fmt.Fprintf(os.Stderr, "\r  %d/%d models   ", done, total)
				}
			},
		})
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
	}

	// Read back what is stored rather than what this run happened to see: a
	// model checked on an earlier run still needs reporting, and the view
	// already excludes updates whose file has since been downloaded.
	updates, err := st.PendingUpdates(0)
	if err != nil {
		return err
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Name < updates[j].Name })

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(updates); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, stats.Summary())
		return nil
	}

	if len(updates) == 0 {
		fmt.Println("everything is up to date")
		fmt.Fprintln(os.Stderr, stats.Summary())
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHAVE\tLATEST\tSIZE\tNOTE")
	for _, u := range updates {
		note := ""
		if u.BaseModelChanged {
			note = "base model changed to " + u.LatestBaseModel
		}
		size := "-"
		if u.SizeBytes > 0 {
			size = humanBytes(u.SizeBytes)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			u.Name, orDash(u.HaveVersionName), orDash(u.LatestVersionName), size, note)
	}
	w.Flush()

	fmt.Println()
	for _, u := range updates {
		if u.DownloadURL != "" {
			fmt.Printf("  mm get %s\n", u.DownloadURL)
		}
	}
	fmt.Fprintln(os.Stderr, stats.Summary())
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func filterListings(items []origin.Listing, keep func(origin.Listing) bool) []origin.Listing {
	// Non-nil so --json emits [] rather than null for an empty result; a
	// consumer iterating the array should not need a null guard for the
	// perfectly ordinary case of "no new models".
	out := []origin.Listing{}
	for _, l := range items {
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
