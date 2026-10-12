// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

const pluginKey = "plugin-key"

func TestPluginsAdmin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	u := pluginstest.Serve(t, plugintest.New(), pluginKey)

	p := admin.expectProblem(http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": "wrong"}, 422, "plugin_unreachable")
	if p.Detail == nil || *p.Detail == "" {
		t.Fatalf("no reason: %+v", p)
	}
	pl := call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)
	if pl.Id != "fake" || pl.Health != apigen.PluginHealthOk || pl.SourceCount != 2 || pl.HasConfig || !strings.Contains(strings.Join(pl.Contracts, ","), "panelarr.source.v1") {
		t.Fatalf("plugin %+v", pl)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 409, "plugin_id_taken")
	admin.expectProblem(http.MethodPost, "/api/v1/plugins", map[string]string{"id": "Bad ID", "url": u}, 400, CodeInvalidRequest)
	if strings.Contains(string(admin.do(http.MethodGet, "/api/v1/plugins", nil).body), pluginKey) {
		t.Fatal("the key is shown")
	}
	list := call[apigen.PluginList](admin, http.MethodGet, "/api/v1/plugins", nil, 200)
	if len(list.Plugins) != 1 {
		t.Fatalf("list %+v", list)
	}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	admin.expectProblem(http.MethodGet, "/api/v1/plugins/fake/config", nil, 404, "no_config")

	// Turning it off hides its sources from everyone.
	call[apigen.Plugin](admin, http.MethodPatch, "/api/v1/plugins/fake", map[string]any{"enabled": false}, 200)
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources", nil, 200); len(l.Sources) != 0 {
		t.Fatalf("sources of a disabled plugin: %d", len(l.Sources))
	}
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true", nil, 200); len(l.Sources) != 2 {
		t.Fatalf("all sources: %d", len(l.Sources))
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/plugins/fake", nil, 204)
	admin.expectProblem(http.MethodGet, "/api/v1/plugins/fake", nil, 404, CodeNotFound)
}

func TestSourcesBrowse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	u := pluginstest.Serve(t, plugintest.New(), pluginKey)
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)
	user, _ := h.createUser(admin, "reader", nil)
	limited, _ := h.createUser(admin, "limited", map[string]any{"all_sources": false, "allowed_sources": []string{"fake:2001"}})

	srcs := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources", nil, 200)
	if len(srcs.Sources) != 2 || srcs.Sources[0].Health.Status == "" || srcs.Sources[0].Lang == "" {
		t.Fatalf("sources %+v", srcs)
	}
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?lang=de", nil, 200); len(l.Sources) != 1 {
		t.Fatalf("lang filter %d", len(l.Sources))
	}

	pop := call[apigen.SourceSeriesPage](admin, http.MethodGet, "/api/v1/sources/fake:1001/popular?page=1", nil, 200)
	if len(pop.Series) == 0 || pop.Series[0].CoverUrl == nil {
		t.Fatalf("popular %+v", pop)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:2001/latest", nil, 422, "unsupported")

	found := call[apigen.SourceSeriesPage](admin, http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{"query": "lantern"}, 200)
	if len(found.Series) != 1 || found.Series[0].Title != "The Lantern Keeper" {
		t.Fatalf("search %+v", found)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{"filters": []any{map[string]any{"nope": 1}}}, 400, CodeInvalidRequest)
	// Searching only reads: read and admin scopes are enough, without write;
	// read alone isn't.
	readAdminKey := h.keyClient(admin.newAPIKey("read", "admin"))
	call[apigen.SourceSeriesPage](readAdminKey, http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{"query": "salt"}, 200)
	h.keyClient(admin.newAPIKey("read")).expectProblem(http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{"query": "salt"}, 403, "insufficient_scope")

	filters := call[apigen.SourceFilters](admin, http.MethodGet, "/api/v1/sources/fake:1001/filters", nil, 200)
	if len(filters.Filters) == 0 {
		t.Fatal("no filters")
	}

	key := url.QueryEscape(found.Series[0].Key)
	series := call[apigen.SourceSeries](admin, http.MethodGet, "/api/v1/sources/fake:1001/series?key="+key, nil, 200)
	if series.Title != "The Lantern Keeper" || len(series.Authors) != 1 || len(series.AltTitles) != 2 {
		t.Fatalf("series %+v", series)
	}
	chs := call[apigen.SourceChapterList](admin, http.MethodGet, "/api/v1/sources/fake:1001/chapters?key="+key, nil, 200)
	if len(chs.Chapters) != 3 || chs.Chapters[0].Number == nil || chs.Chapters[0].UploadedAt == nil {
		t.Fatalf("chapters %+v", chs)
	}

	// Sources are admin-only: users (whatever their grants) and their keys
	// can't list or browse them.
	userKey := h.keyClient(user.newAPIKey("read", "write"))
	for _, c := range []*client{user, limited, userKey} {
		c.expectProblem(http.MethodGet, "/api/v1/sources", nil, 403, "forbidden")
		c.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/popular", nil, 403, "forbidden")
		c.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/latest", nil, 403, "forbidden")
		c.expectProblem(http.MethodPost, "/api/v1/sources/fake:1001/search", map[string]any{"query": "salt"}, 403, "forbidden")
		c.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/filters", nil, 403, "forbidden")
		c.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/series?key="+key, nil, 403, "forbidden")
		c.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/chapters?key="+key, nil, 403, "forbidden")
	}

	// Covers come through the server, signed, and stay open to users within
	// their grants (a work's series show them).
	res := user.do(http.MethodGet, *series.CoverUrl, nil)
	if res.status != 200 || res.header.Get("Content-Type") != "image/png" || len(res.body) == 0 {
		t.Fatalf("cover %d %v", res.status, res.header)
	}
	tampered := strings.Replace(*series.CoverUrl, "ref=", "ref=x", 1)
	user.expectProblem(http.MethodGet, tampered, nil, 403, "bad_signature")
	limited.expectProblem(http.MethodGet, *series.CoverUrl, nil, 404, CodeNotFound)

	// A disabled source can't be browsed, even by admins, until it's back.
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": false}, 200)
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/popular", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, *series.CoverUrl, nil, 404, CodeNotFound)
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true", nil, 200); len(l.Sources) != 2 {
		t.Fatalf("all sources %d", len(l.Sources))
	}
	user.expectProblem(http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": true}, 403, "forbidden")
	admin.expectProblem(http.MethodPatch, "/api/v1/sources/fake:9999", map[string]any{"enabled": true}, 404, CodeNotFound)
}
