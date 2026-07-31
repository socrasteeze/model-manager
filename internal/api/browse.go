package api

// Remote browsing over HTTP.
//
// These endpoints reach out to third-party services on behalf of the caller,
// which none of the Phase 1 endpoints do. Two consequences shape what is here.
//
// A slow or hostile upstream must not be able to pin the daemon open, so every
// call is bounded by its own timeout regardless of what the client does.
//
// And an unauthenticated caller who could drive these would have a proxy for
// making arbitrary outbound requests from this host. The ordinary security
// middleware (§11) already gates them, but they are additionally refused when
// no origin client is configured rather than silently constructing one.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/origin"
)

// browseTimeout bounds a single browse request.
//
// Generous because a search may resolve file listings for several results, each
// throttled, but finite so a hung upstream cannot hold a connection forever.
const browseTimeout = 90 * time.Second

// updatesTimeout bounds an update check, which is one request per owned model
// and legitimately slow on a large library.
const updatesTimeout = 30 * time.Minute

// browseResponse is what the UI renders.
type browseResponse struct {
	Items []origin.Listing `json:"items"`

	// Errors reports providers that failed, keyed by provider id. A provider
	// being down must be visible as "this source is missing" rather than
	// looking like it simply had no matches.
	Errors map[string]string `json:"errors,omitempty"`

	Providers []string `json:"providers"`
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	registry := s.registry()
	if registry == nil {
		writeError(w, http.StatusServiceUnavailable, "browsing is disabled",
			"the server was started without an origin client")
		return
	}

	q := r.URL.Query()
	limit := clampInt(atoiDefault(q.Get("limit"), 24), 1, 100)

	query := origin.Query{
		Text:       strings.TrimSpace(q.Get("q")),
		Types:      splitCSV(q["type"]),
		BaseModels: splitCSV(q["base_model"]),
		NSFW:       q.Get("nsfw") == "true",
		Sort:       q.Get("sort"),
		Page:       clampInt(atoiDefault(q.Get("page"), 1), 1, 1000),
		Cursor:     q.Get("cursor"),
		Limit:      limit,
	}

	ctx, cancel := context.WithTimeout(r.Context(), browseTimeout)
	defer cancel()

	items, errs := registry.SearchAll(ctx, splitCSV(q["provider"]), query)

	// Without this, HuggingFace results carry no hashes and every one of them
	// would render as "new" even when already owned. See Registry.ResolveFiles.
	if q.Get("resolve") != "false" {
		registry.ResolveFiles(ctx, items, limit)
	}

	resp := browseResponse{Items: items, Providers: registry.IDs()}
	if len(errs) > 0 {
		resp.Errors = map[string]string{}
		for id, err := range errs {
			resp.Errors[id] = err.Error()
		}
	}
	// An annotation failure must be visible, not silent: without the local
	// index every result renders as unowned, which is a wrong answer that
	// invites re-downloading the library.
	if idx, err := origin.BuildLocalIndex(s.cfg.Store); err == nil {
		idx.Annotate(items)
	} else {
		if resp.Errors == nil {
			resp.Errors = map[string]string{}
		}
		resp.Errors["local"] = "could not read the library index; have/update status is missing: " + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

type updatesResponse struct {
	Updates []origin.Update `json:"updates"`
	Checked int             `json:"checked"`
	Errors  int             `json:"errors"`

	// RateLimited marks a partial sweep; see origin.UpdateStats.
	RateLimited bool `json:"rate_limited,omitempty"`
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	registry := s.registry()
	if registry == nil {
		writeError(w, http.StatusServiceUnavailable, "update checking is disabled",
			"the server was started without an origin client")
		return
	}

	// Single flight. An update check is one upstream request per owned model
	// with a 30-minute budget; concurrent invocations would multiply that
	// against a rate-limited public API. On the loopback default this
	// endpoint is also reachable without a credential by anything that can
	// make the browser fetch a URL, so the cap doubles as abuse containment:
	// N injected <img> tags get one sweep, not N.
	if !s.updateCheck.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "an update check is already running",
			"wait for it to finish; concurrent checks would multiply requests against the provider")
		return
	}
	defer s.updateCheck.Store(false)

	idx, err := origin.BuildLocalIndex(s.cfg.Store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not index the library", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), updatesTimeout)
	defer cancel()

	updates, stats, err := origin.CheckUpdates(ctx, idx, origin.UpdateOptions{
		Client: s.cfg.Origin,
		Limit:  clampInt(atoiDefault(r.URL.Query().Get("limit"), 0), 0, 100000),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "update check failed", err.Error())
		return
	}

	origin.MarkBaseModelChanges(updates, func(sha string) string {
		rec, err := s.cfg.Store.GetModelRecord(sha)
		if err != nil || rec == nil {
			return ""
		}
		return rec.BaseModel
	})

	writeJSON(w, http.StatusOK, updatesResponse{
		Updates:     updates,
		Checked:     stats.Checked,
		Errors:      stats.Errors,
		RateLimited: stats.RateLimited,
	})
}

// handleRemoteImage proxies a provider's thumbnail.
//
// Two problems solved at once. The page's CSP is img-src 'self' data:, so a
// remote URL in an <img> is refused outright and every preview would silently
// fail to load. And loading them directly would have the browser contact the
// provider's CDN on every search, disclosing the viewer's address and browsing
// to a third party the daemon was otherwise mediating.
//
// This is an outbound fetcher driven by a URL from the client, which is an SSRF
// primitive if left open, so the host must be one this server already talks to:
// a configured provider API host, or a known provider image CDN. Redirects are
// re-checked against the same rule, since an allowed host can otherwise bounce
// the request to an internal address.
//
// Nothing is persisted. These are previews for models that are not owned, and
// writing them into the blob store would fill it with images for files that
// were never downloaded.
func (s *Server) handleRemoteImage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Origin == nil {
		writeError(w, http.StatusServiceUnavailable, "browsing is disabled")
		return
	}

	raw := r.URL.Query().Get("url")
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "invalid image url")
		return
	}
	if !s.imageHostAllowed(target.Host) {
		writeError(w, http.StatusForbidden, "image host not allowed", target.Host)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid image url")
		return
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !s.imageHostAllowed(req.URL.Host) {
				return fmt.Errorf("redirect to disallowed host %s", req.URL.Host)
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not fetch image", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "image fetch failed",
			strconv.Itoa(resp.StatusCode))
		return
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read image")
		return
	}
	// Sniffed, never trusted from the response header: this endpoint must not
	// become a way to serve arbitrary content from the daemon's own origin.
	if !blobstore.IsImage(data) {
		writeError(w, http.StatusUnsupportedMediaType, "not an image")
		return
	}

	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

