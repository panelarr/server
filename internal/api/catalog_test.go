// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta/metatest"
)

// A 1×1 PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func TestMetaConnectionWizard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	fake := metatest.New(t)
	fake.AddKey("reader", "catalog:read")

	conn := call[apigen.MetaConnection](admin, http.MethodGet, "/api/v1/system/meta", nil, 200)
	if conn.Check.Configured || conn.Url != nil || conn.Check.Problem == nil {
		t.Fatalf("fresh: %+v", conn)
	}
	setup := call[apigen.SetupStatus](admin, http.MethodGet, "/api/v1/setup", nil, 200)
	if setup.MetaConfigured || setup.MetaReady {
		t.Fatalf("setup %+v", setup)
	}
	if res := admin.do(http.MethodGet, "/readyz", nil); res.status != 200 || !strings.Contains(string(res.body), `"degraded"`) {
		t.Fatalf("readyz without meta: %d %s", res.status, res.body)
	}

	// A key without the admin scope is refused, with what to fix.
	p := admin.expectProblem(http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": "reader"}, 422, "meta_check_failed")
	if p.Detail == nil || !strings.Contains(*p.Detail, "admin scope") {
		t.Fatalf("problem %+v", p)
	}
	admin.expectProblem(http.MethodPut, "/api/v1/system/meta", map[string]string{"url": "not a url", "api_key": "x"}, 400, CodeInvalidRequest)

	// Connected but no provider data: saved, not ready, and says why.
	conn = call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)
	if !conn.Check.AdminScope || conn.Check.Ready || conn.Url == nil || *conn.Url != fake.URL || !strings.Contains(*conn.Check.Problem, "provider") {
		t.Fatalf("connected: %+v", conn)
	}
	if strings.Contains(string(admin.do(http.MethodGet, "/api/v1/system/meta", nil).body), metatest.AdminKey) {
		t.Fatal("the key is shown")
	}
	setup = call[apigen.SetupStatus](admin, http.MethodGet, "/api/v1/setup", nil, 200)
	if !setup.MetaConfigured || setup.MetaReady {
		t.Fatalf("setup %+v", setup)
	}

	// Start the provider's first sync from the wizard.
	fake.SetProviders(map[string]int64{"books": 0})
	started := call[apigen.MetaSyncStarted](admin, http.MethodPost, "/api/v1/system/meta/providers/books/sync", nil, 202)
	if started.RunId == "" || len(fake.Syncs()) != 1 || fake.Syncs()[0].GetKind() != "dump" {
		t.Fatalf("sync %+v %v", started, fake.Syncs())
	}
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/providers/nope/sync", map[string]string{"kind": "delta"}, 404, CodeNotFound)

	fake.SetProviders(map[string]int64{"books": 100})
	conn = call[apigen.MetaConnection](admin, http.MethodPost, "/api/v1/system/meta/check", nil, 200)
	if !conn.Check.Ready || conn.Check.Problem != nil || len(conn.Check.Providers) != 1 || conn.Check.Providers[0].Records != 100 {
		t.Fatalf("ready: %+v", conn)
	}
	if res := admin.do(http.MethodGet, "/readyz", nil); !strings.Contains(string(res.body), `"status":"ok"`) {
		t.Fatalf("readyz with meta: %s", res.body)
	}
}

type catalogFixture struct {
	h         *harness
	admin     *client
	user      *client
	fake      *metatest.Fake
	coverHits *atomic.Int32
}

