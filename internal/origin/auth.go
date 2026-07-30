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

// tokenFor returns the bearer credential to send to rawURL, or "" for none.
func (c *Client) tokenFor(rawURL string) string {
	host := hostOf(rawURL)
	if host == "" {
		return ""
	}
	switch {
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
