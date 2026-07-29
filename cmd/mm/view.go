package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/socrasteeze/model-manager/internal/link"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/view"
)

func cmdView(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("view: expected one of: list, create, generate, delete")
	}
	switch args[0] {
	case "list":
		return viewList(args[1:])
	case "create":
		return viewCreate(args[1:])
	case "generate":
		return viewGenerate(ctx, args[1:])
	case "delete":
		return viewDelete(args[1:])
	default:
		return fmt.Errorf("view: unknown subcommand %q", args[0])
	}
}

func viewList(args []string) error {
	fs := newFlagSet("view list", "mm view list\n\nLists defined views.")
	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	views, err := view.NewManager(st).List()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(views)
	}
	if len(views) == 0 {
		fmt.Print("No views defined.\n\nCreate one:\n")
		fmt.Println("    mm view create --name by-base --root /path/to/views/by-base --group-by base_model")
		return nil
	}
	for _, v := range views {
		fmt.Printf("%s\n  root      %s\n  group by  %s\n  entries   %d\n  status    %s",
			v.Name, v.Root, v.GroupBy, v.EntryCount, v.Status)
		if v.Strategy != "" {
			fmt.Printf(" via %s", v.Strategy)
		}
		fmt.Print("\n\n")
	}
	return nil
}

func viewCreate(args []string) error {
	fs := newFlagSet("view create", `mm view create --name NAME --root DIR

Defines a view: a generated directory tree that consuming tools point at.

Nothing is written to disk until you run `+"`mm view generate`"+`. The root must
be outside your scanned model roots, or the next scan would count every entry as
another copy of the model it points at.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	name := fs.String("name", "", "view name (required)")
	root := fs.String("root", "", "directory to generate into (required)")
	groupBy := fs.String("group-by", "flat", "flat | base_model | type | tag | collection")
	var types, baseModels, tags rootList
	fs.Var(&types, "type", "restrict to a model type (repeatable)")
	fs.Var(&baseModels, "base-model", "restrict to a base model (repeatable)")
	fs.Var(&tags, "tag", "restrict to a tag (repeatable)")
	query := fs.String("query", "", "restrict to a full-text search")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *root == "" {
		return errors.New("view create: --name and --root are required")
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	def, err := view.NewManager(st).Create(view.Definition{
		Name:    *name,
		Root:    *root,
		GroupBy: view.Grouping(*groupBy),
		Filter: store.SearchQuery{
			Text: *query, Types: types, BaseModels: baseModels, Tags: tags,
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("created view %q at %s\n\nGenerate it:\n    mm view generate %s\n",
		def.Name, def.Root, def.Name)
	return nil
}

func viewGenerate(ctx context.Context, args []string) error {
	fs := newFlagSet("view generate", `mm view generate NAME

Materializes a view. Adds what is missing, removes what no longer belongs, and
leaves everything else alone, so regenerating after one change does not rewrite
the tree.

Picks the best available mechanism by probing the actual filesystems involved:
a reflink where one is possible, a copy where it is not. Hardlinks are never
chosen automatically -- a tool that rewrites a header in place would write
through one and modify the original.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	strategy := fs.String("strategy", "", "force a mechanism: reflink, block-clone, symlink, hardlink, copy")
	allowHardlink := fs.Bool("allow-hardlink", false, "permit hardlinks (see the warning above)")
	dryRun := fs.Bool("dry-run", false, "report what would change without touching the disk")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("view generate: which view?")
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := view.NewManager(st).Generate(ctx, fs.Arg(0), view.GenerateOptions{
		Strategy:      link.Strategy(*strategy),
		AllowHardlink: *allowHardlink,
		DryRun:        *dryRun,
		Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println("(dry run — nothing was written)")
	}
	fmt.Println(res.Summary())
	return nil
}

func viewDelete(args []string) error {
	fs := newFlagSet("view delete", `mm view delete NAME

Removes a view definition, and optionally the files it created.

Only files this app created are deleted, never everything under the root — a
view pointed at a directory that already held something must not destroy it.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	keepFiles := fs.Bool("keep-files", false, "forget the view but leave its files in place")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("view delete: which view?")
	}

	st, err := openStore(*dbPath, false)
	if err != nil {
		return err
	}
	defer st.Close()

	removed, err := view.NewManager(st).Delete(fs.Arg(0), !*keepFiles)
	if err != nil {
		return err
	}
	fmt.Printf("deleted view %q", fs.Arg(0))
	if !*keepFiles {
		fmt.Printf(", removed %d file(s) it created", removed)
	}
	fmt.Println()
	return nil
}

func cmdLinkProbe(args []string) error {
	fs := newFlagSet("link-probe", `mm link-probe --from SRCDIR --to DSTDIR

Reports which link mechanisms actually work between two directories.

Empirical, not inferred: btrfs subvolumes report different device IDs on the
same filesystem, so comparing them gives a false negative. This attempts each
operation on a real temporary file and reports what succeeded.`)

	from := fs.String("from", "", "directory holding the models")
	to := fs.String("to", "", "directory a view would be generated into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("link-probe: --to is required")
	}

	capability, err := link.Probe(*from, *to)
	if err != nil {
		return err
	}
	fmt.Print(capability.Describe())

	if !link.SharedExtentsSupported() {
		fmt.Println("\nShared-extent detection is unavailable on this platform, so duplicate")
		fmt.Println("reporting cannot tell a reflinked view from a real second copy.")
	}
	return nil
}
