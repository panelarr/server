// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestReaderPagesAndProgress(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	user := f.user
	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	chs := waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool {
		return len(cs) == 3 && cs[0].Variants == 2 && cs[1].Variants == 2 && cs[2].Variants == 2
	})
	c1, c2, c3 := chs[0], chs[1], chs[2]

	pages := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages", nil, 200)
	if len(pages.Pages) != 4 || pages.PrevChapterId != nil || pages.NextChapterId == nil || *pages.NextChapterId != c2.Id ||
		pages.Origin != apigen.ChapterPagesOriginSource || pages.WorkId != entry.Work.Id || pages.Progress != nil {
		t.Fatalf("pages %+v", pages)
	}
	res := user.do(http.MethodGet, pages.Pages[0].Url, nil)
	if res.status != 200 || res.header.Get("Content-Type") != "image/png" || len(res.body) == 0 {
		t.Fatalf("page image %d %v", res.status, res.header)
	}
	user.expectProblem(http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages/9", nil, 404, CodeNotFound)
	last := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+c3.Id.String()+"/pages", nil, 200)
	if last.NextChapterId != nil || last.PrevChapterId == nil || *last.PrevChapterId != c2.Id {
		t.Fatalf("last chapter neighbours %+v", last)
	}

	// Reading a specific copy.
	detail := call[apigen.ChapterDetail](user, http.MethodGet, "/api/v1/works/"+id+"/chapters/"+c1.Id.String(), nil, 200)
	var other apigen.ChapterVariant
	for _, v := range detail.Variants {
		if v.Id != detail.Chapter.Chosen.Id {
			other = v
		}
	}
	alt := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages?variant_id="+other.Id.String(), nil, 200)
	if alt.Variant.Id != other.Id || !strings.Contains(alt.Pages[0].Url, "variant_id="+other.Id.String()) {
		t.Fatalf("alt copy %+v", alt)
	}
	if res := user.do(http.MethodGet, alt.Pages[3].Url, nil); res.status != 200 {
		t.Fatalf("alt page %d", res.status)
	}

	// A user who may only use the German source reads the German copy.
	limited, _ := f.h.createUser(f.admin, "limited", map[string]any{"all_sources": false, "allowed_sources": []string{"fake:2001"}})
	lp := call[apigen.ChapterPages](limited, http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages", nil, 200)
	if lp.Variant.SourceId != "fake:2001" {
		t.Fatalf("limited reads %s", lp.Variant.SourceId)
	}
	var english apigen.ChapterVariant
	for _, v := range detail.Variants {
		if v.SourceId == "fake:1001" {
			english = v
		}
	}
	limited.expectProblem(http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages?variant_id="+english.Id.String(), nil, 404, CodeNotFound)

	// Progress: the last page marks a chapter read; continue moves on.
	p := call[apigen.ChapterProgress](user, http.MethodPut, "/api/v1/chapters/"+c1.Id.String()+"/progress", map[string]int{"page": 1, "pages": 4}, 200)
	if p.Completed || p.Page != 1 {
		t.Fatalf("progress %+v", p)
	}
	wp := call[apigen.WorkProgress](user, http.MethodGet, "/api/v1/works/"+id+"/progress", nil, 200)
	if wp.ContinueChapterId == nil || *wp.ContinueChapterId != c1.Id {
		t.Fatalf("continue %+v", wp)
	}
	p = call[apigen.ChapterProgress](user, http.MethodPut, "/api/v1/chapters/"+c1.Id.String()+"/progress", map[string]int{"page": 3, "pages": 4}, 200)
	if !p.Completed {
		t.Fatal("last page didn't finish the chapter")
	}
	wp = call[apigen.WorkProgress](user, http.MethodGet, "/api/v1/works/"+id+"/progress", nil, 200)
	if *wp.ContinueChapterId != c2.Id || len(wp.Chapters) != 1 {
		t.Fatalf("continue after finishing %+v", wp)
	}
	again := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+c1.Id.String()+"/pages", nil, 200)
	if again.Progress == nil || again.Progress.Page != 3 {
		t.Fatalf("pages progress %+v", again.Progress)
	}

	// Mark read and unread.
	call[struct{}](user, http.MethodPost, "/api/v1/works/"+id+"/progress", map[string]any{"chapter_ids": []string{c2.Id.String(), c3.Id.String()}, "read": true}, 204)
	wp = call[apigen.WorkProgress](user, http.MethodGet, "/api/v1/works/"+id+"/progress", nil, 200)
	if len(wp.Chapters) != 3 || wp.ContinueChapterId != nil {
		t.Fatalf("all read %+v", wp)
	}
	call[struct{}](user, http.MethodPost, "/api/v1/works/"+id+"/progress", map[string]any{"chapter_ids": []string{c3.Id.String()}, "read": false}, 204)
	if wp = call[apigen.WorkProgress](user, http.MethodGet, "/api/v1/works/"+id+"/progress", nil, 200); len(wp.Chapters) != 2 {
		t.Fatalf("after unread %+v", wp)
	}
	user.expectProblem(http.MethodPost, "/api/v1/works/"+id+"/progress", map[string]any{"chapter_ids": []string{"00000000-0000-0000-0000-000000000009"}, "read": true}, 404, CodeNotFound)

	// History, newest first, paged.
	h := call[apigen.HistoryPage](user, http.MethodGet, "/api/v1/me/history?page_size=1", nil, 200)
	if len(h.Items) != 1 || h.NextPageToken == "" || h.Items[0].WorkTitle != "The Lantern Keeper" {
		t.Fatalf("history %+v", h)
	}
	h2 := call[apigen.HistoryPage](user, http.MethodGet, "/api/v1/me/history?page_size=5&page_token="+h.NextPageToken, nil, 200)
	if len(h2.Items) != 1 || h2.Items[0].ChapterId == h.Items[0].ChapterId {
		t.Fatalf("history page 2 %+v", h2)
	}
	// Other users don't see it.
	if oh := call[apigen.HistoryPage](f.admin, http.MethodGet, "/api/v1/me/history", nil, 200); len(oh.Items) != 0 {
		t.Fatal("history leaks between users")
	}
}

