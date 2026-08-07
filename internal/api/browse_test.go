package api

import (
	"net/http"
	"testing"

	"github.com/socrasteeze/model-manager/internal/origin"
)

// imageHostAllowed is what stood between every Browse thumbnail and a 502:
// image.civitai.com started 301-redirecting to image-b2.civitai.com, a host
// the old exact-hostname list never enumerated, so handleRemoteImage's
// CheckRedirect refused every single preview. Pinned directly so the next
// provider CDN move (a near certainty -- it has already happened once) is
// caught by a test rather than discovered as "the thumbnails are broken".
func TestImageHostAllowed(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = nil })

	allowed := []string{
		// The regression: Civitai's actual current image CDN redirect target.
		"image-b2.civitai.com",
		// Every host previously enumerated by exact match, now covered by
		// suffix matching against the base domain instead.
		"image.civitai.com", "imagecache.civitai.com", "cdn.civitai.com",
		"civitai.com", "civitai.red", "civitai.green",
		"civarchive.com", "cdn.civarchive.com",
		"huggingface.co", "cdn-uploads.huggingface.co", "cdn-lfs.huggingface.co",
		// HuggingFace's per-region resolve CDN, and the hf.co short domain --
		// neither was ever on the image list, only the download one.
		"cdn-lfs-us-1.huggingface.co", "hf.co",
	}
	for _, h := range allowed {
		if !s.imageHostAllowed(h) {
			t.Errorf("provider image host %q rejected", h)
		}
	}

	// A lookalike must not pass on a substring or suffix-without-dot match --
	// the same anti-spoofing bar the download allowlist already holds itself
	// to.
	denied := []string{
		"evil.com", "civitai.com.attacker.net", "notciviai.com",
		"huggingface.co.evil.test", "xcivitai.com", "169.254.169.254", "localhost",
	}
	for _, h := range denied {
		if s.imageHostAllowed(h) {
			t.Errorf("host %q was allowed as an image source", h)
		}
	}
}

// downloadHostAllowed now delegates to imageHostAllowed entirely -- this
// pins that the two have not silently diverged again into two lists that can
// drift out of sync, which is how the image list went stale in the first
// place while the download list (already suffix-matched) did not.
func TestDownloadAndImageAllowlistsAgree(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = nil })

	hosts := []string{
		"image-b2.civitai.com", "civitai.com", "civitai.red", "civarchive.com",
		"huggingface.co", "hf.co", "cdn-lfs-us-1.huggingface.co",
		"evil.com", "civitai.com.attacker.net",
	}
	for _, h := range hosts {
		if s.downloadHostAllowed(h) != s.imageHostAllowed(h) {
			t.Errorf("downloadHostAllowed(%q) = %v, imageHostAllowed(%q) = %v -- diverged",
				h, s.downloadHostAllowed(h), h, s.imageHostAllowed(h))
		}
	}
}

// TestUpstreamHostAllowedExactlyAndOnlyWhenConfigured is the SSRF boundary for
// the whole upstream feature. Every other host on this allowlist is a public
// provider domain matched by suffix; the upstream is a private host matched by
// equality, and the difference is load-bearing.
//
// The localhost cases are deliberate rather than paranoid. Running two daemons
// on one box, or reaching the NAS through an SSH tunnel, makes
// MM_UPSTREAM_URL=http://localhost:8737 a legitimate configuration -- so the
// allowlist has to widen for that exact port without widening "localhost",
// which the pre-existing tests below pin as denied.
func TestUpstreamHostAllowedExactlyAndOnlyWhenConfigured(t *testing.T) {
	configured := newServer(t, func(c *Config) {
		c.Origin = &origin.Client{UpstreamBase: "http://nas.example:8737"}
	})
	if !configured.imageHostAllowed("nas.example:8737") {
		t.Error("configured upstream host rejected")
	}
	denied := []string{
		// No port, wrong port, and a sibling subdomain: none of these is the
		// machine the operator named, and a suffix match would accept the last.
		"nas.example", "nas.example:9999", "evil.nas.example:8737",
		"localhost", "127.0.0.1", "169.254.169.254", "evil.com",
	}
	for _, h := range denied {
		if configured.imageHostAllowed(h) {
			t.Errorf("host %q allowed by an upstream configured as nas.example:8737", h)
		}
	}

	// The tunnel case: an explicit localhost upstream widens its own port only.
	tunnel := newServer(t, func(c *Config) {
		c.Origin = &origin.Client{UpstreamBase: "http://localhost:8737"}
	})
	if !tunnel.imageHostAllowed("localhost:8737") {
		t.Error("explicitly configured localhost upstream rejected")
	}
	if tunnel.imageHostAllowed("localhost") || tunnel.imageHostAllowed("localhost:9999") {
		t.Error("localhost upstream widened beyond its own port")
	}

	// Without an upstream the host is denied, so the widening cannot be reached
	// by any daemon that did not opt in.
	unconfigured := newServer(t, func(c *Config) { c.Origin = &origin.Client{} })
	if unconfigured.imageHostAllowed("nas.example:8737") {
		t.Error("upstream host allowed with no upstream configured")
	}

	// And the download side must not drift from the image side, under every one
	// of these configurations -- the property TestDownloadAndImageAllowlistsAgree
	// pins for the static list.
	for _, s := range []*Server{configured, tunnel, unconfigured} {
		for _, h := range append(denied, "nas.example:8737", "localhost:8737") {
			if s.downloadHostAllowed(h) != s.imageHostAllowed(h) {
				t.Errorf("allowlists diverged for %q: download=%v image=%v",
					h, s.downloadHostAllowed(h), s.imageHostAllowed(h))
			}
		}
	}
}

// handleRemoteImage must refuse a disallowed host before making any outbound
// request -- checked here without touching the network, since the rejection
// happens at the URL-parse stage.
func TestRemoteImageRejectsDisallowedHost(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = &origin.Client{} })

	w := do(s, "GET", "http://127.0.0.1/api/remote-image?url="+
		"https%3A%2F%2Fevil.com%2Ftracking-pixel.png", "", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a disallowed image host: %s", w.Code, w.Body.String())
	}
}

func TestRemoteImageUnavailableWithoutAnOriginClient(t *testing.T) {
	s := newServer(t, func(c *Config) { c.Origin = nil })

	w := do(s, "GET", "http://127.0.0.1/api/remote-image?url="+
		"https%3A%2F%2Fimage.civitai.com%2Fsomething.jpg", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no origin client", w.Code)
	}
}
