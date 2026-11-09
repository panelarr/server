// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/store/db"
)

// waitChapters polls a work's chapters until ok accepts them.
func waitChapters(t *testing.T, c *client, workID string, ok func([]apigen.UnifiedChapter) bool) []apigen.UnifiedChapter {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		list := call[apigen.ChapterList](c, http.MethodGet, "/api/v1/works/"+workID+"/chapters", nil, 200)
		if ok(list.Chapters) {
			return list.Chapters
		}
		if time.Now().After(deadline) {
			tasks, _ := c.h.st.ListTasks(context.Background(), db.ListTasksParams{Lim: 50})
			for _, tk := range tasks {
				t.Logf("task %s %s attempts=%d run_after=%s err=%q payload=%s", tk.Kind, tk.State, tk.Attempts, time.Until(tk.RunAfter).Round(time.Second), tk.LastError, tk.Payload)
			}
			t.Fatalf("chapters never settled: %+v", list.Chapters)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func chapterNumbered(cs []apigen.UnifiedChapter, n float32) *apigen.UnifiedChapter {
	for i := range cs {
		if cs[i].Number != nil && *cs[i].Number == n {
			return &cs[i]
		}
	}
	return nil
}

func TestUnifiedChapters(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	user, admin := f.user, f.admin
	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, user, id)

	// Both sources have chapters 1–3: three chapters, two copies each.
	chs := waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool {
		return len(cs) == 3 && cs[0].Variants == 2 && cs[2].Variants == 2
	})
	for i, c := range chs {
		if c.Number == nil || *c.Number != float32(i+1) || c.Chosen == nil || c.Reason == "" || !c.New || !c.Available || c.DisplayTitle != "" {
			t.Fatalf("chapter %d: %+v", i, c)
		}
	}
	desc := call[apigen.ChapterList](user, http.MethodGet, "/api/v1/works/"+id+"/chapters?order=desc", nil, 200)
	if *desc.Chapters[0].Number != 3 {
		t.Fatal("desc order")
	}

	// New badges in the library until marked seen.
	lib := call[apigen.LibraryPage](user, http.MethodGet, "/api/v1/library", nil, 200)
	if lib.Entries[0].NewChapters != 3 {
		t.Fatalf("new chapters %d", lib.Entries[0].NewChapters)
	}
	call[struct{}](user, http.MethodPost, "/api/v1/library/"+id+"/seen", nil, 204)
	if lib := call[apigen.LibraryPage](user, http.MethodGet, "/api/v1/library", nil, 200); lib.Entries[0].NewChapters != 0 {
		t.Fatal("still new after seen")
	}

	// The per-source tab lists the source's chapters as it does.
	w := call[apigen.Work](user, http.MethodGet, "/api/v1/works/"+id, nil, 200)
	var deSeries apigen.MatchSeries
	for _, s := range w.Sources {
		if s.Source.Id == "fake:2001" {
			deSeries = *s.Attached
		}
	}
	tab := call[apigen.SeriesChapterList](user, http.MethodGet, "/api/v1/works/"+id+"/series/"+deSeries.Id.String()+"/chapters", nil, 200)
	if len(tab.Chapters) != 3 || tab.Chapters[0].Name != "Chapter 3" || tab.Chapters[0].ChapterId == nil || tab.CheckedAt == nil {
		t.Fatalf("tab %+v", tab)
	}

	// Pin the German copy of chapter 2.
	ch2 := chapterNumbered(chs, 2)
	detail := call[apigen.ChapterDetail](user, http.MethodGet, "/api/v1/works/"+id+"/chapters/"+ch2.Id.String(), nil, 200)
	if len(detail.Variants) != 2 {
		t.Fatalf("detail %+v", detail)
	}
	var german apigen.ChapterVariant
	for _, v := range detail.Variants {
		if v.SourceId == "fake:2001" {
			german = v
		}
	}
	user.expectProblem(http.MethodPut, "/api/v1/works/"+id+"/chapters/"+ch2.Id.String()+"/pin", map[string]any{"variant_id": german.Id}, 403, "grant_required")
	pinned := call[apigen.ChapterDetail](admin, http.MethodPut, "/api/v1/works/"+id+"/chapters/"+ch2.Id.String()+"/pin", map[string]any{"variant_id": german.Id}, 200)
	if !pinned.Chapter.Pinned || pinned.Chapter.Reason != "pinned" || pinned.Chapter.Chosen.Id != german.Id {
		t.Fatalf("pinned %+v", pinned.Chapter)
	}
	admin.expectProblem(http.MethodPut, "/api/v1/works/"+id+"/chapters/"+ch2.Id.String()+"/pin", map[string]any{"variant_id": chapterNumbered(chs, 1).Chosen.Id}, 400, CodeInvalidRequest)
	unpinned := call[apigen.ChapterDetail](admin, http.MethodPut, "/api/v1/works/"+id+"/chapters/"+ch2.Id.String()+"/pin", map[string]any{"variant_id": nil}, 200)
	if unpinned.Chapter.Pinned || unpinned.Chapter.Reason == "pinned" {
		t.Fatalf("unpinned %+v", unpinned.Chapter)
	}

	// The German source gets chapter 4 and an extra; the English source
	// drops chapter 1. A refresh picks it all up. Chapter 1 was started, so
	// it stays, unavailable (one nobody touched would go).
	call[apigen.ChapterProgress](user, http.MethodPut, "/api/v1/chapters/"+chapterNumbered(chs, 1).Id.String()+"/progress", map[string]int{"page": 1, "pages": 4}, 200)
	de := f.plugin.Sources[1].Series[0]
	four := 4.0
	de.Chapters = append([]*plugintest.Chapter{
		{Pages: 4, Chapter: &sourcev1.Chapter{Key: "/series/1/4", Name: "Chapter 4", Number: &four}},
		{Pages: 2, Chapter: &sourcev1.Chapter{Key: "/series/1/special", Name: "Beach Special"}},
	}, de.Chapters...)
	en := f.plugin.Sources[0].Series[0]
	en.Chapters = en.Chapters[:2] // drops "Chapter 1" (listed last)
	de.Chapters = de.Chapters[:4] // and the German "Chapter 1"
	if q := call[apigen.RefreshQueued](user, http.MethodPost, "/api/v1/works/"+id+"/refresh", nil, 202); q.Queued != 2 {
		t.Fatalf("queued %d", q.Queued)
	}
	chs = waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool {
		c1 := chapterNumbered(cs, 1)
		return len(cs) == 5 && chapterNumbered(cs, 4) != nil && c1 != nil && !c1.Available
	})
	c4 := chapterNumbered(chs, 4)
	if c4.Variants != 1 || c4.Reason != "only_source" || !c4.New || c4.Chosen.SourceId != "fake:2001" {
		t.Fatalf("chapter 4 %+v", c4)
	}
	extra := chs[len(chs)-1]
	if !extra.Extra || extra.Number != nil || extra.Title != "Beach Special" {
		t.Fatalf("extra %+v", extra)
	}
	if lib := call[apigen.LibraryPage](user, http.MethodGet, "/api/v1/library", nil, 200); lib.Entries[0].NewChapters != 2 {
		t.Fatalf("new after refresh %d", lib.Entries[0].NewChapters)
	}
}