const maxRemoteImageBytes = 8 << 20

// imageCDNHosts are the provider image hosts, which are separate from their API
// hosts and so are not covered by the configured bases.
var imageCDNHosts = []string{
	"image.civitai.com",
	"imagecache.civitai.com",
	"civitai.com",
	"civitai.red",
	"civitai.green",
	"cdn.civitai.com",
	"civarchive.com",
	"cdn.civarchive.com",
	"huggingface.co",
	"cdn-uploads.huggingface.co",
	"cdn-lfs.huggingface.co",
}

func (s *Server) imageHostAllowed(host string) bool {
	h := strings.ToLower(host)
	for _, allowed := range imageCDNHosts {
		if h == allowed {
			return true
		}
	}
	// Also allow whatever provider hosts this server is configured against, so
	// a mirror or a test server works without special-casing.
	//
	// Nil-checked because this is reached from the download path too, which can
	// be enabled without an origin client. handleRemoteImage guards earlier;
	// downloadHostAllowed does not, and a nil dereference here would take out
	// the request rather than simply denying the host.
	if s.cfg.Origin == nil {
		return false
	}
	for _, base := range s.cfg.Origin.ConfiguredHosts() {
		if h == base {
			return true
		}
	}
	return false
}

// registry builds a provider registry, or nil when browsing is disabled.
func (s *Server) registry() *origin.Registry {
	if s.cfg.Origin == nil {
		return nil
	}
	return origin.NewRegistry(s.cfg.Origin)
}

func splitCSV(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func atoiDefault(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
