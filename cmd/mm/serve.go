package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/socrasteeze/model-manager/internal/api"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/scan"
	"github.com/socrasteeze/model-manager/internal/scanjob"
	"github.com/socrasteeze/model-manager/internal/webui"
)

func cmdServe(ctx context.Context, args []string) error {
	fs_ := newFlagSet("serve", `mm serve

Runs the daemon: HTTP API, and the web UI if this build embeds one.

Binds 127.0.0.1 by default. Any other interface is an explicit opt-in and
requires a bearer token, because an API that can read every model file on the
disk should not be reachable from a shared LAN without one.`)

	dbPath := fs_.String("db", defaultDBPath(), "path to the master database")
	blobDir := fs_.String("blobs", "", "preview blob directory (default: alongside the database)")
	host := fs_.String("host", "127.0.0.1", "interface to bind")
	port := fs_.Int("port", 8737, "port to listen on")
	writable := fs_.Bool("writable", false, "allow edits (Phase 1 defaults to read-only until the index is proven)")
	noRemote := fs_.Bool("no-remote", false, "disable remote browsing and update checking (no outbound requests)")
	tailnet := fs_.Bool("tailnet", false, "trust Tailscale addresses without a token")
	tokenPath := fs_.String("token-file", "", "where to read/write the API token (default: alongside the database)")
	noToken := fs_.Bool("no-token", false, "do not require a token even when bound off-loopback (unsafe)")
	allowNetDB := fs_.Bool("allow-network-db", false, "permit a database on a network filesystem")
	scanWorkers := fs_.Int("scan-workers", 1, "hashing workers per storage device for scans started from the UI")

	var allowHosts rootList
	fs_.Var(&allowHosts, "allow-host", "extra Host header to accept (repeatable)")
	var allowOrigins rootList
	fs_.Var(&allowOrigins, "allow-origin", "extra CORS origin to accept (repeatable)")
	var trustCIDRs rootList
	fs_.Var(&trustCIDRs, "trust-cidr", "network whose clients skip the token (repeatable)")

	if err := fs_.Parse(args); err != nil {
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

	addr := api.ListenAddr{Host: *host, Port: *port}

	sec := api.Security{
		AllowedHosts:   allowHosts,
		AllowedOrigins: allowOrigins,
	}

	// A token is mandatory off-loopback. On loopback it is pointless: anything
	// that can reach the port can already read the token file.
	sec.RequireToken = !addr.IsLoopback() && !*noToken

	cidrs := append([]string(nil), trustCIDRs...)
	if *tailnet {
		cidrs = append(cidrs, api.TailscaleCIDRs...)
	}
	if sec.TrustedCIDRs, err = api.ParseCIDRs(cidrs); err != nil {
		return err
	}

	if sec.RequireToken {
		path := orDefault(*tokenPath, filepath.Join(filepath.Dir(st.Path()), "api-token"))
		if sec.Token, err = api.LoadOrCreateToken(path); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "API token: %s\n", path)
	}

	if *noToken && !addr.IsLoopback() {
		fmt.Fprintln(os.Stderr,
			"mm: warning: --no-token with a non-loopback bind. Anything that can reach\n"+
				"    this port can read every model file this daemon can see.")
	}

	// Browsing and update checking are the only features that make outbound
	// requests. Leaving the client unset is how an operator keeps the daemon
	// from talking to third parties at all, without needing a firewall rule.
	var originClient *origin.Client
	if !*noRemote {
		originClient = origin.NewClient()
	}

	// Downloading additionally requires --writable. It creates files, and the
	// read-only default exists precisely so a daemon cannot be talked into
	// writing anything before the index is proven.
	var downloads *download.Manager
	if !*noRemote && *writable {
		mgr, err := download.NewManager(defaultDownloadDir(st.Path()))
		if err != nil {
			return fmt.Errorf("serve: preparing downloads: %w", err)
		}
		// Depends on originClient existing, which holds: both are gated on
		// !*noRemote. Credentials are selected per host so the Civitai key is
		// never presented to HuggingFace or anywhere else.
		mgr.TokenFor = originClient.TokenFor
		downloads = mgr
	}

	// Scanning from the UI is a write, so it is offered only on a writable
	// daemon -- same rule as downloading. A read-only server serves an index
	// somebody else built and has no business rewriting it.
	var scans *scanjob.Manager
	if *writable {
		scans = scanjob.New(st, scan.Options{
			WorkersPerDevice: *scanWorkers,
			// A scan started from the UI is nearly always a rescan of a library
			// that is already indexed, which is exactly the case the sampled
			// probe exists for: it turns a full re-read of an unchanged
			// multi-gigabyte file into a sampled one.
			UseProbe: true,
		})
	}

	// Enrichment writes metadata and previews, so it needs --writable, and it
	// asks a third party, so it needs a client --no-remote has not withheld.
	// Neither condition implies the other, hence both.
	//
	// Deliberately the *same* client the browse and update endpoints use, rather
	// than one of its own. The throttle lives on the client, so sharing it means
	// a sweep running while somebody browses stays within one request budget
	// between them -- two clients would each be politely paced and together
	// double the rate, which is how a sweep earns the rate limit that stops it.
	var enrichment *enrichjob.Manager
	if !*noRemote && *writable {
		enrichment = enrichjob.New(st, blobs, func() *origin.Client { return originClient })
	}

	// Rendering a thumbnail with ComfyUI creates a preview, so it follows the
	// same --writable rule as every other write. Deliberately *not* gated on
	// --no-remote: that flag stops the daemon talking to third parties, and a
	// ComfyUI address the operator typed into their own settings is not one.
	// With no address configured nothing is contacted at all.
	var renders *comfy.Manager
	if *writable {
		renders = comfy.NewManager(nil, nil)
	}

	srv := api.New(api.Config{
		Store:     st,
		Blobs:     blobs,
		UI:        embeddedUI(),
		Security:  sec,
		Version:   version,
		ReadOnly:  !*writable,
		Origin:    originClient,
		Enrich:    enrichment,
		Scans:     scans,
		Renders:   renders,
		Downloads: downloads,
	})

	listener, err := net.Listen("tcp", addr.String())
	if err != nil {
		return fmt.Errorf("serve: binding %s: %w", addr, err)
	}

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a preview of a large image over a slow tailnet link
		// is a legitimate slow response, and cutting it off would look like a
		// bug rather than a limit.
		IdleTimeout: 120 * time.Second,
	}

	printStartup(addr, sec, *writable, st.Path())

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nmm: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func printStartup(addr api.ListenAddr, sec api.Security, writable bool, dbPath string) {
	scheme := "http://"
	display := addr.Host
	if display == "" || display == "0.0.0.0" || display == "::" {
		display = "<every interface>"
	}
	fmt.Fprintf(os.Stderr, "model-manager listening on %s%s\n", scheme, addr.String())
	fmt.Fprintf(os.Stderr, "  database   %s\n", dbPath)
	fmt.Fprintf(os.Stderr, "  interface  %s\n", display)

	switch {
	case sec.RequireToken && len(sec.TrustedCIDRs) > 0:
		fmt.Fprintln(os.Stderr, "  auth       bearer token (trusted networks exempt)")
	case sec.RequireToken:
		fmt.Fprintln(os.Stderr, "  auth       bearer token required")
	case addr.IsLoopback():
		fmt.Fprintln(os.Stderr, "  auth       none (loopback only)")
	default:
		fmt.Fprintln(os.Stderr, "  auth       NONE — this port is exposed without a token")
	}

	if writable {
		fmt.Fprintln(os.Stderr, "  mode       writable")
	} else {
		fmt.Fprintln(os.Stderr, "  mode       read-only (start with --writable to enable editing)")
	}
	fmt.Fprintf(os.Stderr, "\n  UI    %s%s/\n", scheme, addr.String())
	fmt.Fprintf(os.Stderr, "  API   %s%s/openapi.json\n\n", scheme, addr.String())
}

// embeddedUI returns the built front-end, or nil if this build has none.
func embeddedUI() fs.FS {
	return webui.FS()
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
