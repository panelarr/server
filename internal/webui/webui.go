// SPDX-License-Identifier: GPL-3.0-or-later

// Package webui serves the Panelarr web app next to the API, following the
// web app's embedding contract: files at the root, a single-page fallback
// to index.html (never for API, feed, asset or probe paths), long caching
// for content-hashed assets, and the app's security headers.
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
)

// dist holds the web app build (make web or make web-local put it there;
// only a .keep file is committed). placeholder is served without one.
//
//go:embed all:dist
var dist embed.FS

//go:embed placeholder
var placeholder embed.FS

// Security headers sent with every app response. They must match the web
// app's csp.config.mjs: its tests fail on any violation of this policy.
const (
	ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; manifest-src 'self'; " +
		"worker-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
)

var securityHeaders = map[string]string{
	"Content-Security-Policy": ContentSecurityPolicy,
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "no-referrer",
	"X-Frame-Options":         "DENY",
}

// noFallback are paths that never get the app's page: the API, feeds,
// assets (a missing script must 404, not turn into HTML) and probes.
var noFallback = []string{"/api/", "/opds/", "/assets/"}
var noFallbackExact = []string{"/api", "/opds", "/healthz", "/readyz", "/metrics"}

func init() {
	// Not every system's MIME table knows these.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

// Files returns the app's files: dir when set (a local build, for
// development), else the build embedded in the binary.
func Files(dir string) (fs.FS, error) {
	if dir != "" {
		if _, err := os.Stat(path.Join(dir, "index.html")); err != nil {
			return nil, err
		}
		return os.DirFS(dir), nil
	}
	if _, err := fs.Stat(dist, "dist/index.html"); err == nil {
		return fs.Sub(dist, "dist")
	}
	return fs.Sub(placeholder, "placeholder")
}

// IsPlaceholder reports whether files are the placeholder page, not a
// web app build.
func IsPlaceholder(files fs.FS) bool {
	b, err := fs.ReadFile(files, "index.html")
	return err == nil && strings.Contains(string(b), "data-panelarr-placeholder")
}

// Handler serves the app. Mount it as the router's last resort.
func Handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		for _, x := range noFallbackExact {
			if p == x {
				http.NotFound(w, r)
				return
			}
		}
		name := strings.TrimPrefix(path.Clean("/"+p), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(files, name); err == nil && !st.IsDir() {
				serve(w, r, files, name)
				return
			}
		}
		for _, x := range noFallback {
			if strings.HasPrefix(p, x) {
				http.NotFound(w, r)
				return
			}
		}
		serve(w, r, files, "index.html")
	})
}

func serve(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	for k, v := range securityHeaders {
		h.Set(k, v)
	}
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	h.Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data) //nolint:gosec // the app's own files, sent with its CSP
}
