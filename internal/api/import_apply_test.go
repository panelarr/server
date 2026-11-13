// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/importer"
	"github.com/panelarr/server/internal/importer/mihonbackup/backuptest"
)

var (
	backupAdded = time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	fetchedAt   = time.Date(2024, 12, 1, 8, 0, 0, 0, time.UTC)
	readFirst   = time.Date(2025, 1, 10, 20, 0, 0, 0, time.UTC)
	readSecond  = time.Date(2025, 2, 10, 21, 0, 0, 0, time.UTC)
)

func importPath(id uuid.UUID, rest string) string {
	return "/api/v1/me/imports/" + id.String() + rest
}

// applyAndWait applies a previewed import and waits until it is over.
func applyAndWait(t *testing.T, c *client, id uuid.UUID) apigen.Import {
	t.Helper()
	imp := call[apigen.Import](c, http.MethodPost, importPath(id, "/apply"), nil, 202)
	if imp.State != apigen.ImportStateApplying {
		t.Fatalf("apply: %+v", imp)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		imp = call[apigen.Import](c, http.MethodGet, importPath(id, ""), nil, 200)
		if imp.State != apigen.ImportStateApplying {
			return imp
		}
		if time.Now().After(deadline) {
			t.Fatalf("apply not finished: %+v", imp)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// waitQuiet waits until no task is due or running: matching, chapter
// checks and placing pending chapters are done.
func waitQuiet(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	quiet := 0
	for quiet < 3 {
		var n int
		if err := h.st.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM tasks WHERE state = 'running' OR (state = 'queued' AND run_after <= now())`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			quiet++
		} else {
			quiet = 0
		}
		if time.Now().After(deadline) {
			t.Fatal("tasks never settled")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

type progressRow struct {
	page      int
	completed bool
	at        time.Time
}

// progressOf is a user's progress on a work (by catalogue entry), by
// chapter number.
func progressOf(t *testing.T, h *harness, user uuid.UUID, entry string) map[float64]progressRow {
	t.Helper()
	rows, err := h.st.Pool.Query(context.Background(), `
		SELECT c.number, p.page, p.completed, p.updated_at FROM progress p
		JOIN chapters c ON c.id = p.chapter_id JOIN works w ON w.id = p.work_id
		WHERE p.user_id = $1 AND w.meta_entry_id = $2`, user, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[float64]progressRow{}
	for rows.Next() {
		var n float64
		var r progressRow
		if err := rows.Scan(&n, &r.page, &r.completed, &r.at); err != nil {
			t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

func bookmarksOf(t *testing.T, h *harness, user uuid.UUID, entry string) map[float64]time.Time {
	t.Helper()
	rows, err := h.st.Pool.Query(context.Background(), `
		SELECT c.number, b.created_at FROM bookmarks b
		JOIN chapters c ON c.id = b.chapter_id JOIN works w ON w.id = b.work_id
		WHERE b.user_id = $1 AND w.meta_entry_id = $2`, user, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[float64]time.Time{}
	for rows.Next() {
		var n float64
		var at time.Time
		if err := rows.Scan(&n, &at); err != nil {
			t.Fatal(err)
		}
		out[n] = at
	}
	return out
}

func entryAddedAt(t *testing.T, h *harness, user uuid.UUID, entry string) (time.Time, bool) {
	t.Helper()
	var at time.Time
	err := h.st.Pool.QueryRow(context.Background(), `
		SELECT le.added_at FROM library_entries le JOIN works w ON w.id = le.work_id
		WHERE le.user_id = $1 AND w.meta_entry_id = $2`, user, entry).Scan(&at)
	return at, err == nil
}

func workOf(t *testing.T, h *harness, entry string) string {
	t.Helper()
	var id uuid.UUID
	if err := h.st.Pool.QueryRow(context.Background(), `SELECT id FROM works WHERE meta_entry_id = $1`, entry).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// countRows counts a user's rows of reading state, to show nothing was
// added twice.
func countRows(t *testing.T, h *harness, user uuid.UUID) string {
	t.Helper()
	var le, p, b, ss int
	var latest *time.Time
	if err := h.st.Pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM library_entries WHERE user_id = $1),
		       (SELECT count(*) FROM progress WHERE user_id = $1),
		       (SELECT count(*) FROM bookmarks WHERE user_id = $1),
		       (SELECT count(*) FROM source_series),
		       (SELECT max(updated_at) FROM progress WHERE user_id = $1)`, user).Scan(&le, &p, &b, &ss, &latest); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("entries=%d progress=%d bookmarks=%d series=%d latest=%v", le, p, b, ss, latest)
}

// readingBackup: a series on a source Panelarr has (its chapters are
// placed by key, whatever their numbers say) and one on a source it
// lacks (placed by number, once the work's own matching found chapters).
func readingBackup() backuptest.Backup {
	return backuptest.Backup{
		Series: []backuptest.Series{{
			SourceID: 1001, URL: "/series/1", Title: "The Lantern Keeper", Author: "A. Author", DateAdded: backupAdded,
			Chapters: []backuptest.Chapter{
				{URL: "/series/1/1", Number: 1, Read: true},
				{URL: "/series/1/2", Number: 77, Read: true, Bookmark: true}, // a wrong number: the key places it
				{URL: "/series/1/3", Number: 3, LastPage: 2, DateFetch: fetchedAt},
				{URL: "/gone/9", Number: 9, Read: true}, // no such chapter (yet)
				{URL: "/odd", Number: -1, Read: true},   // no number: only a key could place it
			},
			History: []backuptest.History{{URL: "/series/1/1", LastRead: readFirst}, {URL: "/series/1/2", LastRead: readSecond}},
		}, {
			SourceID: 9999, URL: "/x/3", Title: "Paper Cranes", Author: "A. Author", DateAdded: backupAdded,
			Chapters: []backuptest.Chapter{
				{URL: "/p/1", Number: 1, Read: true},
				{URL: "/p/2", Number: 2, Bookmark: true},
				{URL: "/p/x", Number: -1, Read: true}, // never placeable
			},
			// A wrong clock in the other app: capped at the apply.
			History: []backuptest.History{{URL: "/p/1", LastRead: readFirst}, {URL: "/p/2", LastRead: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}},
		}},
		Sources: []backuptest.Source{{ID: 1001, Name: "Example Comics"}, {ID: 9999, Name: "Gone Comics"}},
	}
}

// TestImportApplyPlacesReading covers AC3 and AC4: works are added (with
// the backup's added date) and attached; read marks, pages, times and
// bookmarks land on the right chapters, by key and else by number; History
// shows the original times and "continue" follows the last read chapter;
// chapters that arrive later are placed after their chapter check, and
// those that can't be placed are counted.
func TestImportApplyPlacesReading(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	imp := waitImport(t, f.user, f.user.uploadImport(readingBackup().File(), 202).Id)
	if got := outcomes(importItems(t, f.user, imp.Id, "")); got != "0:ready 1:source_missing" {
		t.Fatalf("preview %s", got)
	}
	done := applyAndWait(t, f.user, imp.Id)
	if done.State != apigen.ImportStateDone || done.FinishedAt == nil || done.Progress.Done != 2 || done.Progress.Total != 2 ||
		len(done.Results) != 1 || done.Results[0] != (apigen.ImportResultCount{Result: apigen.ImportResultApplied, Series: 2}) {
		t.Fatalf("applied %+v", done)
	}
	waitQuiet(t, f.h)
	items := importItems(t, f.user, imp.Id, "")
	if got := outcomes(items); got != "0:applied 1:applied" {
		t.Fatalf("after apply %s", got)
	}
	// The Lantern Keeper: 3 placed by key; /gone/9 and /odd wait for
	// chapters that may still appear on the attached series.
	if it := items[0]; it.Placed != 3 || it.Pending != 2 || it.Unplaced != 0 || deref(it.SourceId) != "fake:1001" || it.AppliedAt == nil {
		t.Fatalf("lantern keeper %+v", it)
	}
	// Paper Cranes: no source of its own; its chapters were placed by
	// number once matching attached another source's series and its
	// chapters were checked. The one without a number can't be placed.
	if it := items[1]; it.Placed != 2 || it.Pending != 0 || it.Unplaced != 1 || it.SourceId != nil {
		t.Fatalf("paper cranes %+v", it)
	}

	at, ok := entryAddedAt(t, f.h, f.userID, "e1")
	if !ok || !at.Equal(backupAdded) {
		t.Fatalf("added_at %v (%v)", at, ok)
	}
	p := progressOf(t, f.h, f.userID, "e1")
	want := map[float64]progressRow{
		1: {0, true, readFirst},
		2: {0, true, readSecond}, // by key, not by its number 77
		3: {2, false, fetchedAt}, // in progress, at the fetch date
	}
	if len(p) != len(want) {
		t.Fatalf("progress %+v", p)
	}
	for n, w := range want {
		if g := p[n]; g.page != w.page || g.completed != w.completed || !g.at.Equal(w.at) {
			t.Fatalf("chapter %v: %+v, want %+v", n, g, w)
		}
	}
	if b := bookmarksOf(t, f.h, f.userID, "e1"); len(b) != 1 || !b[2].Equal(readSecond) {
		t.Fatalf("bookmarks %+v", b)
	}
	if p := progressOf(t, f.h, f.userID, "e3"); len(p) != 1 || !p[1].completed || !p[1].at.Equal(readFirst) {
		t.Fatalf("paper cranes progress %+v", p)
	}
	if b := bookmarksOf(t, f.h, f.userID, "e3"); len(b) != 1 || b[2].After(time.Now()) {
		t.Fatalf("paper cranes bookmarks %+v", b)
	}
	if s := call[apigen.Import](f.user, http.MethodGet, importPath(imp.Id, ""), nil, 200).Summary; deref(s.ClampedTimes) != 1 {
		t.Fatalf("clamped times %v", s.ClampedTimes)
	}

	// The backup's series is attached, recorded as imported.
	var origin string
	if err := f.h.st.Pool.QueryRow(context.Background(),
		`SELECT origin FROM source_series WHERE source_id = 'fake:1001' AND key = '/series/1' AND state = 'attached'`).Scan(&origin); err != nil ||
		(origin != "import" && origin != "auto") {
		t.Fatalf("attached series: %q %v", origin, err)
	}

	// History shows the original times, newest first; continue follows the
	// chapter read last (2), so it points at 3.
	hist := call[apigen.HistoryPage](f.user, http.MethodGet, "/api/v1/me/history", nil, 200)
	if len(hist.Items) == 0 || !hist.Items[0].UpdatedAt.Equal(readSecond) {
		t.Fatalf("history %+v", hist.Items)
	}
	work := workOf(t, f.h, "e1")
	chs := waitChapters(t, f.user, work, func(c []apigen.UnifiedChapter) bool { return len(c) == 3 })
	var third uuid.UUID
	for _, c := range chs {
		if c.Number != nil && *c.Number == 3 {
			third = c.Id
		}
	}
	wp := call[apigen.WorkProgress](f.user, http.MethodGet, "/api/v1/works/"+work+"/progress", nil, 200)
	if wp.ContinueChapterId == nil || *wp.ContinueChapterId != third {
		t.Fatalf("continue %v, want chapter 3 %v", wp.ContinueChapterId, third)
	}
}

// TestImportNewerReadingWins covers AC6 and AC5: reading done in Panelarr
// is never undone by an import, and applying a backup again adds nothing.
func TestImportNewerReadingWins(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	le := call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e1", "source": map[string]string{"source_id": "fake:1001", "key": "/series/1"}}, 201)
	work := le.Work.Id.String()
	chs := waitChapters(t, f.user, work, func(c []apigen.UnifiedChapter) bool { return len(c) == 3 })
	ids := map[float32]uuid.UUID{}
	for _, c := range chs {
		ids[*c.Number] = c.Id
	}
	// In Panelarr: 1 read, 2 at page 3 (now, newer than the backup), 3
	// bookmarked.
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+ids[1].String()+"/progress", map[string]any{"page": 5, "completed": true}, 200)
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+ids[2].String()+"/progress", map[string]any{"page": 3}, 200)
	call[struct{}](f.user, http.MethodPost, "/api/v1/works/"+work+"/bookmarks", map[string]any{"chapter_ids": []string{ids[3].String()}, "bookmarked": true}, 200)
	before := progressOf(t, f.h, f.userID, "e1")
	marks := bookmarksOf(t, f.h, f.userID, "e1")
	addedBefore, _ := entryAddedAt(t, f.h, f.userID, "e1")

	// The backup: 1 unread at page 1 (older), 2 read at an older time, 3
	// read and not bookmarked.
	backup := backuptest.Backup{Series: []backuptest.Series{{
		SourceID: 1001, URL: "/series/1", Title: "The Lantern Keeper", Author: "A. Author", DateAdded: backupAdded,
		Chapters: []backuptest.Chapter{
			{URL: "/series/1/1", Number: 1, LastPage: 1},
			{URL: "/series/1/2", Number: 2, Read: true, LastPage: 9},
			{URL: "/series/1/3", Number: 3, Read: true},
		},
		History: []backuptest.History{{URL: "/series/1/1", LastRead: readFirst}, {URL: "/series/1/2", LastRead: readFirst}, {URL: "/series/1/3", LastRead: readSecond}},
	}}}
	imp := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	if got := outcomes(importItems(t, f.user, imp.Id, "")); got != "0:in_library" {
		t.Fatalf("preview %s", got)
	}
	applyAndWait(t, f.user, imp.Id)
	waitQuiet(t, f.h)
	after := progressOf(t, f.h, f.userID, "e1")
	// 1: read in Panelarr stays read, its page and time too.
	if g := after[1]; g != before[1] {
		t.Fatalf("chapter 1: %+v, was %+v", g, before[1])
	}
	// 2: read in the backup, so read; Panelarr's newer page and time stay.
	if g := after[2]; !g.completed || g.page != 3 || !g.at.Equal(before[2].at) {
		t.Fatalf("chapter 2: %+v, was %+v", g, before[2])
	}
	// 3: read at its original time.
	if g := after[3]; !g.completed || !g.at.Equal(readSecond) {
		t.Fatalf("chapter 3: %+v", g)
	}
	// The bookmark stays, the entry keeps its date.
	if b := bookmarksOf(t, f.h, f.userID, "e1"); len(b) != 1 || !b[3].Equal(marks[3]) {
		t.Fatalf("bookmarks %+v, were %+v", b, marks)
	}
	if at, _ := entryAddedAt(t, f.h, f.userID, "e1"); !at.Equal(addedBefore) {
		t.Fatalf("added_at moved: %v, was %v", at, addedBefore)
	}
	if it := importItems(t, f.user, imp.Id, "")[0]; deref(it.Result) != apigen.ImportResultApplied || it.Placed != 3 {
		t.Fatalf("item %+v", it)
	}

	// A done import isn't applied twice.
	f.user.expectProblem(http.MethodPost, importPath(imp.Id, "/apply"), nil, 409, "import_state")

	// AC5: the same backup again (a new import once the first is deleted)
	// changes nothing and says so.
	rows := countRows(t, f.h, f.userID)
	call[struct{}](f.user, http.MethodDelete, importPath(imp.Id, ""), nil, 204)
	again := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	applyAndWait(t, f.user, again.Id)
	waitQuiet(t, f.h)
	if got := outcomes(importItems(t, f.user, again.Id, "")); got != "0:already_imported" {
		t.Fatalf("re-apply %s", got)
	}
	if got := countRows(t, f.h, f.userID); got != rows {
		t.Fatalf("re-apply changed rows:\n%s\nwas\n%s", got, rows)
	}
	if got := progressOf(t, f.h, f.userID, "e1"); fmt.Sprint(got) != fmt.Sprint(after) {
		t.Fatalf("re-apply changed progress:\n%+v\nwas\n%+v", got, after)
	}
}

// TestImportApplyRechecksGrants: grants are checked again at apply, as
// they are then.
func TestImportApplyRechecksGrants(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	// A user who loses may_add after the preview: only the work already
	// in their library gets its reading.
	noadd, me := f.h.createUser(f.admin, "noadd", nil)
	call[apigen.LibraryEntry](noadd, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e3", "source": map[string]string{"source_id": "fake:1001", "key": "/series/4"}}, 201)
	imp := waitImport(t, noadd, noadd.uploadImport(importBackup().File(), 202).Id)
	if got := outcomes(importItems(t, noadd, imp.Id, "")); !strings.HasPrefix(got, "0:ready ") {
		t.Fatalf("preview %s", got)
	}
	call[apigen.User](f.admin, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"grants": map[string]any{"may_add": false}}, 200)
	applyAndWait(t, noadd, imp.Id)
	items := importItems(t, noadd, imp.Id, "")
	want := "0:not_allowed 1:needs_choice 2:not_allowed 3:not_found 4:not_found 5:not_found 6:already_imported 7:not_allowed"
	if got := outcomes(items); got != want {
		t.Fatalf("without may_add\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(deref(items[0].Message), "may not add") {
		t.Fatalf("message %+v", items[0])
	}
	if l := call[apigen.LibraryPage](noadd, http.MethodGet, "/api/v1/library", nil, 200); len(l.Entries) != 1 {
		t.Fatalf("library %+v", l.Entries)
	}

	// A user who may no longer use the source: nothing is attached for
	// them, and the series isn't added.
	limited, lu := f.h.createUser(f.admin, "limited", nil)
	imp = waitImport(t, limited, limited.uploadImport(readingBackup().File(), 202).Id)
	call[apigen.User](f.admin, http.MethodPatch, "/api/v1/users/"+lu.Id.String(),
		map[string]any{"grants": map[string]any{"all_sources": false, "allowed_sources": []string{}}}, 200)
	applyAndWait(t, limited, imp.Id)
	items = importItems(t, limited, imp.Id, "")
	if it := items[0]; it.Outcome != apigen.ImportOutcomeNotAllowed || !strings.Contains(deref(it.Message), "source") {
		t.Fatalf("source no longer allowed %+v", it)
	}
	var attached int
	if err := f.h.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM source_series WHERE key = '/series/1'`).Scan(&attached); err != nil || attached != 0 {
		t.Fatalf("%d series attached (%v)", attached, err)
	}
	if _, ok := entryAddedAt(t, f.h, lu.Id, "e1"); ok {
		t.Fatal("added without the source")
	}
	// Paper Cranes' work is only on a source they may not use now (noadd
	// added it with one): they can't see it, so it isn't added.
	if it := items[1]; it.Outcome != apigen.ImportOutcomeNotAllowed || !strings.Contains(deref(it.Message), "sources you may not use") {
		t.Fatalf("paper cranes %+v", it)
	}
	if _, ok := entryAddedAt(t, f.h, lu.Id, "e3"); ok {
		t.Fatal("added a work they can't see")
	}

	// A user who may neither add works nor fix matches (as attaching by
	// hand needs one of them): the backup's series aren't attached to the
	// shared work, nor recorded as candidates, but their reading goes on
	// the chapters the work has, by number.
	bare, bu := f.h.createUser(f.admin, "bare", nil)
	libraryWithLanternKeeper(t, bare)
	backup := backuptest.Backup{Series: []backuptest.Series{{
		// No series on this source is attached yet: it would be.
		SourceID: 2001, URL: "/series/2", Title: "The Lantern Keeper", Author: "A. Author", DateAdded: backupAdded,
		Chapters: []backuptest.Chapter{{URL: "/series/2/1", Number: 1, Read: true}},
		History:  []backuptest.History{{URL: "/series/2/1", LastRead: readFirst}},
	}, {
		// Another series of the attached source: it would be a candidate.
		SourceID: 1001, URL: "/series/3", Title: "The Lantern Keeper", Author: "A. Author", DateAdded: backupAdded,
		Chapters: []backuptest.Chapter{{URL: "/series/3/2", Number: 2, LastPage: 3}},
		History:  []backuptest.History{{URL: "/series/3/2", LastRead: readSecond}},
	}}}
	imp = waitImport(t, bare, bare.uploadImport(backup.File(), 202).Id)
	if got := outcomes(importItems(t, bare, imp.Id, "")); got != "0:in_library 1:in_library" {
		t.Fatalf("preview %s", got)
	}
	call[apigen.User](f.admin, http.MethodPatch, "/api/v1/users/"+bu.Id.String(),
		map[string]any{"grants": map[string]any{"may_add": false, "may_manage_matches": false}}, 200)
	applyAndWait(t, bare, imp.Id)
	waitQuiet(t, f.h)
	items = importItems(t, bare, imp.Id, "")
	if got := outcomes(items); got != "0:applied 1:applied" {
		t.Fatalf("without may_add and may_manage_matches %s", got)
	}
	for i, it := range items {
		if it.SourceId != nil || !strings.Contains(deref(it.Message), "isn't attached") {
			t.Fatalf("series %d: %+v", i, it)
		}
	}
	if err := f.h.st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM source_series ss JOIN works w ON w.id = ss.work_id
		WHERE w.meta_entry_id = 'e1' AND ((ss.source_id = 'fake:2001' AND ss.key = '/series/2') OR (ss.source_id = 'fake:1001' AND ss.key = '/series/3'))`).Scan(&attached); err != nil || attached != 0 {
		t.Fatalf("%d series attached or recorded (%v)", attached, err)
	}
	p := progressOf(t, f.h, bu.Id, "e1")
	if g := p[1]; !g.completed || !g.at.Equal(readFirst) {
		t.Fatalf("chapter 1: %+v", g)
	}
	if g := p[2]; g.completed || g.page != 3 || !g.at.Equal(readSecond) {
		t.Fatalf("chapter 2: %+v", g)
	}
}

// TestImportResolve covers AC9: a series can be given its work by hand
// or skipped, before apply (it then goes with the rest) and after (it is
// applied at once, without uploading again).
func TestImportResolve(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	backup := importBackup()
	backup.Series[3].Chapters = []backuptest.Chapter{{URL: "/x/2/1", Number: 1, Read: true}}
	backup.Series[3].History = []backuptest.History{{URL: "/x/2/1", LastRead: readFirst}}
	imp := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	resolve := func(idx int, body any, want int) apigen.ImportItem {
		t.Helper()
		return call[apigen.ImportItem](f.user, http.MethodPost, importPath(imp.Id, "/items/"+strconv.Itoa(idx)+"/resolve"), body, want)
	}
	// Before apply: series 1 (needs a choice) gets its candidate; 4 is
	// skipped.
	if it := resolve(1, map[string]any{"entry_id": "e2"}, 200); it.Outcome != apigen.ImportOutcomeReady || deref(it.EntryId) != "e2" {
		t.Fatalf("resolved %+v", it)
	}
	if it := resolve(4, map[string]any{"skip": true}, 200); deref(it.Result) != apigen.ImportResultSkipped || it.Outcome != apigen.ImportOutcomeNotFound {
		t.Fatalf("skipped %+v", it)
	}
	path := importPath(imp.Id, "/items/")
	f.user.expectProblem(http.MethodPost, path+"0/resolve", map[string]any{"entry_id": "e2"}, 409, "import_item_state")
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{"entry_id": "nope"}, 400, CodeInvalidRequest)
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{"entry_id": "e4"}, 400, CodeInvalidRequest) // above the ceiling
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{}, 400, CodeInvalidRequest)
	f.user.expectProblem(http.MethodPost, path+"99/resolve", map[string]any{"skip": true}, 404, CodeNotFound)

	// While it applies, resolving waits.
	if err := f.h.queue.SetPaused(importer.KindApply, true); err != nil {
		t.Fatal(err)
	}
	call[apigen.Import](f.user, http.MethodPost, importPath(imp.Id, "/apply"), nil, 202)
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{"entry_id": "e3"}, 409, "import_running")
	if err := f.h.queue.SetPaused(importer.KindApply, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for call[apigen.Import](f.user, http.MethodGet, importPath(imp.Id, ""), nil, 200).State == apigen.ImportStateApplying {
		if time.Now().After(deadline) {
			t.Fatal("apply not finished")
		}
		time.Sleep(30 * time.Millisecond)
	}
	items := importItems(t, f.user, imp.Id, "")
	if it := items[1]; deref(it.Result) != apigen.ImportResultApplied {
		t.Fatalf("resolved series after apply %+v", it)
	}
	if it := items[4]; deref(it.Result) != apigen.ImportResultSkipped {
		t.Fatalf("skipped series after apply %+v", it)
	}
	if sk := importItems(t, f.user, imp.Id, "&result=skipped"); len(sk) != 1 || sk[4].Title != "First Light" {
		t.Fatalf("skipped %+v", sk)
	}
	if _, ok := entryAddedAt(t, f.h, f.userID, "e2"); !ok {
		t.Fatal("the resolved work isn't in the library")
	}

	// After apply: series 3 (not found) gets a work and its reading at
	// once.
	it := resolve(3, map[string]any{"entry_id": "e3"}, 200)
	if deref(it.Result) != apigen.ImportResultApplied || it.AppliedAt == nil {
		t.Fatalf("resolved after apply %+v", it)
	}
	waitQuiet(t, f.h)
	if p := progressOf(t, f.h, f.userID, "e3"); !p[1].completed || !p[1].at.Equal(readFirst) {
		t.Fatalf("resolved series' reading %+v", p)
	}
	// An applied series can't be resolved again.
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{"entry_id": "e1"}, 409, "import_item_state")
	f.user.expectProblem(http.MethodPost, path+"3/resolve", map[string]any{"skip": true}, 409, "import_item_state")
}

