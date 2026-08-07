package api

// Reporting on the configured upstream library.
//
// The settings UI needs to answer four separate questions that all look like
// "is it working": has an upstream been named at all, can this daemon reach it,
// does it accept our token, and will it actually hand over model files. They
// fail independently and each has a different fix, so they are reported
// independently rather than collapsed into one boolean.
//
// Nothing here is writable. The upstream is configured through the environment
// -- see Client.UpstreamBase for why that is not a preference -- so this
// endpoint exists to explain the configuration, never to change it.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/origin"
)

// File-serving states for an upstream.
const (
	// UpstreamFilesYes means the upstream advertises serve-files.
	UpstreamFilesYes = "yes"
	// UpstreamFilesNo means the upstream answered, and said it does not.
	UpstreamFilesNo = "no"
	// UpstreamFilesUnknown means the upstream never mentioned the question --
	// it predates the capability list, so its answer has to be probed for.
	UpstreamFilesUnknown = "unknown"
)

// UpstreamStatus describes the configured upstream to the UI.
//
// Deliberately carries no token and no upstream filesystem path. Host is
// host:port, which the operator typed themselves and needs to see to confirm
// they typed the right one.
type UpstreamStatus struct {
	Configured    bool   `json:"configured"`
	Reachable     bool   `json:"reachable"`
	Authenticated bool   `json:"authenticated"`
	Name          string `json:"name,omitempty"`
	Host          string `json:"host,omitempty"`
	Version       string `json:"version,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`

	// Files is one of the UpstreamFiles* constants.
	Files string `json:"files"`

	// CanPull folds in this daemon's own side of the question: an upstream that
	// serves files is no use if downloads are disabled here.
	CanPull bool `json:"can_pull"`

	// Error is the human-readable reason the answer above is not "all fine".
	Error string `json:"error,omitempty"`
}

func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.upstreamStatus(r.Context()))
}

func (s *Server) upstreamStatus(ctx context.Context) UpstreamStatus {
	st := UpstreamStatus{Files: UpstreamFilesUnknown}

	c := s.cfg.Origin
	if c == nil || !c.HasUpstream() {
		st.Error = "no upstream configured; set MM_UPSTREAM_URL (and MM_UPSTREAM_TOKEN) and restart"
		return st
	}
	st.Configured = true
	st.Name = c.UpstreamLabel()
	base := upstreamBaseOf(c)
	if u, err := url.Parse(base); err == nil {
		st.Host = u.Host
	}

	// Short timeout and no retries: this runs behind a "test connection" button
	// and behind a settings panel that renders on open. A caller waiting on the
	// answer would rather have "unreachable" in five seconds than the truth in
	// forty, and the retrying client exists for transfers, not for probes.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	health, status, err := s.upstreamGET(ctx, base+"/api/health")
	switch {
	case err != nil:
		st.Error = "could not reach " + st.Host + ": " + err.Error()
		return st
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Reached it, so the address and the network are fine; only the
		// credential is wrong. Saying so is the difference between the operator
		// checking their firewall and checking their token.
		st.Reachable = true
		st.Error = "the upstream rejected our credentials; check MM_UPSTREAM_TOKEN against its api-token file"
		return st
	case status != http.StatusOK:
		st.Reachable = true
		st.Error = "the upstream returned HTTP " + http.StatusText(status) + " from /api/health"
		return st
	}
	st.Reachable = true
	st.Authenticated = true

	var body struct {
		Version       string    `json:"version"`
		SchemaVersion int       `json:"schema_version"`
		Capabilities  *[]string `json:"capabilities"`
	}
	if err := json.Unmarshal(health, &body); err != nil {
		st.Error = "the upstream's /api/health was not model-manager's: " + err.Error()
		st.Authenticated = false
		st.Reachable = false
		return st
	}
	st.Version = body.Version
	st.SchemaVersion = body.SchemaVersion

	// A pointer, so the three cases stay distinct: a list containing the
	// capability, a list that does not, and no list at all. The third is an
	// upstream older than the capability list, which is a different problem with
	// a different fix, and flattening it into "no" would tell the operator to
	// set a flag their build does not have.
	switch {
	case body.Capabilities == nil:
		st.Files = UpstreamFilesUnknown
	case containsString(*body.Capabilities, CapServeFiles):
		st.Files = UpstreamFilesYes
	default:
		st.Files = UpstreamFilesNo
	}
	if st.Files == UpstreamFilesUnknown {
		st.Files = s.probeUpstreamFiles(ctx, base)
	}

	switch st.Files {
	case UpstreamFilesNo:
		st.Error = "the upstream is not serving model files; restart it with --serve-files"
	case UpstreamFilesUnknown:
		st.Error = "this upstream is older than the file-serving feature; upgrade it to pull models from it"
	}

	// The local half. An upstream that will hand over files is no use on a
	// daemon that has been told not to write any.
	if st.Files == UpstreamFilesYes {
		switch {
		case s.cfg.ReadOnly:
			st.Error = "this daemon is read-only; restart it with --writable to pull models"
		case s.cfg.Downloads == nil:
			st.Error = "downloads are disabled on this daemon"
		default:
			st.CanPull = true
		}
	}
	return st
}

// probeUpstreamFiles asks an upstream too old to advertise capabilities.
//
// The trick is that handleModelFile refuses with 503 before it looks at
// anything else, so any *other* status from that route means the route exists
// and serving is on. An upstream without the route falls through to the SPA
// handler, which answers deep links with index.html -- so the discriminator is
// the content type, not the status code: this API always answers in JSON.
func (s *Server) probeUpstreamFiles(ctx context.Context, base string) string {
	// An all-zero hash matches nothing, so the probe cannot start a transfer
	// even against a daemon that is serving files.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		base+"/api/models/"+strings.Repeat("0", 64)+"/file", nil)
	if err != nil {
		return UpstreamFilesUnknown
	}
	s.authorizeUpstream(req)

	resp, err := s.outboundClient(5 * time.Second).Do(req)
	if err != nil {
		return UpstreamFilesUnknown
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusServiceUnavailable {
		return UpstreamFilesNo
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return UpstreamFilesYes
	}
	return UpstreamFilesUnknown
}

// upstreamGET performs one unretried GET against the upstream.
func (s *Server) upstreamGET(ctx context.Context, target string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	s.authorizeUpstream(req)

	resp, err := s.outboundClient(5 * time.Second).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// Capped for the same reason the origin client caps its reads: a URL that
	// turns out not to be a model-manager can return anything at all.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

// authorizeUpstream attaches the upstream's credential, host-scoped through the
// same selection every other outbound request here uses.
func (s *Server) authorizeUpstream(req *http.Request) {
	if s.cfg.Origin == nil {
		return
	}
	if token := s.cfg.Origin.TokenFor(req.URL.String()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// upstreamBaseOf reads the client's upstream root through its public surface.
func upstreamBaseOf(c *origin.Client) string {
	return strings.TrimRight(strings.TrimSpace(c.UpstreamBase), "/")
}

// upstreamBaseFor returns the configured upstream base when target is on it.
//
// Compared by host including the port, matching how the allowlist admitted the
// URL in the first place -- two daemons on one machine differ only by port, and
// a host-only comparison would label a transfer from one as coming from the
// other.
func (s *Server) upstreamBaseFor(target *url.URL) string {
	if s.cfg.Origin == nil || target == nil {
		return ""
	}
	base := upstreamBaseOf(s.cfg.Origin)
	if base == "" {
		return ""
	}
	configured, err := url.Parse(base)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(configured.Host, target.Host) {
		return ""
	}
	return base
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