func TestUnifyAllReadsNamesAgain(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t, func(fk *plugintest.Fake) plugin.Plugin {
		fk.Sources[0].Series[0].Chapters[0].Chapter.Name = "Vol. 2 Chapter 3: The Last Lantern"
		return fk
	})
	entry := call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, f.user, id)
	waitChapters(t, f.user, id, func(cs []apigen.UnifiedChapter) bool {
		c := chapterNumbered(cs, 3)
		return c != nil && c.Volume != nil && *c.Volume == "2"
	})

	// As if stored by an older reading of the names.
	ctx := context.Background()
	if _, err := f.h.st.Pool.Exec(ctx, `UPDATE chapters SET volume = '' WHERE work_id = $1`, entry.Work.Id); err != nil {
		t.Fatal(err)
	}
	if n, err := f.h.chs.UnifyAll(ctx); err != nil || n != 1 {
		t.Fatalf("unify all: %d %v", n, err)
	}
	c := chapterNumbered(call[apigen.ChapterList](f.user, http.MethodGet, "/api/v1/works/"+id+"/chapters", nil, 200).Chapters, 3)
	if c == nil || c.Volume == nil || *c.Volume != "2" || c.DisplayTitle != "The Last Lantern" {
		t.Fatalf("chapter 3 %+v", c)
	}
}

func TestReattachRemovesGhostChapters(t *testing.T) {
	t.Parallel()
	// The English series the matcher attaches has chapters 4–6 (the German
	// one 1–3); the other English series have 1–3.
	f := newLibraryFixture(t, func(fk *plugintest.Fake) plugin.Plugin {
		for i, c := range fk.Sources[0].Series[0].Chapters {
			n := float64(6 - i)
			c.Chapter.Number, c.Chapter.Name = &n, fmt.Sprintf("Chapter %v", n)
		}
		return fk
	})
	user, admin := f.user, f.admin
	entry := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	waitWorkMatched(t, user, id)
	chs := waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) == 6 })
	c4, c5, c6 := chapterNumbered(chs, 4), chapterNumbered(chs, 5), chapterNumbered(chs, 6)

	// The user started chapter 5 and bookmarked chapter 6; nobody touched 4.
	call[apigen.ChapterProgress](user, http.MethodPut, "/api/v1/chapters/"+c5.Id.String()+"/progress", map[string]int{"page": 1, "pages": 4}, 200)
	call[apigen.BookmarkList](user, http.MethodPost, "/api/v1/works/"+id+"/bookmarks", map[string]any{"chapter_ids": []string{c6.Id.String()}, "bookmarked": true}, 200)

	// Replacing the English series with another one (chapters 1–3): chapter
	// 4 only the old series had goes; 5 and 6 stay, unavailable.
	call[apigen.MatchSeries](admin, http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"source_id": "fake:1001", "key": "/series/2"}, 201)
	chs = waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool {
		c1 := chapterNumbered(cs, 1) // once the new series' chapters are in
		return chapterNumbered(cs, 4) == nil && len(cs) == 5 && c1 != nil && c1.Variants == 2
	})
	for _, c := range []*apigen.UnifiedChapter{c5, c6} {
		got := chapterNumbered(chs, *c.Number)
		if got == nil || got.Id != c.Id || got.Available {
			t.Fatalf("chapter %v: %+v", *c.Number, got)
		}
	}
	for n := float32(1); n <= 3; n++ {
		if c := chapterNumbered(chs, n); c == nil || !c.Available || c.Variants != 2 {
			t.Fatalf("chapter %v: %+v", n, c)
		}
	}
	user.expectProblem(http.MethodGet, "/api/v1/works/"+id+"/chapters/"+c4.Id.String(), nil, 404, CodeNotFound)

	// Attaching the old series again brings its chapters back.
	call[apigen.MatchSeries](admin, http.MethodPost, "/api/v1/works/"+id+"/series", map[string]string{"source_id": "fake:1001", "key": "/series/1"}, 201)
	waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool {
		back4, back5 := chapterNumbered(cs, 4), chapterNumbered(cs, 5)
		return back4 != nil && back4.Available && back5 != nil && back5.Available && back5.Id == c5.Id
	})
}