// TestImportCancel: a running import can be cancelled; one that isn't
// running can't.
func TestImportCancel(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	if err := f.h.queue.SetPaused(importer.KindPreview, true); err != nil {
		t.Fatal(err)
	}
	imp := f.user.uploadImport(importBackup().File(), 202)
	out := call[apigen.Import](f.user, http.MethodPost, importPath(imp.Id, "/cancel"), nil, 200)
	if out.State != apigen.ImportStateCancelled || out.FinishedAt == nil {
		t.Fatalf("cancelled %+v", out)
	}
	f.user.expectProblem(http.MethodPost, importPath(imp.Id, "/cancel"), nil, 409, "import_not_running")
	f.user.expectProblem(http.MethodPost, importPath(imp.Id, "/apply"), nil, 409, "import_state")
	if err := f.h.queue.SetPaused(importer.KindPreview, false); err != nil {
		t.Fatal(err)
	}
	// The user can import again right away.
	waitImport(t, f.user, f.user.uploadImport(importBackup().File(), 202).Id)
}

// TestImportApplyIsolatedPerUser covers AC10 for apply: another user
// can't apply, cancel or resolve someone's import, and their own reading
// of the same work is untouched.
func TestImportApplyIsolatedPerUser(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	other, ou := f.h.createUser(f.admin, "other", nil)
	call[apigen.LibraryEntry](other, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e1", "source": map[string]string{"source_id": "fake:1001", "key": "/series/1"}}, 201)
	imp := waitImport(t, f.user, f.user.uploadImport(readingBackup().File(), 202).Id)
	other.expectProblem(http.MethodPost, importPath(imp.Id, "/apply"), nil, 404, CodeNotFound)
	other.expectProblem(http.MethodPost, importPath(imp.Id, "/cancel"), nil, 404, CodeNotFound)
	other.expectProblem(http.MethodPost, importPath(imp.Id, "/items/0/resolve"), map[string]any{"skip": true}, 404, CodeNotFound)
	applyAndWait(t, f.user, imp.Id)
	waitQuiet(t, f.h)
	if p := progressOf(t, f.h, f.userID, "e1"); len(p) != 3 {
		t.Fatalf("importer's progress %+v", p)
	}
	if p := progressOf(t, f.h, ou.Id, "e1"); len(p) != 0 {
		t.Fatalf("the other user got progress %+v", p)
	}
	if b := bookmarksOf(t, f.h, ou.Id, "e1"); len(b) != 0 {
		t.Fatalf("the other user got bookmarks %+v", b)
	}
	if _, ok := entryAddedAt(t, f.h, ou.Id, "e3"); ok {
		t.Fatal("the other user got a library entry")
	}
}

// TestImportsCapAndPaging: a user keeps at most 20 imports, and lists them
// in pages.
func TestImportsCapAndPaging(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, false)
	for i := range importer.MaxImports {
		if _, err := f.h.st.Pool.Exec(context.Background(), `
			INSERT INTO imports (user_id, format, file_sha256, state, created_at)
			VALUES ($1, 'mihon_backup', $2, 'done', now() - make_interval(mins => $3))`, f.userID, fmt.Sprint("sum", i), i); err != nil {
			t.Fatal(err)
		}
	}
	file := backuptest.Backup{Series: []backuptest.Series{{SourceID: 1, URL: "/a", Title: "A"}}}.File()
	f.user.expectProblemUpload(file, 409, "too_many_imports")

	seen := map[uuid.UUID]bool{}
	var last time.Time
	tok, pages := "", 0
	for {
		path := "/api/v1/me/imports?page_size=7"
		if tok != "" {
			path += "&page_token=" + tok
		}
		page := call[apigen.ImportList](f.user, http.MethodGet, path, nil, 200)
		pages++
		for _, imp := range page.Imports {
			if seen[imp.Id] || (!last.IsZero() && imp.CreatedAt.After(last)) {
				t.Fatalf("page %d: %v out of order or repeated", pages, imp.Id)
			}
			seen[imp.Id], last = true, imp.CreatedAt
		}
		if page.NextPageToken == "" {
			break
		}
		tok = page.NextPageToken
	}
	if len(seen) != importer.MaxImports || pages != 3 {
		t.Fatalf("%d imports in %d pages", len(seen), pages)
	}
	f.user.expectProblem(http.MethodGet, "/api/v1/me/imports?page_token=bm9wZQ", nil, 400, CodeInvalidRequest)

	// Deleting one makes room.
	for id := range seen {
		call[struct{}](f.user, http.MethodDelete, importPath(id, ""), nil, 204)
		break
	}
	f.user.uploadImport(file, 202)
}