func TestReaderSettingsAndPreferences(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, _ = h.setupAdmin()
	admin := h.login("admin", adminPassword)
	work := "11111111-1111-1111-1111-111111111111"

	call[struct{}](admin, http.MethodPut, "/api/v1/me/reader-settings/global", map[string]any{"mode": "paged", "zoom": 1}, 204)
	call[struct{}](admin, http.MethodPut, "/api/v1/me/reader-settings/source:fake:1001", map[string]any{"zoom": 2}, 204)
	call[struct{}](admin, http.MethodPut, "/api/v1/me/reader-settings/work:"+work, map[string]any{"direction": "rtl", "zoom": 3}, 204)
	st := call[apigen.ReaderSettings](admin, http.MethodGet, "/api/v1/me/reader-settings?source_id=fake:1001&work_id="+work, nil, 200)
	if st.Effective["mode"] != "paged" || st.Effective["zoom"] != 3.0 || st.Effective["direction"] != "rtl" || st.Source == nil || st.Work == nil {
		t.Fatalf("settings %+v", st)
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/me/reader-settings/work:"+work, nil, 204)
	st = call[apigen.ReaderSettings](admin, http.MethodGet, "/api/v1/me/reader-settings?source_id=fake:1001&work_id="+work, nil, 200)
	if st.Effective["zoom"] != 2.0 || st.Work != nil {
		t.Fatalf("after delete %+v", st)
	}
	admin.expectProblem(http.MethodDelete, "/api/v1/me/reader-settings/work:"+work, nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodPut, "/api/v1/me/reader-settings/everything", map[string]any{}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPut, "/api/v1/me/reader-settings/global", map[string]any{"big": strings.Repeat("x", 20000)}, 413, "too_large")
	// Settings are per user.
	other, _ := h.createUser(admin, "reader", nil)
	if s := call[apigen.ReaderSettings](other, http.MethodGet, "/api/v1/me/reader-settings", nil, 200); len(s.Effective) != 0 {
		t.Fatalf("settings leak %+v", s)
	}

	if p := call[map[string]any](admin, http.MethodGet, "/api/v1/me/preferences", nil, 200); len(p) != 0 {
		t.Fatalf("prefs %+v", p)
	}
	call[struct{}](admin, http.MethodPut, "/api/v1/me/preferences", map[string]any{"theme": "true-black", "accent": "#ff0066"}, 204)
	if p := call[map[string]any](admin, http.MethodGet, "/api/v1/me/preferences", nil, 200); p["theme"] != "true-black" {
		t.Fatalf("prefs %+v", p)
	}
}

func TestBookmarks(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	user := f.user
	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, user, id)
	chs := waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) == 3 })
	c1, c2, c3 := chs[0].Id, chs[1].Id, chs[2].Id
	base := "/api/v1/works/" + id + "/bookmarks"

	if l := call[apigen.BookmarkList](user, http.MethodGet, base, nil, 200); len(l.ChapterIds) != 0 {
		t.Fatalf("empty %+v", l)
	}
	// One, then a batch (again for the first: no harm).
	call[apigen.BookmarkList](user, http.MethodPost, base, map[string]any{"chapter_ids": []string{c2.String()}, "bookmarked": true}, 200)
	l := call[apigen.BookmarkList](user, http.MethodPost, base, map[string]any{"chapter_ids": []string{c2.String(), c3.String()}, "bookmarked": true}, 200)
	if len(l.ChapterIds) != 2 || l.ChapterIds[0] != c2 || l.ChapterIds[1] != c3 {
		t.Fatalf("bookmarked %+v", l)
	}

	// The chapter list shows and filters them, in either order.
	list := call[apigen.ChapterList](user, http.MethodGet, "/api/v1/works/"+id+"/chapters", nil, 200)
	if list.Chapters[0].Bookmarked || !list.Chapters[1].Bookmarked || !list.Chapters[2].Bookmarked {
		t.Fatalf("flags %+v", list.Chapters)
	}
	only := call[apigen.ChapterList](user, http.MethodGet, "/api/v1/works/"+id+"/chapters?bookmarked=true&order=desc", nil, 200)
	if len(only.Chapters) != 2 || only.Chapters[0].Id != c3 || only.Chapters[1].Id != c2 {
		t.Fatalf("bookmarked only %+v", only.Chapters)
	}
	if rest := call[apigen.ChapterList](user, http.MethodGet, "/api/v1/works/"+id+"/chapters?bookmarked=false", nil, 200); len(rest.Chapters) != 1 || rest.Chapters[0].Id != c1 {
		t.Fatalf("not bookmarked %+v", rest.Chapters)
	}
	if d := call[apigen.ChapterDetail](user, http.MethodGet, "/api/v1/works/"+id+"/chapters/"+c3.String(), nil, 200); !d.Chapter.Bookmarked {
		t.Fatal("detail without the bookmark")
	}
	if p := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+c2.String()+"/pages", nil, 200); !p.Chapter.Bookmarked {
		t.Fatal("reader without the bookmark")
	}
	// The library counts them.
	lib := call[apigen.LibraryPage](user, http.MethodGet, "/api/v1/library", nil, 200)
	if len(lib.Entries) != 1 || lib.Entries[0].Bookmarks != 2 {
		t.Fatalf("library %+v", lib.Entries)
	}

	// Bookmarks are each user's own.
	f.admin.expectProblem(http.MethodPost, base, map[string]any{"chapter_ids": []string{"00000000-0000-0000-0000-000000000001"}, "bookmarked": true}, 404, CodeNotFound)
	if l := call[apigen.BookmarkList](f.admin, http.MethodGet, base, nil, 200); len(l.ChapterIds) != 0 {
		t.Fatalf("someone else's bookmarks %+v", l)
	}

	// Other devices hear about it; removing works in batches too.
	got := readEvents(t, user, 2, func() {
		call[apigen.BookmarkList](user, http.MethodPost, base, map[string]any{"chapter_ids": []string{c2.String(), c3.String()}, "bookmarked": false}, 200)
	})
	if len(got) < 2 || got[1] != "bookmarks.changed" {
		t.Fatalf("events %v", got)
	}
	if l := call[apigen.BookmarkList](user, http.MethodGet, base, nil, 200); len(l.ChapterIds) != 0 {
		t.Fatalf("after removing %+v", l)
	}
	user.expectProblem(http.MethodPost, base, map[string]any{"chapter_ids": []string{}, "bookmarked": true}, 400, CodeInvalidRequest)
}

