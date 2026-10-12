// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// sourceIconFake gives its sources icons: a path on the plugin's address
// for one, an absolute URL elsewhere for the other.
type sourceIconFake struct {
	*plugintest.Fake
	elsewhere string
}

func (f *sourceIconFake) ListSources(ctx context.Context, r *sourcev1.ListSourcesRequest) (*sourcev1.ListSourcesResponse, error) {
	res, err := f.Fake.ListSources(ctx, r)
	for _, s := range res.GetSources() {
		switch s.GetId() {
		case "1001":
			s.IconUrl = "/v1/sources/1001/icon"
		case "2001":
			s.IconUrl = f.elsewhere + "/2001.png"
		}
	}
	return res, err
}

func TestSourceIcons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()

	var elsewhereAuth atomic.Value
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(elsewhere.Close)
	var broken atomic.Bool
	var hits atomic.Int64
	icons := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pluginKey {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		if broken.Load() {
			http.Error(w, "not yet", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	fake := &sourceIconFake{Fake: plugintest.New(), elsewhere: elsewhere.URL}
	u := pluginstest.ServeWithRoutes(t, fake, pluginKey, map[string]http.Handler{"/v1/sources/1001/icon": icons})
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)

	byID := map[string]string{}
	for _, s := range call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources", nil, 200).Sources {
		if s.IconUrl == nil || !strings.Contains(*s.IconUrl, "kind=icon") {
			t.Fatalf("%s icon %v", s.Id, s.IconUrl)
		}
		byID[s.Id] = *s.IconUrl
	}

	// A failing plugin isn't remembered: once it's fixed, the icon comes.
	broken.Store(true)
	admin.expectProblem(http.MethodGet, byID["fake:1001"], nil, 502, "icon_failed")
	broken.Store(false)
	for range 2 {
		res := admin.do(http.MethodGet, byID["fake:1001"], nil)
		if res.status != 200 || res.header.Get("Content-Type") != "image/png" || !bytes.Equal(res.body, pngBytes) {
			t.Fatalf("relative icon %d", res.status)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("%d fetches, want 2 (one failed, then cached)", hits.Load())
	}

	// Elsewhere: fetched without the plugin's key.
	if res := admin.do(http.MethodGet, byID["fake:2001"], nil); res.status != 200 {
		t.Fatalf("absolute icon %d", res.status)
	}
	if got, _ := elsewhereAuth.Load().(string); got != "" {
		t.Fatalf("the plugin's key went elsewhere: %q", got)
	}

	// An absent source's icon still shows on the admin's list; users don't
	// get it.
	if _, err := fake.UninstallPackage(context.Background(), &sourcev1.UninstallPackageRequest{PackageId: "example.comics"}); err != nil {
		t.Fatal(err)
	}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true&present=false", nil, 200); len(l.Sources) != 1 || l.Sources[0].Id != "fake:1001" {
		t.Fatalf("absent %+v", l)
	}
	if res := admin.do(http.MethodGet, byID["fake:1001"], nil); res.status != 200 {
		t.Fatalf("icon of an absent source %d", res.status)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/popular", nil, 404, CodeNotFound)
	user, _ := h.createUser(admin, "reader", nil)
	user.expectProblem(http.MethodGet, byID["fake:1001"], nil, 404, CodeNotFound)

	// An icon link isn't a cover link, nor the other way round.
	asCover := strings.Replace(byID["fake:1001"], "kind=icon&", "", 1)
	admin.expectProblem(http.MethodGet, asCover, nil, 403, "bad_signature")
}
