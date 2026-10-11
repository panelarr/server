// SPDX-License-Identifier: GPL-3.0-or-later

package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func app() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>app</title>")},
		"assets/app-1a2b.js":   {Data: []byte("console.log(1)")},
		"sw.js":                {Data: []byte("self.addEventListener('fetch',()=>{})")},
		"manifest.webmanifest": {Data: []byte("{}")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	}
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestRouting(t *testing.T) {
	h := Handler(app())
	for _, tc := range []struct {
		path, wantBody, wantType, wantCache string
		code                                int
	}{
		{"/", "<!doctype html><title>app</title>", "text/html; charset=utf-8", "no-cache", 200},
		{"/library/123", "<!doctype html><title>app</title>", "text/html; charset=utf-8", "no-cache", 200},
		{"/assets/app-1a2b.js", "console.log(1)", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 200},
		{"/sw.js", "self.addEventListener('fetch',()=>{})", "text/javascript; charset=utf-8", "no-cache", 200},
		{"/manifest.webmanifest", "{}", "application/manifest+json", "no-cache", 200},
		{"/assets/gone-9z.js", "", "", "", 404},
		{"/api/v2/whatever", "", "", "", 404},
		{"/opds/v1.2/catalog", "", "", "", 404},
		{"/healthz", "", "", "", 404},
		{"/../../etc/passwd", "<!doctype html><title>app</title>", "text/html; charset=utf-8", "no-cache", 200},
	} {
		rec := get(h, http.MethodGet, tc.path)
		if rec.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.path, rec.Code, tc.code)
			continue
		}
		if tc.code != 200 {
			continue
		}
		if rec.Body.String() != tc.wantBody || rec.Header().Get("Content-Type") != tc.wantType || rec.Header().Get("Cache-Control") != tc.wantCache {
			t.Errorf("%s: %q %q %q", tc.path, rec.Body.String(), rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
		}
		for k, v := range securityHeaders {
			if rec.Header().Get(k) != v {
				t.Errorf("%s: %s = %q", tc.path, k, rec.Header().Get(k))
			}
		}
	}
	if rec := get(h, http.MethodHead, "/"); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", rec.Code, rec.Body.Len())
	}
	if rec := get(h, http.MethodPost, "/"); rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func TestEmbeddedPlaceholder(t *testing.T) {
	files, err := Files("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Files(t.TempDir()); err == nil {
		t.Fatal("a directory without index.html accepted")
	}
	_ = IsPlaceholder(files) // the placeholder, or a web build when one was copied in
}
