// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

func TestImageRetrySettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": pluginstest.Serve(t, plugintest.New(), pluginKey), "api_key": pluginKey}, 201)
	const pkg = "/api/v1/plugins/fake/packages/example.comics/image-retries"
	const src = "/api/v1/sources/fake:1001/image-retries"
	get := func(path string) apigen.ImageRetries {
		return call[apigen.ImageRetries](admin, http.MethodGet, path, nil, 200)
	}
	put := func(path string, ms ...int) apigen.ImageRetries {
		return call[apigen.ImageRetries](admin, http.MethodPut, path, map[string][]int{"delays_ms": append([]int{}, ms...)}, 200)
	}
	is := func(r apigen.ImageRetries, level apigen.ImageRetriesLevel, from string, ms ...int) bool {
		return r.Level == level && slices.Equal(r.Effective.DelaysMs, append([]int{}, ms...)) && r.Effective.Attempts == len(ms) && deref(r.FromTargetId) == from
	}

	// Nothing set: the default.
	if r := get(src); r.Own != nil || r.TargetKind != apigen.ImageRetriesTargetKindSource || r.TargetId != "fake:1001" || !is(r, apigen.ImageRetriesLevelDefault, "", 1000, 3000) {
		t.Fatalf("default %+v", r)
	}
	if r := get(pkg); r.Own != nil || r.TargetId != "fake:example.comics" || !is(r, apigen.ImageRetriesLevelDefault, "", 1000, 3000) {
		t.Fatalf("package default %+v", r)
	}

	// The package's setting is its sources'; other packages' sources keep the default.
	if r := put(pkg, 500, 2000, 4000); r.Own == nil || !slices.Equal(r.Own.DelaysMs, []int{500, 2000, 4000}) || !is(r, apigen.ImageRetriesLevelPackage, "fake:example.comics", 500, 2000, 4000) {
		t.Fatalf("package set %+v", r)
	}
	if r := get(src); r.Own != nil || !is(r, apigen.ImageRetriesLevelPackage, "fake:example.comics", 500, 2000, 4000) {
		t.Fatalf("source from package %+v", r)
	}
	if r := get("/api/v1/sources/fake:2001/image-retries"); !is(r, apigen.ImageRetriesLevelDefault, "", 1000, 3000) {
		t.Fatalf("other package's source %+v", r)
	}

	// A source's own setting wins; [] is off, unlike unset.
	if r := put(src); r.Own == nil || len(r.Own.DelaysMs) != 0 || !is(r, apigen.ImageRetriesLevelSource, "fake:1001") {
		t.Fatalf("source off %+v", r)
	}
	call[struct{}](admin, http.MethodDelete, src, nil, 204)
	if r := get(src); r.Own != nil || !is(r, apigen.ImageRetriesLevelPackage, "fake:example.comics", 500, 2000, 4000) {
		t.Fatalf("source cleared %+v", r)
	}
	call[struct{}](admin, http.MethodDelete, pkg, nil, 204)
	call[struct{}](admin, http.MethodDelete, pkg, nil, 204)
	if r := get(src); !is(r, apigen.ImageRetriesLevelDefault, "", 1000, 3000) {
		t.Fatalf("package cleared %+v", r)
	}

	// Bounds, and targets that don't exist.
	admin.expectProblem(http.MethodPut, src, map[string][]int{"delays_ms": {1, 2, 3, 4, 5, 6}}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPut, src, map[string][]int{"delays_ms": {30001}}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPut, src, map[string][]int{"delays_ms": {-1}}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:9999/image-retries", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodPut, "/api/v1/plugins/nope/packages/example.comics/image-retries", map[string][]int{"delays_ms": {}}, 404, CodeNotFound)
	admin.expectProblem(http.MethodDelete, "/api/v1/sources/fake:9999/image-retries", nil, 404, CodeNotFound)
}
