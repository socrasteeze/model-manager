package origin

// Credential scoping.
//
// One Client now talks to three unrelated third parties. A single Authorization
// header applied to every request would mean a Civitai API key is handed to
// HuggingFace the first time someone searches both -- a working credential
// disclosed to a service that has no business holding it, and one the user would
// have no way to notice.
//
// So credentials are selected by host. A request to a host this client has no
// credential for is sent unauthenticated rather than authenticated with
// somebody else's token.

import (
	"net/url"
	"strings"
)

// TokenFor returns the bearer credential to send to rawURL, or "" for none.
//
// Exported because the download manager needs the same host-scoped selection
// for transfers: a Manager that applied one key to every request would hand
// the Civitai credential to huggingface.co on the first cross-provider
// download, which is the exact leak this function exists to prevent.
func (c *Client) TokenFor(rawURL string) string {
	host := hostOf(rawURL)
	if host == "" {
		return ""
	}
	switch {
	case hostMatches(host, c.upstreamBase()):
		// First on purpose. If MM_CIVITAI_API were ever pointed at the upstream
		// host, a later case would hand the Civitai key to the NAS; ordering the
		// upstream ahead of the public providers means the worst outcome of that
		// misconfiguration is an unauthenticated Civitai request, not a working
		// third-party credential disclosed onto the local network.
		return c.UpstreamToken
	case hostMatches(host, c.civitaiBase()):
		return c.APIKey
	case hostMatches(host, c.huggingFaceBase()), isHuggingFaceHost(host):
		return c.HFToken
	case hostMatches(host, c.civArchiveBase()):
		// CivArchive is a public mirror and has no credential of its own.
		return ""
	}
	return ""
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// hostMatches reports whether host is the host of base.
//
// Compared exactly, including port. A suffix match would treat
// "evil-huggingface.co" as HuggingFace, which is precisely the mistake that
// leaks the token this function exists to protect.
func hostMatches(host, base string) bool {
	b := hostOf(base)
	return b != "" && strings.EqualFold(host, b)
}

func isHuggingFaceHost(host string) bool {
	h := strings.ToLower(host)
	return h == "huggingface.co" || h == "hf.co" ||
		strings.HasSuffix(h, ".huggingface.co")
}

// siteBase turns an API root into the site root by dropping a trailing /api
// segment, so download URLs can be built against the same host the API came
// from. This keeps a mirror (or a test server) self-consistent instead of
// silently sending downloads to the real site.
func siteBase(apiBase string) string {
	trimmed := strings.TrimRight(apiBase, "/")
	if rest, ok := strings.CutSuffix(trimmed, "/api"); ok {
		return rest
	}
	return trimmed
}

// ConfiguredHosts lists the hosts this client is pointed at.
//
// Used by the daemon to decide which image hosts it is willing to proxy: a
// mirror or a test server configured through MM_*_API should work without
// being special-cased anywhere else.
// The upstream is included, and that is the whole of the allowlist change needed
// to make pulling work: the daemon's image proxy and its download host check are
// the same function, so both widen together and cannot drift apart. The widening
// is exactly one host, because callers compare these entries for equality while
// the suffix match applies only to the static public-provider list -- so
// hostOf's port is part of the match and a sibling subdomain is not.
func (c *Client) ConfiguredHosts() []string {
	var out []string
	for _, base := range []string{c.civitaiBase(), c.huggingFaceBase(), c.civArchiveBase(), c.upstreamBase()} {
		if h := hostOf(base); h != "" {
			out = append(out, h)
		}
	}
	return out
}
