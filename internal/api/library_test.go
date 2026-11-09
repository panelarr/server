// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta/metatest"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

type libraryFixture struct {
	h      *harness
	admin  *client
	user   *client
	plugin *plugintest.Fake
}

// newLibraryFixture sets up a catalogue and the fake plugin; wrap, when
// given, serves the plugin through a wrapper (e.g. one that slows calls).
func newLibraryFixture(t *testing.T, wrap ...func(*plugintest.Fake) plugin.Plugin) libraryFixture {
	t.Helper()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(images.Close)
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 2})
	add := func(id, title, author string) {
		fake.AddEntry(&metav1.Entry{
			Id: id, Title: title, ContentRating: metadatav1.ContentRating_CONTENT_RATING_SAFE,
			Type:    metadatav1.EntryType_ENTRY_TYPE_MANGA,
			Titles:  []*metadatav1.Title{{Text: title, Language: "en", IsPrimary: true}, {Text: "Der Leuchtturm", Language: "de"}},
			Credits: []*metadatav1.Credit{{Person: &metadatav1.Person{Name: author}, Role: metadatav1.PersonRole_PERSON_ROLE_AUTHOR}},
			Images:  []*metadatav1.Image{{Kind: metadatav1.ImageKind_IMAGE_KIND_COVER, Url: images.URL + "/" + id + ".png"}},
		})
	}
	add("e1", "The Lantern Keeper", "A. Author")
	add("e2", "Salt and Iron", "Somebody Else")
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)
	plug := plugintest.New()
	var served plugin.Plugin = plug
	for _, w := range wrap {
		served = w(plug)
	}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": pluginstest.Serve(t, served, "k"), "api_key": "k"}, 201)
	user, _ := h.createUser(admin, "reader", nil)
	return libraryFixture{h: h, admin: admin, user: user, plugin: plug}
}

