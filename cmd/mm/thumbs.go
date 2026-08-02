package main

// mm thumbs -- derive grid-sized copies of previews already in the library.
//
// New previews get a thumbnail as they arrive (enrichment and upload both do
// it). This is the one-off pass for everything ingested before that existed, so
// a library built over the last five phases stops sending full-size renders to
// the grid without waiting for a re-enrich.

import (
	"context"
	"fmt"
	"os"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/thumb"
)

func cmdThumbs(ctx context.Context, args []string) error {
	fs := newFlagSet("thumbs", `mm thumbs

Derives a small grid copy of every preview image that does not have one yet.

Purely additive: the original preview is never touched, and a preview that
already has a thumbnail, is already small, or cannot be decoded is skipped.
Safe to interrupt and rerun -- it picks up where it stopped.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	blobDir := fs.String("blobs", "", "preview blob directory (default: alongside the database)")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")
	limit := fs.Int("limit", 0, "stop after this many previews (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dbPath, *allowNetDB)
	if err != nil {
		return err
	}
	defer st.Close()

	blobs, err := blobstore.New(orDefault(*blobDir, defaultBlobDir(st.Path())))
	if err != nil {
		return err
	}

	pending, err := st.PreviewsWithoutThumbnails(*limit)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("every preview already has a thumbnail")
		return nil
	}

	var made, skipped, failed int
	var savedBytes int64
	for i, p := range pending {
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(os.Stderr, "\r%d/%d...", i+1, len(pending))

		data, err := blobs.Read(p.ImageSHA256)
		if err != nil {
			failed++
			continue
		}
		t, err := thumb.Derive(data)
		if err != nil {
			// Already small, or not decodable. Either way there is nothing to
			// derive, and recording the attempt is not worth a column.
			skipped++
			continue
		}
		tb, err := blobs.Put(t.Data)
		if err != nil {
			failed++
			continue
		}
		if err := st.SetPreviewThumbnail(p.SHA256, p.ImageSHA256, tb.SHA256,
			t.SourceWidth, t.SourceHeight); err != nil {
			failed++
			continue
		}
		made++
		savedBytes += p.Bytes - int64(len(t.Data))
	}
	fmt.Fprintln(os.Stderr)

	fmt.Printf("%d thumbnail(s) derived, %d skipped, %d failed\n", made, skipped, failed)
	if made > 0 {
		fmt.Printf("grid now sends %s less per full page of these previews\n",
			humanBytes(savedBytes))
	}
	return nil
}