// slowImages takes a while per image and records how many run at once.
type slowImages struct {
	*plugintest.Fake
	inFlight, most atomic.Int64
}

func (f *slowImages) FetchImage(ctx context.Context, r *sourcev1.FetchImageRequest, send plugin.ImageSendFunc) error {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.most.Load()
		if n <= m || f.most.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	return f.Fake.FetchImage(ctx, r, send)
}

func TestPageImagesUseTheImageBudget(t *testing.T) {
	t.Parallel()
	var slow *slowImages
	f := newLibraryFixture(t, func(fk *plugintest.Fake) plugin.Plugin {
		slow = &slowImages{Fake: fk}
		return slow
	})
	user := f.user
	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, user, id)
	chs := waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) == 3 })
	pages := call[apigen.ChapterPages](user, http.MethodGet, "/api/v1/chapters/"+chs[0].Id.String()+"/pages", nil, 200)

	// The plugin declares no image rate and 4 images at a time per source
	// (its API rate, 10 a second, would take 2 s for 20 images).
	start := time.Now()
	var wg sync.WaitGroup
	failed := atomic.Int64{}
	for i := range 20 {
		wg.Go(func() {
			if res := user.do(http.MethodGet, pages.Pages[i%len(pages.Pages)].Url, nil); res.status != 200 {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	took := time.Since(start)
	if failed.Load() != 0 {
		t.Fatalf("%d page fetches failed", failed.Load())
	}
	if most := slow.most.Load(); most > 4 || most < 2 {
		t.Fatalf("%d images at once, want 2 to 4", most)
	}
	// 20 images, 4 at a time, 100 ms each: about half a second, no pacing.
	if took > 1500*time.Millisecond {
		t.Fatalf("20 images took %s", took)
	}
}