func newCatalogFixture(t *testing.T) catalogFixture {
	t.Helper()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil) // ceiling: suggestive
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 3})
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)

	hits := new(atomic.Int32)
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, ".svg") {
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte("<svg onload=alert(1)>"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(images.Close)

	add := func(id, title string, rating metadatav1.ContentRating, coverPath string) {
		fake.AddEntry(&metav1.Entry{
			Id: id, Title: title, ContentRating: rating,
			Type: metadatav1.EntryType_ENTRY_TYPE_MANHWA, Status: metadatav1.EntryStatus_ENTRY_STATUS_RELEASING,
			Titles: []*metadatav1.Title{
				{Text: title, Language: "en", Kind: metadatav1.TitleKind_TITLE_KIND_OFFICIAL, IsPrimary: true},
				{Text: "Native " + title, Language: "ko", Kind: metadatav1.TitleKind_TITLE_KIND_NATIVE},
			},
			Descriptions: []*metadatav1.Description{{Language: "en", Text: "A made-up story.", Format: metadatav1.DescriptionFormat_DESCRIPTION_FORMAT_MARKDOWN}},
			Credits:      []*metadatav1.Credit{{Person: &metadatav1.Person{Id: "p1", Name: "Some Author"}, Role: metadatav1.PersonRole_PERSON_ROLE_AUTHOR}},
			Tags: []*metadatav1.TagAssignment{
				{Tag: &metadatav1.Tag{Name: "Drama", IsGenre: true}},
				{Tag: &metadatav1.Tag{Name: "Big reveal", IsSpoiler: true}},
			},
			Dates:       &metadatav1.Dates{Start: &metadatav1.PartialDate{Year: 2019}},
			ExternalIds: []*metadatav1.ExternalId{{Provider: "books", Id: "123"}},
			Images:      []*metadatav1.Image{{Kind: metadatav1.ImageKind_IMAGE_KIND_COVER, Url: images.URL + coverPath, Width: 300, Height: 450, Blurhash: "LEHV6n"}},
			Sources:     []*metav1.EntrySource{{Attribution: &metadatav1.Attribution{ProviderName: "Books DB", License: "CC BY 4.0"}}},
		})
	}
	add("e1", "Gentle Story", metadatav1.ContentRating_CONTENT_RATING_SAFE, "/e1.png")
	add("e2", "Spicy Story", metadatav1.ContentRating_CONTENT_RATING_EROTIC, "/e2.png")
	add("e3", "Vector Story", metadatav1.ContentRating_CONTENT_RATING_SUGGESTIVE, "/e3.svg")
	return catalogFixture{h: h, admin: admin, user: user, fake: fake, coverHits: hits}
}

func TestCatalog(t *testing.T) {
	t.Parallel()
	f := newCatalogFixture(t)
	user, admin := f.user, f.admin

	// The user's ceiling filters search and browse.
	page := call[apigen.CatalogSearchPage](user, http.MethodGet, "/api/v1/catalog/search?q=story&type=manhwa", nil, 200)
	var ids []string
	for _, h := range page.Hits {
		ids = append(ids, h.Entry.Id)
	}
	if strings.Join(ids, ",") != "e1,e3" || page.NoProviders {
		t.Fatalf("user hits %v", ids)
	}
	first := page.Hits[0].Entry
	if first.Cover == nil || first.Cover.Url != "/api/v1/catalog/entries/e1/cover" || first.Type == nil || *first.Type != "manhwa" ||
		first.ContentRating != apigen.EntryContentRatingSafe || first.Year == nil || *first.Year != 2019 {
		t.Fatalf("summary %+v", first)
	}
	if all := call[apigen.CatalogSearchPage](admin, http.MethodGet, "/api/v1/catalog/search?q=story", nil, 200); len(all.Hits) != 3 {
		t.Fatalf("admin sees %d", len(all.Hits))
	}
	browse := call[apigen.CatalogPage](user, http.MethodGet, "/api/v1/catalog/browse?sort=popularity&status=releasing", nil, 200)
	if len(browse.Entries) != 2 {
		t.Fatalf("browse %d", len(browse.Entries))
	}

	// Bad filters.
	user.expectProblem(http.MethodGet, "/api/v1/catalog/search", nil, 400, CodeInvalidRequest) // q is required
	p := user.expectProblem(http.MethodGet, "/api/v1/catalog/browse?type=scroll", nil, 400, CodeInvalidRequest)
	if (*p.Errors)[0].Field != "type" {
		t.Fatalf("errors %+v", *p.Errors)
	}
	user.expectProblem(http.MethodGet, "/api/v1/catalog/browse?sort=random", nil, 400, CodeInvalidRequest)

	// Details.
	e := call[apigen.CatalogEntry](user, http.MethodGet, "/api/v1/catalog/entries/e1", nil, 200)
	if len(e.Titles) != 2 || e.Titles[1].Kind != "native" || e.Credits[0].Role != "author" || e.Credits[0].Name != "Some Author" ||
		strings.Join(e.Genres, ",") != "Drama" || !e.Tags[1].Spoiler || e.Descriptions[0].Format != apigen.CatalogDescriptionFormatMarkdown ||
		len(e.Attribution) != 1 || e.Attribution[0].Provider != "Books DB" || len(e.ExternalIds) != 1 {
		t.Fatalf("entry %+v", e)
	}
	user.expectProblem(http.MethodGet, "/api/v1/catalog/entries/e2", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, "/api/v1/catalog/entries/nope", nil, 404, CodeNotFound)
	call[apigen.CatalogEntry](admin, http.MethodGet, "/api/v1/catalog/entries/e2", nil, 200)
}

func TestCatalogCovers(t *testing.T) {
	t.Parallel()
	f := newCatalogFixture(t)
	user := f.user

	for range 2 {
		res := user.do(http.MethodGet, "/api/v1/catalog/entries/e1/cover", nil)
		if res.status != 200 || res.header.Get("Content-Type") != "image/png" || !bytes.Equal(res.body, pngBytes) ||
			!strings.Contains(res.header.Get("Cache-Control"), "private") {
			t.Fatalf("cover: %d %v", res.status, res.header)
		}
	}
	if n := f.coverHits.Load(); n != 1 {
		t.Fatalf("cover fetched %d times, want once (cached)", n)
	}
	// Above the ceiling, SVG (scripts) and anonymous callers get nothing.
	user.expectProblem(http.MethodGet, "/api/v1/catalog/entries/e2/cover", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, "/api/v1/catalog/entries/e3/cover", nil, 404, CodeNotFound)
	f.h.client().expectProblem(http.MethodGet, "/api/v1/catalog/entries/e1/cover", nil, 401, "unauthenticated")
}

func TestCatalogWithoutMeta(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	admin.expectProblem(http.MethodGet, "/api/v1/catalog/search?q=x", nil, 503, "meta_unavailable")

	f := newCatalogFixture(t)
	f.fake.SetDown(true)
	f.user.expectProblem(http.MethodGet, "/api/v1/catalog/search?q=x", nil, 503, "meta_unavailable")
}

func TestMetaErrorMapping(t *testing.T) {
	t.Parallel()
	f := newCatalogFixture(t)
	admin, user, fake := f.admin, f.user, f.fake
	fake.SetProviders(map[string]int64{"books": 3})

	// Meta is up; its provider plugin isn't. Say so, not "meta is down".
	fake.SetProviderDown("books", false)
	p := admin.expectProblem(http.MethodPost, "/api/v1/system/meta/providers/books/sync", nil, 502, "meta_provider_unavailable")
	if p.Detail == nil || !strings.Contains(*p.Detail, "'books'") || !strings.Contains(*p.Detail, "plugin is running") {
		t.Fatalf("detail %+v", p.Detail)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/providers/nope/sync", nil, 404, CodeNotFound)

	// Meta answering with other errors.
	fake.FailWith(connect.CodeInternal)
	user.expectProblem(http.MethodGet, "/api/v1/catalog/search?q=story", nil, 502, "meta_error")
	fake.FailWith(connect.CodePermissionDenied)
	user.expectProblem(http.MethodGet, "/api/v1/catalog/entries/e1", nil, 502, "meta_key_rejected")
	fake.FailWith(0)

	// Meta not answering at all.
	fake.SetDown(true)
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/providers/books/sync", nil, 503, "meta_unavailable")
	user.expectProblem(http.MethodGet, "/api/v1/catalog/browse", nil, 503, "meta_unavailable")
	// Not mistaken for a source problem when adding a work.
	user.expectProblem(http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 503, "meta_unavailable")
	conn := call[apigen.MetaConnection](admin, http.MethodPost, "/api/v1/system/meta/check", nil, 200)
	if conn.Check.Reachable || conn.Check.Problem == nil || !strings.Contains(*conn.Check.Problem, "Can't reach") {
		t.Fatalf("check %+v", conn.Check)
	}
}
