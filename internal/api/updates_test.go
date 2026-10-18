// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestUpdatesFeed(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	user, admin := f.user, f.admin
	id := call[apigen.LibraryEntry](user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201).Work.Id.String()
	waitWorkMatched(t, user, id)
	waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) >= 3 })
	feed := func(c *client, query string) apigen.UpdatesPage {
		return call[apigen.UpdatesPage](c, http.MethodGet, "/api/v1/me/updates"+query, nil, 200)
	}
	settled := func() apigen.UpdatesStatus {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			st := call[apigen.UpdatesStatus](user, http.MethodGet, "/api/v1/me/updates/status", nil, 200)
			if st.Checking == 0 {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("still checking %+v", st)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}

	// What the series had when first listed isn't news.
	if p := feed(user, ""); len(p.Items) != 0 {
		t.Fatalf("first chapters listed %+v", p.Items)
	}
	st := settled()
	if st.LastCheckedAt == nil || st.Failing != 0 {
		t.Fatalf("status %+v", st)
	}

	// A later check finds a chapter: it's in the feed.
	add := func(n float64) {
		en := f.plugin.Sources[0].Series[0]
		num := strconv.FormatFloat(n, 'f', -1, 64)
		en.Chapters = append([]*plugintest.Chapter{{Pages: 2, Chapter: &sourcev1.Chapter{Key: "/series/1/" + num, Name: "Chapter " + num + ": Rain", Number: &n}}}, en.Chapters...)
	}
	add(4)
	if r := call[apigen.RefreshQueued](user, http.MethodPost, "/api/v1/me/updates/refresh", nil, 202); r.Queued == 0 {
		t.Fatalf("refresh %+v", r)
	}
	waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) >= 4 })
	if st2 := settled(); !st2.LastCheckedAt.After(*st.LastCheckedAt) {
		t.Fatalf("not checked again %+v", st2)
	}
	p := feed(user, "")
	if len(p.Items) != 1 {
		t.Fatalf("feed %+v", p.Items)
	}
	u := p.Items[0]
	if u.WorkId.String() != id || u.WorkTitle != "The Lantern Keeper" || u.WorkCoverUrl == nil || u.ChapterNumber == nil || *u.ChapterNumber != 4 ||
		u.ChapterDisplayTitle != "Rain" || u.AppearedAt.IsZero() || !u.New || u.Read || u.Page != nil || u.DownloadState != apigen.UpdateItemDownloadStateNone {
		t.Fatalf("item %+v", u)
	}

	// Read state, and the "new" mark clears when the work's chapters are seen.
	call[apigen.ChapterProgress](user, http.MethodPut, "/api/v1/chapters/"+u.ChapterId.String()+"/progress", map[string]int{"page": 1, "pages": 2}, 200)
	call[struct{}](user, http.MethodPost, "/api/v1/library/"+id+"/seen", nil, 204)
	if u := feed(user, "").Items[0]; !u.Read || u.Page == nil || *u.Page != 1 || u.New {
		t.Fatalf("after reading %+v", u)
	}

	// Download state.
	call[apigen.DownloadsQueued](admin, http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]string{"scope": "all"}, 202)
	waitDownloads(t, admin, 4)
	if u := feed(user, "").Items[0]; u.DownloadState != apigen.UpdateItemDownloadStateDownloaded {
		t.Fatalf("after downloading %+v", u)
	}

	// Pages, newest first.
	add(5)
	add(6)
	call[apigen.RefreshQueued](user, http.MethodPost, "/api/v1/me/updates/refresh", nil, 202)
	waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) >= 6 })
	p = feed(user, "?page_size=2")
	if len(p.Items) != 2 || p.NextPageToken == "" || *p.Items[0].ChapterNumber == 4 || *p.Items[1].ChapterNumber == 4 {
		t.Fatalf("first page %+v", p)
	}
	if p2 := feed(user, "?page_size=2&page_token="+p.NextPageToken); len(p2.Items) != 1 || *p2.Items[0].ChapterNumber != 4 || p2.NextPageToken != "" {
		t.Fatalf("second page %+v", p2)
	}

	// Rebuilding the unified list doesn't announce again: a chapter made
	// anew from variants already seen stays out of the feed.
	ctx := context.Background()
	if _, err := f.h.st.Pool.Exec(ctx, "DELETE FROM chapters WHERE work_id = $1 AND number = 6", id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.chs.Unify(ctx, uuid.MustParse(id)); err != nil {
		t.Fatal(err)
	}
	waitChapters(t, user, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) >= 6 })
	if p := feed(user, ""); len(p.Items) != 2 || *p.Items[0].ChapterNumber != 5 {
		t.Fatalf("after rebuilding %+v", p.Items)
	}

	// Someone who adds the work later sees only what appeared after.
	other, _ := f.h.createUser(admin, "other", nil)
	if p := feed(other, ""); len(p.Items) != 0 {
		t.Fatalf("someone else's feed %+v", p.Items)
	}
	call[apigen.LibraryEntry](other, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	if p := feed(other, ""); len(p.Items) != 0 {
		t.Fatalf("what appeared before adding %+v", p.Items)
	}
	if r := call[apigen.RefreshQueued](other, http.MethodPost, "/api/v1/me/updates/refresh", nil, 202); r.Queued == 0 {
		t.Fatalf("other's refresh %+v", r)
	}

	// Works the user may no longer see leave the feed.
	me := call[apigen.Me](user, http.MethodGet, "/api/v1/me", nil, 200).User
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"grants": map[string]any{"all_sources": false, "allowed_sources": []string{}}}, 200)
	if p := feed(user, ""); len(p.Items) != 0 {
		t.Fatalf("hidden work's chapters %+v", p.Items)
	}
}
