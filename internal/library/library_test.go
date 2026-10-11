// SPDX-License-Identifier: GPL-3.0-or-later

package library_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/meta/metatest"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
	"github.com/panelarr/server/internal/users"
)

type fixture struct {
	st   *store.Store
	lib  *library.Service
	fake *plugintest.Fake
	meta *metatest.Fake
	user library.Viewer
}

func entry(id, title string, rating metadatav1.ContentRating, author string, alts ...*metadatav1.Title) *metav1.Entry {
	titles := append([]*metadatav1.Title{{Text: title, Language: "en", IsPrimary: true, Kind: metadatav1.TitleKind_TITLE_KIND_OFFICIAL}}, alts...)
	return &metav1.Entry{
		Id: id, Title: title, ContentRating: rating, Type: metadatav1.EntryType_ENTRY_TYPE_MANGA,
		Titles:  titles,
		Credits: []*metadatav1.Credit{{Person: &metadatav1.Person{Name: author}, Role: metadatav1.PersonRole_PERSON_ROLE_AUTHOR}},
		Dates:   &metadatav1.Dates{Start: &metadatav1.PartialDate{Year: 2020}},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	box := secret.NewBox("", filepath.Join(t.TempDir(), "secret.key"))

	mf := metatest.New(t)
	mf.SetProviders(map[string]int64{"books": 3})
	mf.AddEntry(entry("e1", "The Lantern Keeper", metadatav1.ContentRating_CONTENT_RATING_SAFE, "A. Author",
		&metadatav1.Title{Text: "Der Leuchtturm", Language: "de"},
		&metadatav1.Title{Text: "Toudai Mori", Language: "ja-Latn", Kind: metadatav1.TitleKind_TITLE_KIND_ROMANIZED}))
	mf.AddEntry(entry("e2", "Salt and Iron", metadatav1.ContentRating_CONTENT_RATING_SAFE, "Somebody Else"))
	mf.AddEntry(entry("e3", "Spicy Thing", metadatav1.ContentRating_CONTENT_RATING_EROTIC, "X"))
	m := meta.NewService(st, box, meta.Conn{URL: mf.URL, APIKey: metatest.AdminKey}, nil)
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}

	fake := plugintest.New()
	host := plugins.NewHost(st, box, plugins.RemoteDialer, nil)
	if _, err := host.Register(ctx, plugins.NewPlugin{URL: pluginstest.Serve(t, fake, "k"), APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	lim := limits.NewRegistry()
	q := queue.New(st, events.NewHub(), lim, nil, nil)
	q.Poll = 20 * time.Millisecond
	lib := library.NewService(st, m, host, q, lim, events.NewHub(), nil)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })

	u, err := st.CreateUser(ctx, db.CreateUserParams{Username: "reader", PasswordHash: "x", Role: "user", MayAdd: true,
		ContentRatingCeiling: "suggestive", AllSources: true, AllowedSources: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, lib: lib, fake: fake, meta: mf, user: library.Viewer{UserID: u.ID, Grants: users.GrantsOf(u)}}
}

// waitMatched waits until every source of a work has a final status.
func waitMatched(t *testing.T, f *fixture, workID uuid.UUID) library.Work {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		w, err := f.lib.Get(context.Background(), f.user, workID)
		if err != nil {
			t.Fatal(err)
		}
		done := len(w.Sources) > 0
		for _, s := range w.Sources {
			if s.Status == library.MatchPending || s.Status == "unchecked" {
				done = false
			}
		}
		if done {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("matching didn't finish: %+v", w.Sources)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func sourceOf(t *testing.T, w library.Work, id string) library.WorkSource {
	t.Helper()
	for _, s := range w.Sources {
		if s.Source.ID == id {
			return s
		}
	}
	t.Fatalf("no source %s in %+v", id, w.Sources)
	return library.WorkSource{}
}

func TestAddAndAutoMatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	added, err := f.lib.Add(ctx, f.user, "e1", nil)
	if err != nil || !added.New || added.Work.Title != "The Lantern Keeper" || added.Work.ContentRating != "safe" {
		t.Fatalf("add: %v %+v", err, added)
	}
	// Adding again (or by someone else) reuses the work.
	again, err := f.lib.Add(ctx, f.user, "e1", nil)
	if err != nil || again.New || again.Work.ID != added.Work.ID {
		t.Fatalf("again: %v %+v", err, again)
	}

	w := waitMatched(t, f, added.Work.ID)
	en := sourceOf(t, w, "fake:1001")
	if en.Status != library.MatchMatched || en.Attached == nil || en.Attached.Title != "The Lantern Keeper" || en.Attached.Origin != library.OriginAuto {
		t.Fatalf("english source: %+v", en)
	}
	ev := library.EvidenceOf(*en.Attached)
	if ev.TitleScore != 1 || ev.AuthorsMatch == nil || !*ev.AuthorsMatch || ev.ChapterCount == nil || *ev.ChapterCount != 3 {
		t.Fatalf("evidence %+v", ev)
	}
	// The German source has it under its German title: the main title
	// finds nothing, the alternative does.
	de := sourceOf(t, w, "fake:2001")
	if de.Status != library.MatchMatched || de.Attached == nil || de.Attached.Title != "Der Leuchtturm" || len(de.Queries) != 2 {
		t.Fatalf("german source: %+v", de)
	}
	if !w.InLibrary {
		t.Fatal("not in library")
	}
}

func TestCandidatesAndFixes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// The exact title but another author: a candidate, not attached.
	added, err := f.lib.Add(ctx, f.user, "e2", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := waitMatched(t, f, added.Work.ID)
	en := sourceOf(t, w, "fake:1001")
	if en.Status != library.MatchCandidates || en.Attached != nil || len(en.Candidates) != 1 {
		t.Fatalf("candidates: %+v", en)
	}
	cand := en.Candidates[0]
	if ev := library.EvidenceOf(cand); ev.AuthorsMatch == nil || *ev.AuthorsMatch {
		t.Fatalf("evidence %+v", ev)
	}

	// Attach the candidate by hand; detach rejects it for good.
	if _, err := f.lib.Attach(ctx, f.user, w.ID, cand.ID); err != nil {
		t.Fatal(err)
	}
	if got := sourceOf(t, waitMatched(t, f, w.ID), "fake:1001"); got.Attached == nil || got.Attached.Origin != library.OriginManual {
		t.Fatalf("attached: %+v", got)
	}
	if _, err := f.lib.Detach(ctx, f.user, w.ID, cand.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lib.Rematch(ctx, w.ID, f.user.UserID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	got := sourceOf(t, waitMatched(t, f, w.ID), "fake:1001")
	if got.Attached != nil || len(got.Candidates) != 0 {
		t.Fatalf("a rejected series came back: %+v", got)
	}

	// Attach by source and key (from browsing), and by URL.
	se, err := f.lib.AttachRef(ctx, f.user, w.ID, library.SourceRef{SourceID: "fake:2001", Key: "/series/2"}, library.OriginManual)
	if err != nil || se.State != library.StateAttached || se.Title != "Salz und Eisen" {
		t.Fatalf("attach ref: %v %+v", err, se)
	}
	byURL, err := f.lib.AttachURL(ctx, f.user, w.ID, "https://example-comics.example/series/2")
	if err != nil || byURL.Origin != library.OriginURL || byURL.SourceID != "fake:1001" {
		t.Fatalf("attach url: %v %+v", err, byURL)
	}
	if _, err := f.lib.AttachURL(ctx, f.user, w.ID, "https://elsewhere.example/x"); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("unknown url: %v", err)
	}
}

func TestVisibilityAndLibrary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.lib.Add(ctx, f.user, "e3", nil); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("above the ceiling: %v", err)
	}
	for _, id := range []string{"e2", "e1"} {
		if _, err := f.lib.Add(ctx, f.user, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.lib.List(ctx, f.user, "", nil, 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Work.Title != "Salt and Iron" || page.Next == nil {
		t.Fatalf("page 1: %v %+v", err, page)
	}
	page, err = f.lib.List(ctx, f.user, "", page.Next, 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Work.Title != "The Lantern Keeper" || page.Next != nil {
		t.Fatalf("page 2: %v %+v", err, page)
	}
	if q, _ := f.lib.List(ctx, f.user, "lantern", nil, 10); len(q.Entries) != 1 {
		t.Fatalf("query: %+v", q)
	}

	// Once matched only to sources a user may not use, the work hides.
	added, _ := f.lib.Add(ctx, f.user, "e1", nil)
	waitMatched(t, f, added.Work.ID)
	limited := f.user
	limited.Grants.AllSources, limited.Grants.AllowedSources = false, []string{"other:1"}
	if _, err := f.lib.Get(ctx, limited, added.Work.ID); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("hidden work: %v", err)
	}
	// Removing from the library.
	if err := f.lib.Remove(ctx, f.user, added.Work.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.lib.Remove(ctx, f.user, added.Work.ID); !errors.Is(err, library.ErrNotInLibrary) {
		t.Fatalf("twice: %v", err)
	}
}

func TestAddFromSource(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	// Clear: the exact title and the same author.
	res, err := f.lib.AddFromSource(ctx, f.user, library.SourceRef{SourceID: "fake:1001", Key: "/series/1"})
	if err != nil || res.Added == nil || res.Added.Work.MetaEntryID != "e1" {
		t.Fatalf("clear: %v %+v", err, res)
	}
	w, _ := f.lib.Get(ctx, f.user, res.Added.Work.ID)
	if got := sourceOf(t, w, "fake:1001"); got.Attached == nil || got.Attached.Key != "/series/1" {
		t.Fatalf("attached %+v", got)
	}
	// Unclear (another author): candidates to pick from.
	res, err = f.lib.AddFromSource(ctx, f.user, library.SourceRef{SourceID: "fake:1001", Key: "/series/2"})
	if err != nil || res.Added != nil || len(res.Candidates) != 1 || res.Candidates[0].Entry.GetId() != "e2" {
		t.Fatalf("unclear: %v %+v", err, res)
	}
}
