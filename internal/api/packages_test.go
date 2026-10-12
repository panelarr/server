// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

func packageIDs(l apigen.PackageList) []string {
	var ids []string
	for _, p := range l.Packages {
		ids = append(ids, p.Id)
	}
	sort.Strings(ids)
	return ids
}

func sourceIDs(l apigen.SourceList) []string {
	var ids []string
	for _, s := range l.Sources {
		ids = append(ids, s.Id)
	}
	sort.Strings(ids)
	return ids
}

func TestPackagesAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	u := pluginstest.Serve(t, plugintest.New(), pluginKey)
	pl := call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)
	if !slices.Contains(pl.Capabilities, "packages") {
		t.Fatalf("capabilities %v", pl.Capabilities)
	}
	const base = "/api/v1/plugins/fake/packages"

	// What the plugin offers, with what is installed and updatable.
	all := call[apigen.PackageList](admin, http.MethodGet, base, nil, 200)
	if got := packageIDs(all); !slices.Equal(got, []string{"beispiel.comics", "example.comics", "multi.comics"}) {
		t.Fatalf("packages %v", got)
	}
	ex := call[apigen.Package](admin, http.MethodGet, base+"/example.comics", nil, 200)
	if !ex.Installed || ex.InstalledVersion == nil || *ex.InstalledVersion != "1.0" || !ex.UpdateAvailable || ex.Version != "1.1" ||
		len(ex.Offers) != 1 || len(ex.Sources) != 1 || ex.Sources[0].Id != "fake:1001" || !ex.Sources[0].Available || ex.ContentRating != "safe" {
		t.Fatalf("example %+v", ex)
	}
	if got := packageIDs(call[apigen.PackageList](admin, http.MethodGet, base+"?installed=true", nil, 200)); len(got) != 2 {
		t.Fatalf("installed %v", got)
	}
	if got := packageIDs(call[apigen.PackageList](admin, http.MethodGet, base+"?updates=true", nil, 200)); !slices.Equal(got, []string{"example.comics"}) {
		t.Fatalf("updates %v", got)
	}
	if got := packageIDs(call[apigen.PackageList](admin, http.MethodGet, base+"?lang=fr", nil, 200)); !slices.Equal(got, []string{"multi.comics"}) {
		t.Fatalf("lang %v", got)
	}
	if got := packageIDs(call[apigen.PackageList](admin, http.MethodGet, base+"?q=beispiel", nil, 200)); !slices.Equal(got, []string{"beispiel.comics"}) {
		t.Fatalf("query %v", got)
	}
	admin.expectProblem(http.MethodGet, base+"/nope.pkg", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodPost, base+"/nope.pkg/install", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodGet, base+"/bad:id", nil, 400, CodeInvalidRequest)

	// Sources say which package provides them.
	srcs := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?package_id=example.comics", nil, 200)
	if len(srcs.Sources) != 1 || srcs.Sources[0].PackageName == nil || *srcs.Sources[0].PackageName != "Example Comics" {
		t.Fatalf("source package %+v", srcs)
	}

	// Install: a task; its sources appear once it's done.
	call[apigen.QueueState](admin, http.MethodPatch, "/api/v1/queue/kinds/plugin.package", map[string]bool{"paused": true}, 200)
	ref := call[apigen.TaskRef](admin, http.MethodPost, base+"/multi.comics/install", map[string]string{"origin_id": "store-b"}, 202)
	if again := call[apigen.TaskRef](admin, http.MethodPost, base+"/multi.comics/install", nil, 202); again.TaskId != ref.TaskId {
		t.Fatal("the same install was queued twice")
	}
	pending := call[apigen.Package](admin, http.MethodGet, base+"/multi.comics", nil, 200)
	if pending.TaskId == nil || *pending.TaskId != ref.TaskId {
		t.Fatalf("pending %+v", pending)
	}
	task := call[apigen.Task](admin, http.MethodGet, "/api/v1/tasks/"+ref.TaskId.String(), nil, 200)
	if task.Kind != "plugin.package" || task.Payload["plugin_id"] != "fake" || task.Payload["package_id"] != "multi.comics" || task.Payload["action"] != "install" {
		t.Fatalf("task %+v", task)
	}
	call[apigen.QueueState](admin, http.MethodPatch, "/api/v1/queue/kinds/plugin.package", map[string]bool{"paused": false}, 200)
	waitTask(t, admin, ref.TaskId.String(), apigen.TaskStateSucceeded)
	multi := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?package_id=multi.comics", nil, 200)
	if got := sourceIDs(multi); !slices.Equal(got, []string{"fake:3001", "fake:3002", "fake:3003"}) {
		t.Fatalf("installed sources %v", got)
	}
	if p := call[apigen.Package](admin, http.MethodGet, base+"/multi.comics", nil, 200); !p.Installed || p.InstalledVersion == nil || *p.InstalledVersion != "3.1" || p.TaskId != nil {
		t.Fatalf("installed %+v", p)
	}

	// Languages decide which of its sources exist.
	p := call[apigen.Package](admin, http.MethodPut, base+"/multi.comics/languages", map[string]any{"enabled_langs": []string{"fr"}}, 200)
	if !slices.Equal(p.EnabledLangs, []string{"fr"}) {
		t.Fatalf("languages %+v", p)
	}
	if got := sourceIDs(call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?package_id=multi.comics", nil, 200)); !slices.Equal(got, []string{"fake:3002"}) {
		t.Fatalf("sources in fr %v", got)
	}
	off := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true&present=false", nil, 200)
	if got := sourceIDs(off); !slices.Equal(got, []string{"fake:3001", "fake:3003"}) {
		t.Fatalf("absent %v", got)
	}
	// Not gone: not available, its language being off.
	for _, s := range off.Sources {
		if s.UnavailableReason == nil || *s.UnavailableReason != apigen.SourceInfoUnavailableReasonLanguage {
			t.Fatalf("%s: reason %v", s.Id, s.UnavailableReason)
		}
	}
	call[apigen.Package](admin, http.MethodPut, base+"/multi.comics/languages", map[string]any{"enabled_langs": []string{}}, 200)
	back := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?package_id=multi.comics", nil, 200)
	if got := sourceIDs(back); len(got) != 3 {
		t.Fatalf("all languages again %v", got)
	}
	for _, s := range back.Sources {
		if s.UnavailableReason != nil {
			t.Fatalf("%s still unavailable", s.Id)
		}
	}

	// Update and uninstall.
	up := call[apigen.TaskRef](admin, http.MethodPost, base+"/example.comics/update", nil, 202)
	waitTask(t, admin, up.TaskId.String(), apigen.TaskStateSucceeded)
	if p := call[apigen.Package](admin, http.MethodGet, base+"/example.comics", nil, 200); p.InstalledVersion == nil || *p.InstalledVersion != "1.1" || p.UpdateAvailable {
		t.Fatalf("updated %+v", p)
	}
	un := call[apigen.TaskRef](admin, http.MethodDelete, base+"/beispiel.comics", nil, 202)
	waitTask(t, admin, un.TaskId.String(), apigen.TaskStateSucceeded)
	gone := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true&present=false", nil, 200)
	if got := sourceIDs(gone); !slices.Equal(got, []string{"fake:2001"}) || gone.Sources[0].UnavailableReason != nil {
		t.Fatalf("uninstalled %+v", gone)
	}

	// A plugin turned off can't change packages.
	call[apigen.Plugin](admin, http.MethodPatch, "/api/v1/plugins/fake", map[string]any{"enabled": false}, 200)
	admin.expectProblem(http.MethodGet, base, nil, 409, "plugin_disabled")
	admin.expectProblem(http.MethodPost, base+"/beispiel.comics/install", nil, 409, "plugin_disabled")
}

func TestSingleSitePluginHasNoPackages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	single := plugintest.New()
	single.ID, single.Packages = "single", nil
	u := pluginstest.Serve(t, single, pluginKey)
	pl := call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)
	if slices.Contains(pl.Capabilities, "packages") {
		t.Fatalf("capabilities %v", pl.Capabilities)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/plugins/single/packages", nil, 422, "no_packages")
	admin.expectProblem(http.MethodPost, "/api/v1/plugins/single/packages/x/install", nil, 422, "no_packages")
	admin.expectProblem(http.MethodGet, "/api/v1/plugins/nope/packages", nil, 404, CodeNotFound)
}