// libraryWithLanternKeeper adds e1 with the fake source's series to c's
// library and returns the work and its chapters' IDs by number.
func libraryWithLanternKeeper(t *testing.T, c *client) (string, map[float32]uuid.UUID) {
	t.Helper()
	le := call[apigen.LibraryEntry](c, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e1", "source": map[string]string{"source_id": "fake:1001", "key": "/series/1"}}, 201)
	work := le.Work.Id.String()
	ids := map[float32]uuid.UUID{}
	for _, ch := range waitChapters(t, c, work, func(c []apigen.UnifiedChapter) bool { return len(c) == 3 }) {
		ids[*ch.Number] = ch.Id
	}
	return work, ids
}

// ageProgress moves a user's reading a week back, as if done then.
func ageProgress(t *testing.T, h *harness, user uuid.UUID) {
	t.Helper()
	if _, err := h.st.Pool.Exec(context.Background(),
		`UPDATE progress SET updated_at = date_trunc('second', now()) - interval '7 days' WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
}

// TestImportUndatedMarksKeepPanelarrReading: a backup mark with no time at
// all (no history, fetch or added date) only has the upload's time, which
// says nothing about when it was read. It may add a missing row and mark
// a chapter read, but never moves the page or time of older reading done
// in Panelarr (it would win as "newer" and flood History).
func TestImportUndatedMarksKeepPanelarrReading(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	_, ids := libraryWithLanternKeeper(t, f.user)
	// A week ago in Panelarr: 1 read, 2 at page 3.
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+ids[1].String()+"/progress", map[string]any{"page": 5, "completed": true}, 200)
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+ids[2].String()+"/progress", map[string]any{"page": 3}, 200)
	ageProgress(t, f.h, f.userID)
	before := progressOf(t, f.h, f.userID, "e1")

	// The backup, without a single date: 1 unread at page 1, 2 read at
	// page 1, 3 read.
	backup := backuptest.Backup{Series: []backuptest.Series{{
		SourceID: 1001, URL: "/series/1", Title: "The Lantern Keeper", Author: "A. Author",
		Chapters: []backuptest.Chapter{
			{URL: "/series/1/1", Number: 1, LastPage: 1},
			{URL: "/series/1/2", Number: 2, Read: true, LastPage: 1},
			{URL: "/series/1/3", Number: 3, Read: true},
		},
	}}}
	uploaded := time.Now().Add(-time.Second)
	imp := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	applyAndWait(t, f.user, imp.Id)
	waitQuiet(t, f.h)
	after := progressOf(t, f.h, f.userID, "e1")
	// 1: untouched.
	if g := after[1]; g != before[1] {
		t.Fatalf("chapter 1: %+v, was %+v", g, before[1])
	}
	// 2: now read, but its page and time stay Panelarr's.
	if g := after[2]; !g.completed || g.page != before[2].page || !g.at.Equal(before[2].at) {
		t.Fatalf("chapter 2: %+v, was %+v", g, before[2])
	}
	// 3: added, read, at the upload's time.
	if g := after[3]; !g.completed || g.at.Before(uploaded) || g.at.After(time.Now()) {
		t.Fatalf("chapter 3: %+v", g)
	}
	if it := importItems(t, f.user, imp.Id, "")[0]; deref(it.Result) != apigen.ImportResultApplied || it.Placed != 3 {
		t.Fatalf("item %+v", it)
	}
}

// TestImportNeverRegresses pins the import's merge rules on reading that
// is in Panelarr already: a newer unread mark doesn't un-read a chapter,
// an existing bookmark keeps its time, and two backup copies of a chapter
// merge (read if either is, the later reading's page and time, bookmarked
// if either is).
func TestImportNeverRegresses(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	work, ids := libraryWithLanternKeeper(t, f.user)
	// In Panelarr: 1 read at page 4 a week ago; 3 bookmarked.
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+ids[1].String()+"/progress", map[string]any{"page": 4, "completed": true}, 200)
	ageProgress(t, f.h, f.userID)
	call[struct{}](f.user, http.MethodPost, "/api/v1/works/"+work+"/bookmarks", map[string]any{"chapter_ids": []string{ids[3].String()}, "bookmarked": true}, 200)
	marks := bookmarksOf(t, f.h, f.userID, "e1")

	yesterday := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond).UTC()
	backup := backuptest.Backup{Series: []backuptest.Series{{
		SourceID: 1001, URL: "/series/1", Title: "The Lantern Keeper", Author: "A. Author", DateAdded: backupAdded,
		Chapters: []backuptest.Chapter{
			// Unread at page 2, yesterday: newer than Panelarr's reading.
			{URL: "/series/1/1", Number: 1, LastPage: 2},
			// Two copies of chapter 2 (the second placed by number), both
			// bookmarked: one read, one unread at page 5 and read later.
			{URL: "/series/1/2", Number: 2, Read: true, Bookmark: true},
			{URL: "/elsewhere/2", Number: 2, LastPage: 5, Bookmark: true},
			// Bookmarked in the backup too, earlier.
			{URL: "/series/1/3", Number: 3, Bookmark: true},
		},
		History: []backuptest.History{
			{URL: "/series/1/1", LastRead: yesterday}, {URL: "/series/1/2", LastRead: readFirst},
			{URL: "/elsewhere/2", LastRead: readSecond}, {URL: "/series/1/3", LastRead: readFirst},
		},
	}}}
	imp := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	applyAndWait(t, f.user, imp.Id)
	waitQuiet(t, f.h)
	p := progressOf(t, f.h, f.userID, "e1")
	// 1: stays read with its page; the newer time is the backup's.
	if g := p[1]; !g.completed || g.page != 4 || !g.at.Equal(yesterday) {
		t.Fatalf("chapter 1: %+v", g)
	}
	// 2: merged.
	if g := p[2]; !g.completed || g.page != 5 || !g.at.Equal(readSecond) {
		t.Fatalf("chapter 2: %+v", g)
	}
	b := bookmarksOf(t, f.h, f.userID, "e1")
	if len(b) != 2 || !b[2].Equal(readSecond) {
		t.Fatalf("bookmarks %+v", b)
	}
	// 3: the existing bookmark keeps its time.
	if !b[3].Equal(marks[3]) {
		t.Fatalf("bookmark of 3 at %v, was %v", b[3], marks[3])
	}
}

// TestImportPlacesEachChapterOnce: a chapter placed after its work's
// chapters arrived isn't placed again when they are rebuilt later, while
// other chapters of its series still wait: marked unread in between, it
// stays unread.
func TestImportPlacesEachChapterOnce(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	// Paper Cranes on a source Panelarr lacks: placed by number once
	// matching attached the fake source's series. Chapter 50 never comes.
	backup := backuptest.Backup{Series: []backuptest.Series{{
		SourceID: 9999, URL: "/x/3", Title: "Paper Cranes", Author: "A. Author", DateAdded: backupAdded,
		Chapters: []backuptest.Chapter{{URL: "/p/1", Number: 1, Read: true}, {URL: "/p/50", Number: 50, Read: true}},
		History:  []backuptest.History{{URL: "/p/1", LastRead: readFirst}},
	}}}
	imp := waitImport(t, f.user, f.user.uploadImport(backup.File(), 202).Id)
	applyAndWait(t, f.user, imp.Id)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	waitFor("chapter 1 placed", func() bool { return progressOf(t, f.h, f.userID, "e3")[1].completed })
	waitQuiet(t, f.h)
	if it := importItems(t, f.user, imp.Id, "")[0]; it.Placed != 1 || it.Pending != 1 {
		t.Fatalf("item %+v", it)
	}
	work := workOf(t, f.h, "e3")
	var first uuid.UUID
	for _, c := range waitChapters(t, f.user, work, func(c []apigen.UnifiedChapter) bool { return len(c) == 3 }) {
		if *c.Number == 1 {
			first = c.Id
		}
	}
	call[struct{}](f.user, http.MethodPut, "/api/v1/chapters/"+first.String()+"/progress", map[string]any{"page": 0, "completed": false}, 200)

	places := func() int {
		var n int
		if err := f.h.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM tasks WHERE kind = $1`, importer.KindPlace).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := places()
	call[apigen.RefreshQueued](f.user, http.MethodPost, "/api/v1/works/"+work+"/refresh", nil, 202)
	waitFor("the rebuild to place again", func() bool { return places() > before })
	waitQuiet(t, f.h)
	if g := progressOf(t, f.h, f.userID, "e3")[1]; g.completed {
		t.Fatalf("chapter 1 read again: %+v", g)
	}
}
