// Package web serves the Admin CRM single-page application.
//
// Stack choice: the repository had no frontend and ships as a single Go binary
// managed by systemd. The CRM is therefore written as a dependency-free
// ES-module SPA embedded with go:embed. That keeps deployment exactly what it
// was — build one binary, restart one unit — with no Node toolchain on the
// server, no bundle to rebuild, and no CDN dependency at runtime.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var assets embed.FS

// Handler serves the SPA. Unknown paths fall back to index.html so client-side
// routing works on a hard refresh, while /api/ is left untouched for the API.
func Handler(basePath string) http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("web assets are missing: " + err.Error())
	}
	basePath = strings.TrimSuffix(basePath, "/")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The CRM is a private admin tool: it must never be cached by a shared
		// proxy and must never be embedded in another site's frame.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")

		path := strings.TrimPrefix(r.URL.Path, basePath)
		path = strings.TrimPrefix(path, "/")

		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			path = "index.html"
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}

		// ServeFileFS is used instead of http.FileServer because the latter
		// redirects any request for index.html back to the directory, which
		// turns the SPA fallback into a redirect loop.
		http.ServeFileFS(w, r, sub, path)
	})
}
