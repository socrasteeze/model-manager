package main

// Freeing space taken by a pulled copy, from a shell.
//
// Every safety check lives in internal/evict, which the daemon's endpoint calls
// too. This command is the flag parsing, the confirmation and the reporting --
// deliberately nothing else. A destructive operation with this many
// preconditions must not have a second implementation of them that can drift
// from the first, and the CLI is exactly where that drift would go unnoticed.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/socrasteeze/model-manager/internal/evict"
	"github.com/socrasteeze/model-manager/internal/store"
)

func cmdEvict(args []string) error {
	fs := newFlagSet("evict", `mm evict <sha256>

Removes a local copy of a model that was pulled from an upstream library, and
records that it is no longer here.

Only a copy this machine pulled can be evicted, because only then is it
provably re-fetchable. Everything the library knows about the model -- its
name, tags, previews, provenance and your own edits -- is kept, and it stays
listed as available from the upstream, so pulling it again restores it.

Refused when the file has changed since it was indexed, when several copies are
present and none is named, and when a generated view still links to it: with a
hardlinked view the delete would free nothing while appearing to succeed.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")
	path := fs.String("path", "", "which copy to remove, when more than one is present")
	upstream := fs.String("upstream", "", "which upstream, when the model was pulled from several")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("evict: name exactly one model by its SHA256")
	}
	sha := strings.ToLower(strings.TrimSpace(fs.Arg(0)))

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	// Shown before the prompt so the confirmation names a real path and a real
	// number, rather than asking the user to agree to something abstract.
	pull, err := st.ResidentPull(sha, *upstream)
	if err != nil {
		if errors.Is(err, store.ErrNoPulledCopy) {
			return errors.New(
				"evict: this model was not pulled from an upstream, so it is not re-fetchable " +
					"and will not be deleted")
		}
		return err
	}

	if !*yes {
		fmt.Printf("Remove this copy from this machine?\n\n  %s\n\n", pull.Path)
		fmt.Printf("Frees %s. The library keeps everything it knows about it, and the model\n"+
			"stays listed as available from %s.\n\n", humanSize(pull.SizeBytes), pull.Upstream)
		if !confirm() {
			fmt.Println("Left alone.")
			return nil
		}
	}

	res, err := evict.Do(st, evict.Request{SHA256: sha, Path: *path, Upstream: *upstream})
	if err != nil {
		return err
	}
	if res.AlreadyGone {
		fmt.Printf("already gone: %s (the index has been updated)\n", res.Path)
		return nil
	}
	fmt.Printf("evicted %s, freeing %s\n", res.Path, humanSize(res.FreedBytes))
	fmt.Printf("still available from %s\n", res.Upstream)
	return nil
}

func confirm() bool {
	fmt.Print("Type yes to continue: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
