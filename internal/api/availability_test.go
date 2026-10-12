// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"testing"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestListSourcesFilters(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	admin := f.admin
	list := func(query string) []string {
		var ids []string
		for _, s := range call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources"+query, nil, 200).Sources {
			ids = append(ids, s.Id)
		}
		return ids
	}
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": false}, 200)
	if got := list(""); len(got) != 1 || got[0] != "fake:2001" {
		t.Fatalf("usable %v", got)
	}
	if got := list("?all=true&enabled=false"); len(got) != 1 || got[0] != "fake:1001" {
		t.Fatalf("turned off %v", got)
	}
	if got := list("?all=true&plugin_id=fake&present=true"); len(got) != 2 {
		t.Fatalf("present %v", got)
	}
	if got := list("?all=true&plugin_id=other"); len(got) != 0 {
		t.Fatalf("other plugin %v", got)
	}
	if got := list("?all=true&lang=en"); len(got) != 1 || got[0] != "fake:1001" {
		t.Fatalf("lang %v", got)
	}
}

func TestWorkSourceSearch(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	admin := f.admin
	fixer, _ := f.h.createUser(admin, "fixer", map[string]any{"may_manage_matches": true})
	limited, _ := f.h.createUser(admin, "limited", map[string]any{"may_manage_matches": true, "all_sources": false, "allowed_sources": []string{"fake:2001"}})
	entry := call[apigen.LibraryEntry](fixer, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	call[apigen.LibraryEntry](limited, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	w := waitWorkMatched(t, fixer, id)
	var attached apigen.MatchSeries
	for _, s := range w.Sources {
		if s.Source.Id == "fake:1001" && s.Attached != nil {
			attached = *s.Attached
		}
	}
	search := "/api/v1/works/" + id + "/sources/fake:1001/search"

	// A user who may fix matches searches the work's source; results the
	// work already knows say so.
	page := call[apigen.SourceSeriesPage](fixer, http.MethodPost, search, map[string]any{"query": "lantern"}, 200)
	if len(page.Series) != 1 || page.Series[0].Match == nil || page.Series[0].Match.State != apigen.SeriesMatchRefStateAttached || page.Series[0].Match.SeriesId != attached.Id {
		t.Fatalf("lantern %+v", page)
	}
	other := call[apigen.SourceSeriesPage](fixer, http.MethodPost, search, map[string]any{"query": "salt"}, 200)
	if len(other.Series) != 1 || (other.Series[0].Match != nil && other.Series[0].Match.State == apigen.SeriesMatchRefStateAttached) {
		t.Fatalf("salt %+v", other)
	}
	// Any result can be attached by its key.
	got := call[apigen.MatchSeries](fixer, http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"source_id": "fake:1001", "key": other.Series[0].Key}, 201)
	if got.State != apigen.MatchSeriesStateAttached {
		t.Fatalf("attached %+v", got)
	}
	call[apigen.SourceSeriesPage](admin, http.MethodPost, search, map[string]any{"query": "lantern"}, 200)
	call[apigen.SourceSeriesPage](h(fixer).keyClient(fixer.newAPIKey("read")), http.MethodPost, search, map[string]any{"query": "lantern"}, 200)

	fixer.expectProblem(http.MethodPost, search, map[string]any{}, 400, CodeInvalidRequest)
	f.user.expectProblem(http.MethodPost, search, map[string]any{"query": "lantern"}, 403, "grant_required")
	// A source outside the caller's grants, a source turned off, a
	// source that doesn't exist: 404 alike.
	limited.expectProblem(http.MethodPost, search, map[string]any{"query": "lantern"}, 404, CodeNotFound)
	call[apigen.SourceSeriesPage](limited, http.MethodPost, "/api/v1/works/"+id+"/sources/fake:2001/search", map[string]any{"query": "salz"}, 200)
	fixer.expectProblem(http.MethodPost, "/api/v1/works/"+id+"/sources/fake:9999/search", map[string]any{"query": "x"}, 404, CodeNotFound)
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": false}, 200)
	fixer.expectProblem(http.MethodPost, search, map[string]any{"query": "lantern"}, 404, CodeNotFound)
	admin.expectProblem(http.MethodPost, search, map[string]any{"query": "lantern"}, 404, CodeNotFound)
	fixer.expectProblem(http.MethodPost, "/api/v1/works/00000000-0000-0000-0000-000000000001/sources/fake:2001/search", map[string]any{"query": "x"}, 404, CodeNotFound)
}

func h(c *client) *harness { return c.h }

// chosenSources maps chapter numbers to the source of their chosen copy.
func chosenSources(cs []apigen.UnifiedChapter) map[float32]string {
	out := map[float32]string{}
	for _, c := range cs {
		if c.Number != nil && c.Chosen != nil {
			out[*c.Number] = c.Chosen.SourceId
		}
	}
	return out
}

func allChosenOn(source string) func([]apigen.UnifiedChapter) bool {
	return func(cs []apigen.UnifiedChapter) bool {
		got := chosenSources(cs)
		if len(cs) != 3 || len(got) != 3 {
			return false
		}
		for _, s := range got {
			if s != source {
				return false
			}
		}
		return true
	}
}

func TestCopiesFollowSourceAvailability(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	admin := f.admin
	ctx := context.Background()
	entry := call[apigen.LibraryEntry](admin, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, admin, id)
	// Both sources list chapters 1–3; the first source's copies win on its
	// order.
	waitChapters(t, admin, id, func(cs []apigen.UnifiedChapter) bool {
		return len(cs) == 3 && cs[0].Variants == 2 && cs[2].Variants == 2 && allChosenOn("fake:1001")(cs)
	})

	// Turned off: the other source's copies take over, and come back.
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": false}, 200)
	chs := waitChapters(t, admin, id, allChosenOn("fake:2001"))
	if chs[0].Reason != "healthier_source" {
		t.Fatalf("reason %q", chs[0].Reason)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"url": "https://example-comics.example/series/2"}, 404, "url_unknown")
	// Matching skips it.
	salt := call[apigen.LibraryEntry](admin, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e2"}, 201)
	waitWorkMatched(t, admin, salt.Work.Id.String())
	matches, err := f.h.st.ListWorkSourceMatches(ctx, salt.Work.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if m.SourceID == "fake:1001" {
			t.Fatal("matched on a source turned off")
		}
	}
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": true}, 200)
	waitChapters(t, admin, id, allChosenOn("fake:1001"))

	// No longer listed (its package uninstalled): absent, the work keeps
	// its series and chapters, and the other copies serve meanwhile.
	if _, err := f.plugin.UninstallPackage(ctx, &sourcev1.UninstallPackageRequest{PackageId: "example.comics"}); err != nil {
		t.Fatal(err)
	}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	if l := call[apigen.SourceList](admin, http.MethodGet, "/api/v1/sources?all=true&present=false", nil, 200); len(l.Sources) != 1 || l.Sources[0].Id != "fake:1001" {
		t.Fatalf("absent %+v", l)
	}
	waitChapters(t, admin, id, allChosenOn("fake:2001"))
	// Not even admins browse a source no longer listed.
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:1001/popular", nil, 404, CodeNotFound)
	series, err := f.h.st.ListSourceSeries(ctx, entry.Work.Id)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, se := range series {
		kept = kept || (se.SourceID == "fake:1001" && se.State == "attached")
	}
	if !kept {
		t.Fatal("the absent source's series was dropped")
	}
	// Checking it for new chapters is skipped, not failed.
	call[apigen.TaskRef](admin, http.MethodPost, "/api/v1/works/"+id+"/refresh", nil, 202)

	// Listed again: its copies come back.
	if _, err := f.plugin.InstallPackage(ctx, &sourcev1.InstallPackageRequest{PackageId: "example.comics"}); err != nil {
		t.Fatal(err)
	}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	waitChapters(t, admin, id, allChosenOn("fake:1001"))
}