func waitWorkMatched(t *testing.T, c *client, id string) apigen.Work {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		w := call[apigen.Work](c, http.MethodGet, "/api/v1/works/"+id, nil, 200)
		done := len(w.Sources) > 0
		for _, s := range w.Sources {
			if s.Status == apigen.WorkSourceStateStatusPending || s.Status == apigen.WorkSourceStateStatusUnchecked {
				done = false
			}
		}
		if done {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("not matched: %+v", w.Sources)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestLibraryFlow(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	user, admin := f.user, f.admin

	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	if entry.Work.Title != "The Lantern Keeper" || entry.Work.EntryId != "e1" || entry.Work.CoverUrl == nil {
		t.Fatalf("entry %+v", entry)
	}
	again := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 200)
	if again.Work.Id != entry.Work.Id {
		t.Fatal("a second work for the same entry")
	}
	// Another user adding it shares the work.
	byAdmin := call[apigen.LibraryEntry](admin, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	if byAdmin.Work.Id != entry.Work.Id {
		t.Fatal("another user got another work")
	}
	user.expectProblem(http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "missing"}, 404, CodeNotFound)

	// The catalogue says which entries are in your library.
	found := call[apigen.CatalogSearchPage](user, http.MethodGet, "/api/v1/catalog/search?q=lantern", nil, 200)
	if len(found.Hits) != 1 || found.Hits[0].Entry.LibraryWorkId == nil || *found.Hits[0].Entry.LibraryWorkId != entry.Work.Id {
		t.Fatalf("search %+v", found)
	}
	if e := call[apigen.CatalogEntry](user, http.MethodGet, "/api/v1/catalog/entries/e1", nil, 200); e.LibraryWorkId == nil {
		t.Fatal("entry without library_work_id")
	}
	if e := call[apigen.CatalogEntry](user, http.MethodGet, "/api/v1/catalog/entries/e2", nil, 200); e.LibraryWorkId != nil {
		t.Fatal("entry not in the library has library_work_id")
	}

	w := waitWorkMatched(t, user, entry.Work.Id.String())
	if !w.InLibrary || w.AttachedSources != 2 {
		t.Fatalf("work %+v", w)
	}
	for _, s := range w.Sources {
		if s.Attached == nil || s.Attached.Evidence.TitleScore != 1 || s.Attached.CoverUrl == nil {
			t.Fatalf("source %s: %+v", s.Source.Id, s)
		}
		// Sources are admin-only, but a user sees the work's: with their
		// language, their series' chapter lists and their covers.
		if s.Source.Lang == "" {
			t.Fatalf("source %s without a language", s.Source.Id)
		}
		chs := call[apigen.SeriesChapterList](user, http.MethodGet, "/api/v1/works/"+entry.Work.Id.String()+"/series/"+s.Attached.Id.String()+"/chapters", nil, 200)
		if chs.Series.Id != s.Attached.Id {
			t.Fatalf("chapters of %s: %+v", s.Source.Id, chs.Series)
		}
		if res := user.do(http.MethodGet, *s.Attached.CoverUrl, nil); res.status != 200 {
			t.Fatalf("series cover %d", res.status)
		}
	}
	user.expectProblem(http.MethodGet, "/api/v1/sources", nil, 403, "forbidden")
	res := user.do(http.MethodGet, *entry.Work.CoverUrl, nil)
	if res.status != 200 || res.header.Get("Content-Type") != "image/png" {
		t.Fatalf("cover %d", res.status)
	}

	lib := call[apigen.LibraryPage](user, http.MethodGet, "/api/v1/library", nil, 200)
	if len(lib.Entries) != 1 || lib.Entries[0].Work.AttachedSources != 2 {
		t.Fatalf("library %+v", lib)
	}

	// Fixing matches needs the grant.
	user.expectProblem(http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/match", nil, 403, "grant_required")
	attached := w.Sources[0].Attached
	user.expectProblem(http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series/"+attached.Id.String()+"/detach", nil, 403, "grant_required")
	det := call[apigen.MatchSeries](admin, http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series/"+attached.Id.String()+"/detach", nil, 200)
	if det.State != apigen.MatchSeriesStateRejected {
		t.Fatalf("detached %+v", det)
	}
	re := call[apigen.MatchSeries](admin, http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series/"+attached.Id.String()+"/attach", nil, 200)
	if re.State != apigen.MatchSeriesStateAttached || re.Origin != apigen.MatchSeriesOriginManual {
		t.Fatalf("reattached %+v", re)
	}
	pr := call[apigen.MatchSeries](admin, http.MethodPatch, "/api/v1/works/"+entry.Work.Id.String()+"/series/"+attached.Id.String(), map[string]int{"priority": 5}, 200)
	if pr.Priority != 5 {
		t.Fatalf("priority %+v", pr)
	}
	byURL := call[apigen.MatchSeries](admin, http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series", map[string]string{"url": "https://example-comics.example/series/2"}, 201)
	if byURL.Origin != apigen.MatchSeriesOriginUrl || byURL.SourceId != "fake:1001" || byURL.Evidence.BestEffort == nil || *byURL.Evidence.BestEffort {
		t.Fatalf("by url %+v", byURL)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series", map[string]string{"url": "https://nowhere.example/x"}, 404, "url_unknown")
	admin.expectProblem(http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/series", map[string]string{"source_id": "fake:1001"}, 400, CodeInvalidRequest)
	task := call[apigen.TaskRef](admin, http.MethodPost, "/api/v1/works/"+entry.Work.Id.String()+"/match", map[string]bool{"force": true}, 202)
	if task.TaskId.String() == "" {
		t.Fatal("no task")
	}

	call[struct{}](user, http.MethodDelete, "/api/v1/library/"+entry.Work.Id.String(), nil, 204)
	user.expectProblem(http.MethodDelete, "/api/v1/library/"+entry.Work.Id.String(), nil, 404, "not_in_library")
	if w := call[apigen.Work](user, http.MethodGet, "/api/v1/works/"+entry.Work.Id.String(), nil, 200); w.InLibrary {
		t.Fatal("still in library")
	}
}

func TestAddFromSourceAPI(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	res := call[apigen.AddFromSourceResult](f.user, http.MethodPost, "/api/v1/library/from-source", map[string]string{"source_id": "fake:1001", "key": "/series/1"}, 200)
	if !res.Added || res.Entry == nil || res.Entry.Work.EntryId != "e1" {
		t.Fatalf("clear %+v", res)
	}
	res = call[apigen.AddFromSourceResult](f.user, http.MethodPost, "/api/v1/library/from-source", map[string]string{"source_id": "fake:1001", "key": "/series/2"}, 200)
	if res.Added || len(res.Candidates) != 1 || res.Candidates[0].Entry.Id != "e2" || res.Candidates[0].AuthorsMatch == nil {
		t.Fatalf("unclear %+v", res)
	}
	// Pick the candidate, attaching the series.
	picked := call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e2", "source": map[string]string{"source_id": "fake:1001", "key": "/series/2"}}, 201)
	w := call[apigen.Work](f.user, http.MethodGet, "/api/v1/works/"+picked.Work.Id.String(), nil, 200)
	found := false
	for _, s := range w.Sources {
		if s.Attached != nil && s.Attached.Key == "/series/2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("series not attached: %+v", w.Sources)
	}

	// Without may_add.
	noAdd, _ := f.h.createUser(f.admin, "viewer", map[string]any{"may_add": false})
	noAdd.expectProblem(http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 403, "grant_required")
	noAdd.expectProblem(http.MethodPost, "/api/v1/library/from-source", map[string]string{"source_id": "fake:1001", "key": "/series/1"}, 403, "grant_required")
}

// searchingResolver stands for a plugin whose sources can't resolve URLs
// natively but that finds the series by searching the site for the URL and
// says it only guessed (best effort).
type searchingResolver struct{ *plugintest.Fake }

func (r searchingResolver) ResolveUrl(_ context.Context, req *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	if req.GetUrl() != "https://example-comics.example/?q=salt" {
		return nil, plugin.ErrNotFound("nothing found")
	}
	return &sourcev1.ResolveUrlResponse{SourceId: "1001", Series: r.Sources[0].Series[1].Series, BestEffort: true}, nil
}

func TestAttachURLBestEffort(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t, func(fk *plugintest.Fake) plugin.Plugin {
		for _, s := range fk.Sources {
			s.Source.Capabilities = slices.DeleteFunc(s.Source.Capabilities, func(c sourcev1.SourceCapability) bool {
				return c == sourcev1.SourceCapability_SOURCE_CAPABILITY_RESOLVE_URL
			})
		}
		return searchingResolver{fk}
	})
	entry := call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, f.user, id)

	// No source resolves URLs natively, yet the plugin is asked and its
	// guess attached, flagged as one and scored by its title.
	se := call[apigen.MatchSeries](f.admin, http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"url": "https://example-comics.example/?q=salt"}, 201)
	ev := se.Evidence
	if se.Origin != apigen.MatchSeriesOriginUrl || se.Title != "Salt and Iron" || ev.BestEffort == nil || !*ev.BestEffort ||
		ev.TitleScore >= 1 || se.Score != ev.TitleScore || ev.EntryTitle == nil || *ev.EntryTitle != "The Lantern Keeper" {
		t.Fatalf("attached %+v", se)
	}
	// The work's sources say so too.
	w := call[apigen.Work](f.user, http.MethodGet, "/api/v1/works/"+id, nil, 200)
	for _, s := range w.Sources {
		if s.Source.Id == "fake:1001" && (s.Attached == nil || s.Attached.Id != se.Id || s.Attached.Evidence.BestEffort == nil || !*s.Attached.Evidence.BestEffort) {
			t.Fatalf("work source %+v", s)
		}
	}
	f.admin.expectProblem(http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"url": "https://nowhere.example/x"}, 404, "url_unknown")
}
