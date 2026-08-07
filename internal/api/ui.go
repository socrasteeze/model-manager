package api

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiHandler serves the embedded front-end.
//
// Two things it does beyond a plain file server:
//
//  1. Injects the bearer token into index.html. §11 is explicit that the token
//     is *also* written to a file for third-party clients and the CLI, but that
//     copy is not for the bundled UI -- a browser cannot read a file off disk,
//     and any design assuming it can is unimplementable. Same-origin injection
//     is what makes the served UI work when a token is required.
//
//  2. Falls back to index.html for unknown paths, because the UI is a
//     single-page app and a deep link must not 404.
func (s *Server) uiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		data, err := fs.ReadFile(s.cfg.UI, upath)
		if err != nil {
			// Anything that is not a real asset is a client-side route.
			// Extensions are excluded so a genuinely missing script 404s rather
			// than silently returning HTML, which is a maddening thing to debug.
			if path.Ext(upath) != "" {
				http.NotFound(w, r)
				return
			}
			data, err = fs.ReadFile(s.cfg.UI, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			upath = "index.html"
		}

		if upath == "index.html" {
			data = s.injectToken(data)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// The page carries a credential, so it must never be cached by a
			// shared cache or served stale after the token rotates.
			w.Header().Set("Cache-Control", "no-store")
			// A strict CSP: everything the UI needs is same-origin, so nothing
			// external is permitted to load or connect.
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
					"script-src 'self' 'unsafe-inline'; connect-src 'self'; object-src 'none'; "+
					"base-uri 'none'; frame-ancestors 'none'")
			_, _ = w.Write(data)
			return
		}

		if ctype := contentTypeFor(upath); ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		// Vite emits content-hashed asset names, so these are safe to cache hard.
		if strings.HasPrefix(upath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		_, _ = io.Copy(w, bytes.NewReader(data))
	})
}

// injectToken places the bearer token and API base into the served page.
func (s *Server) injectToken(html []byte) []byte {
	token := ""
	if s.cfg.Security.RequireToken {
		token = s.cfg.Security.Token
	}
	// Calls the same check the enrichment endpoints themselves gate on, rather
	// than restating its two conditions here -- a duplicated condition is
	// exactly how this flag would drift out of step with what the server
	// actually enforces the next time that check grows a third one.
	enrichStatus, _, _ := s.enrichPrereq()
	archiveStatus, _, _ := s.archivePrereq()

	config := map[string]any{
		"token":           token,
		"readOnly":        s.cfg.ReadOnly,
		"version":         s.cfg.Version,
		"enrichAvailable": enrichStatus == 0,
		// Configuration, not reachability: whether the upstream is up is a
		// question with a five-second answer, and blocking the page render on it
		// would make a sleeping NAS look like a broken app. The UI asks
		// /api/upstream for the rest.
		"upstreamConfigured": s.cfg.Origin != nil && s.cfg.Origin.HasUpstream(),
		// Both halves the evict handler itself checks, for the same reason the
		// enrichment flag calls its own prerequisite rather than restating it.
		"evictAvailable": s.cfg.AllowEvict && !s.cfg.ReadOnly,
		// Calls the prerequisite rather than restating its conditions, so this
		// cannot drift out of step with what the endpoint enforces.
		"archiveAvailable": archiveStatus == 0,
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return html
	}

	// Injected before any application script runs, so the client can read it
	// synchronously at startup rather than fetching it and racing.
	snippet := []byte("<script>window.__MM__=" + string(encoded) + ";</script>")

	if i := bytes.Index(html, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(html)+len(snippet))
		out = append(out, html[:i]...)
		out = append(out, snippet...)
		out = append(out, html[i:]...)
		return out
	}
	return append(snippet, html...)
}

func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".webmanifest":
		return "application/manifest+json"
	case ".woff2":
		return "font/woff2"
	}
	return ""
}
