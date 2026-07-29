package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The §11 baseline, which the spec calls non-negotiable for a public build.
//
// Two exposures matter for a tool that binds an HTTP port on a machine full of
// files: users binding 0.0.0.0 on a shared LAN, and DNS-rebinding attacks
// against a localhost API from an ordinary browser tab. Both are cheap to close
// now and painful to retrofit once third-party front-ends depend on the API.

// TailscaleCIDRs are the ranges Tailscale assigns. A tailnet is already
// authenticated, so §11 allows exempting it from the bearer token -- but the
// exemption has to be opt-in and explicit, never inferred.
var TailscaleCIDRs = []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}

// Security holds the request-filtering configuration.
type Security struct {
	// AllowedHosts is the Host header allowlist. Loopback names are always
	// permitted; anything else must be listed.
	AllowedHosts []string

	// AllowedOrigins is the CORS allowlist. There is deliberately no wildcard
	// option: a wildcard on an API that can read every model file on the disk
	// lets any page on the internet enumerate the library.
	AllowedOrigins []string

	// Token is required when RequireToken is set.
	Token        string
	RequireToken bool

	// TrustedCIDRs skip the token. Used for a tailnet, which is authenticated
	// before a packet ever arrives.
	TrustedCIDRs []*net.IPNet
}

// GenerateToken returns a new bearer token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: generating token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// LoadOrCreateToken reads the token file, creating it if absent.
//
// The file exists for third-party clients and the CLI, not for the bundled UI --
// a browser cannot read a file off disk, and any design assuming it can is
// unimplementable (§11). The UI gets its token injected into the page instead.
func LoadOrCreateToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}

	token, err := GenerateToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("api: creating token directory: %w", err)
	}
	// 0600: the token is equivalent to read access to every model file the
	// daemon can see.
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("api: writing token file: %w", err)
	}
	return token, nil
}

// ParseCIDRs converts textual CIDRs into networks.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("api: %q is not a CIDR: %w", c, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// checkHost implements the standard DNS-rebinding defense.
//
// A browser tab on any site can resolve a hostname it controls to 127.0.0.1 and
// then issue same-origin requests to a localhost server. The connection is
// genuinely local, so the network gives no protection; what distinguishes the
// attack is that the Host header carries the attacker's name rather than a
// loopback one. Rejecting unexpected Host values is what closes it.
func (s *Security) checkHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))

	if host == "" {
		// HTTP/1.1 requires a Host header; its absence is not a request we need
		// to serve.
		return false
	}
	if isLoopbackName(host) {
		return true
	}
	for _, allowed := range s.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

func isLoopbackName(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// checkOrigin applies the CORS allowlist.
func (s *Security) checkOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
}

// authorized reports whether a request may proceed.
func (s *Security) authorized(r *http.Request) bool {
	if !s.RequireToken {
		return true
	}
	if s.trustedRemote(r) {
		return true
	}

	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	// Constant-time comparison: a token check that leaks its answer through
	// timing is a token check an attacker can walk byte by byte.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.Token)) == 1
}

func (s *Security) trustedRemote(r *http.Request) bool {
	if len(s.TrustedCIDRs) == 0 {
		return false
	}
	// RemoteAddr only. X-Forwarded-For is attacker-controlled unless a trusted
	// proxy set it, and this daemon is not behind one by design.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range s.TrustedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	// A header-only design would make it impossible to open a preview image in a
	// new browser tab, so the query parameter is accepted as well. It is no
	// weaker: both are same-origin values the page already holds.
	return r.URL.Query().Get("token")
}

// middleware applies the security baseline to every request.
func (s *Security) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkHost(r) {
			http.Error(w,
				"Host header not allowed. This is the DNS-rebinding defense; "+
					"add the hostname with --allow-host if you meant to reach the API by it.",
				http.StatusForbidden)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.checkOrigin(origin) {
				// No wildcard, ever. Omitting the header entirely is what makes
				// the browser refuse the response.
				http.Error(w, "Origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="model-manager"`)
			http.Error(w, "Unauthorized: a bearer token is required when not bound to loopback",
				http.StatusUnauthorized)
			return
		}

		// Defence in depth for the served UI and for any preview blob: no
		// framing, no MIME sniffing, no referrer leakage to an origin server.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		next.ServeHTTP(w, r)
	})
}
