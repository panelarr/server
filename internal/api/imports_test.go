// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/importer"
	"github.com/panelarr/server/internal/importer/mihonbackup/backuptest"
	"github.com/panelarr/server/internal/meta/metatest"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// upload posts a file as the multipart field "file" to /me/imports.
func (c *client) upload(field string, file io.Reader) response {
	c.h.t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, err := mw.CreateFormFile(field, "library.tachibk")
		if err == nil {
			_, err = io.Copy(fw, file)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.h.srv.URL+"/api/v1/me/imports", pr)
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.csrf != "" {
		req.Header.Set(CSRFHeader, c.csrf)
	}
	if c.apiKey != "" {
		req.Header.Set(APIKeyHeader, c.apiKey)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: b}
}

func (c *client) uploadImport(file []byte, want int) apigen.Import {
	c.h.t.Helper()
	res := c.upload("file", bytes.NewReader(file))
	if res.status != want {
		c.h.t.Fatalf("upload: status %d, want %d: %s", res.status, want, res.body)
	}
	var imp apigen.Import
	if err := json.Unmarshal(res.body, &imp); err != nil {
		c.h.t.Fatal(err)
	}
	return imp
}

// waitImport waits until an import's preview is over.
func waitImport(t *testing.T, c *client, id uuid.UUID) apigen.Import {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		imp := call[apigen.Import](c, http.MethodGet, "/api/v1/me/imports/"+id.String(), nil, 200)
		if imp.State != apigen.ImportStatePreviewing {
			return imp
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview not finished: %+v", imp)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// importItems returns an import's series by idx, all pages.
func importItems(t *testing.T, c *client, id uuid.UUID, query string) map[int]apigen.ImportItem {
	t.Helper()
	out := map[int]apigen.ImportItem{}
	tok := ""
	for {
		path := "/api/v1/me/imports/" + id.String() + "/items?page_size=3" + query
		if tok != "" {
			path += "&page_token=" + tok
		}
		page := call[apigen.ImportItemPage](c, http.MethodGet, path, nil, 200)
		for _, it := range page.Items {
			out[it.Idx] = it
		}
		if page.NextPageToken == "" {
			return out
		}
		tok = page.NextPageToken
	}
}

func outcomes(items map[int]apigen.ImportItem) string {
	var b strings.Builder
	for i := range len(items) {
		fmt.Fprintf(&b, "%d:%s ", i, items[i].Outcome)
	}
	return strings.TrimSpace(b.String())
}

type importFixture struct {
	h      *harness
	admin  *client
	user   *client
	userID uuid.UUID
}

// newImportFixture sets up a catalogue and, with sources, the fake plugin
// whose sources are known by Mihon-style compat IDs ("mihon:<id>"). All
// names are made up.
func newImportFixture(t *testing.T, sources bool) importFixture {
	t.Helper()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 4})
	add := func(id, title, author string, rating metadatav1.ContentRating) {
		fake.AddEntry(&metav1.Entry{
			Id: id, Title: title, ContentRating: rating, Type: metadatav1.EntryType_ENTRY_TYPE_MANGA,
			Titles:  []*metadatav1.Title{{Text: title, Language: "en", IsPrimary: true}},
			Credits: []*metadatav1.Credit{{Person: &metadatav1.Person{Name: author}, Role: metadatav1.PersonRole_PERSON_ROLE_AUTHOR}},
		})
	}
	add("e1", "The Lantern Keeper", "A. Author", metadatav1.ContentRating_CONTENT_RATING_SAFE)
	add("e2", "Salt and Iron", "Somebody Else", metadatav1.ContentRating_CONTENT_RATING_SAFE)
	add("e3", "Paper Cranes", "A. Author", metadatav1.ContentRating_CONTENT_RATING_SAFE)
	add("e4", "Night Garden", "A. Author", metadatav1.ContentRating_CONTENT_RATING_PORNOGRAPHIC)
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)
	if sources {
		plug := plugintest.New()
		for _, s := range plug.Sources {
			s.Source.CompatIds = []string{plugin.CompatID("mihon", s.Source.GetId())}
		}
		call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": pluginstest.Serve(t, plug, "k"), "api_key": "k"}, 201)
	}
	user, u := h.createUser(admin, "reader", nil)
	return importFixture{h: h, admin: admin, user: user, userID: u.Id}
}

// importBackup holds one series per outcome (see the comments), with
// sources 1001 and 3001/3002 of the fake plugin (the latter two from a
// package that isn't installed) and 9999, which nothing provides.
func importBackup() backuptest.Backup {
	read := []backuptest.Chapter{{URL: "/c/1", Number: 1, Read: true}, {URL: "/c/2", Number: 2, Bookmark: true}}
	return backuptest.Backup{
		Series: []backuptest.Series{
			{SourceID: 1001, URL: "/series/1", Title: "Lantern Keeper (old name)", Chapters: read},        // 0 ready
			{SourceID: 1001, URL: "/series/2", Title: "Salt and Iron", Author: "A. Author"},               // 1 needs_choice
			{SourceID: 9999, URL: "/x/1", Title: "The Lantern Keeper", Author: "B. Other, A. Author"},     // 2 source_missing
			{SourceID: 9999, URL: "/x/2", Title: "Nothing Like This", Author: "A. Author"},                // 3 not_found
			{SourceID: 3001, URL: "/series/1", Title: "First Light", Author: "A. Author"},                 // 4 not_found, a hint
			{SourceID: 3002, URL: "/series/1", Title: "Premier Jour", Author: "A. Author"},                // 5 not_found, a hint
			{SourceID: 1001, URL: "/series/4", Title: "Paper Cranes", Author: "A. Author"},                // 6 in_library
			{SourceID: 9999, URL: "/x/3", Title: "Night Garden", Artist: "A. Author", Chapters: read[:1]}, // 7 not_allowed (ceiling)
		},
		Sources: []backuptest.Source{{ID: 1001, Name: "Example Comics"}, {ID: 9999, Name: "Gone Comics"}},
	}
}

func TestImportPreviewOutcomes(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	// Paper Cranes is in the user's library with the backup's series.
	call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e3", "source": map[string]string{"source_id": "fake:1001", "key": "/series/4"}}, 201)

	imp := f.user.uploadImport(importBackup().File(), 202)
	if imp.State != apigen.ImportStatePreviewing || imp.Format != apigen.ImportFormatMihonBackup || imp.Summary.Series != 8 ||
		imp.Summary.ChaptersRead != 2 || imp.Summary.Bookmarks != 1 || imp.Progress.Total != 8 {
		t.Fatalf("new import %+v", imp)
	}
	imp = waitImport(t, f.user, imp.Id)
	if imp.State != apigen.ImportStatePreview || imp.Progress.Done != 8 || imp.FinishedAt != nil {
		t.Fatalf("previewed %+v", imp)
	}
	items := importItems(t, f.user, imp.Id, "")
	want := "0:ready 1:needs_choice 2:source_missing 3:not_found 4:not_found 5:not_found 6:in_library 7:not_allowed"
	if got := outcomes(items); got != want {
		t.Fatalf("outcomes\n got %s\nwant %s", got, want)
	}
	// Ready through the source's own details of the series (the backup's
	// title is an old one), with the source to attach.
	if it := items[0]; deref(it.EntryId) != "e1" || deref(it.SourceId) != "fake:1001" || it.ChaptersRead != 1 || it.Bookmarks != 1 ||
		it.Chapters != 2 || it.SourceName != "Example Comics" || it.SourceCompatId != "mihon:1001" {
		t.Fatalf("ready %+v", it)
	}
	if it := items[1]; len(it.Candidates) == 0 || it.Candidates[0].EntryId != "e2" || it.EntryId != nil {
		t.Fatalf("needs choice %+v", it)
	}
	if it := items[2]; deref(it.EntryId) != "e1" || it.SourceId != nil || it.SourceName != "Gone Comics" {
		t.Fatalf("source missing %+v", it)
	}
	if it := items[6]; it.WorkId == nil || deref(it.EntryId) != "e3" {
		t.Fatalf("in library %+v", it)
	}
	if it := items[7]; !strings.Contains(deref(it.Message), "content rating") || len(it.Candidates) != 0 {
		t.Fatalf("not allowed %+v", it)
	}
	// Package hints: the not-installed package provides 3001 and 3002.
	if h := imp.Summary.PackageHints; len(h) != 1 || h[0].PluginId != "fake" || h[0].PackageId != "multi.comics" || h[0].Series != 2 {
		t.Fatalf("hints %+v", h)
	}
	counts := map[apigen.ImportOutcome]int{}
	for _, c := range imp.Outcomes {
		counts[c.Outcome] = c.Series
	}
	if counts[apigen.ImportOutcomeNotFound] != 3 || counts[apigen.ImportOutcomeReady] != 1 || counts[apigen.ImportOutcomePending] != 0 {
		t.Fatalf("outcome counts %+v", imp.Outcomes)
	}
	// Filtered by outcome.
	if nf := importItems(t, f.user, imp.Id, "&outcome=not_found"); len(nf) != 3 || nf[3].Title != "Nothing Like This" {
		t.Fatalf("not found %+v", nf)
	}
	// The preview changed nothing: only Paper Cranes is in the library.
	if lib := call[apigen.LibraryPage](f.user, http.MethodGet, "/api/v1/library", nil, 200); len(lib.Entries) != 1 {
		t.Fatalf("library %+v", lib.Entries)
	}
	// An admin's ceiling allows Night Garden: source missing, not refused.
	adminImp := waitImport(t, f.admin, f.admin.uploadImport(importBackup().File(), 202).Id)
	if it := importItems(t, f.admin, adminImp.Id, "")[7]; it.Outcome != apigen.ImportOutcomeSourceMissing || deref(it.EntryId) != "e4" {
		t.Fatalf("admin's night garden %+v", it)
	}
	// Listed, newest first.
	if l := call[apigen.ImportList](f.user, http.MethodGet, "/api/v1/me/imports", nil, 200); len(l.Imports) != 1 || l.Imports[0].Id != imp.Id {
		t.Fatalf("list %+v", l)
	}
}

func TestImportPreviewGrants(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	// noadd had Paper Cranes added before losing may_add.
	noadd, me := f.h.createUser(f.admin, "noadd", nil)
	call[apigen.LibraryEntry](noadd, http.MethodPost, "/api/v1/library",
		map[string]any{"entry_id": "e3", "source": map[string]string{"source_id": "fake:1001", "key": "/series/4"}}, 201)
	call[apigen.User](f.admin, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"grants": map[string]any{"may_add": false}}, 200)
	imp := waitImport(t, noadd, noadd.uploadImport(importBackup().File(), 202).Id)
	items := importItems(t, noadd, imp.Id, "")
	want := "0:not_allowed 1:not_allowed 2:not_allowed 3:not_allowed 4:not_allowed 5:not_allowed 6:in_library 7:not_allowed"
	if got := outcomes(items); got != want {
		t.Fatalf("without may_add\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(deref(items[0].Message), "may not add") {
		t.Fatalf("message %+v", items[0])
	}

	// limited may use no source: the plugin's sources are not allowed, a
	// source nobody has is missing.
	limited, _ := f.h.createUser(f.admin, "limited", map[string]any{"all_sources": false, "allowed_sources": []string{}})
	imp = waitImport(t, limited, limited.uploadImport(importBackup().File(), 202).Id)
	items = importItems(t, limited, imp.Id, "")
	want = "0:not_allowed 1:not_allowed 2:source_missing 3:not_found 4:not_found 5:not_found 6:not_allowed 7:not_allowed"
	if got := outcomes(items); got != want {
		t.Fatalf("limited\n got %s\nwant %s", got, want)
	}
	if it := items[0]; !strings.Contains(deref(it.Message), "source") || it.SourceId != nil {
		t.Fatalf("source not allowed %+v", it)
	}
}

func TestImportPreviewWithoutSourcePlugins(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, false)
	imp := waitImport(t, f.user, f.user.uploadImport(importBackup().File(), 202).Id)
	items := importItems(t, f.user, imp.Id, "")
	// Everything goes through the catalogue alone: clear works are
	// "source missing".
	want := "0:not_found 1:needs_choice 2:source_missing 3:not_found 4:not_found 5:not_found 6:source_missing 7:not_allowed"
	if got := outcomes(items); got != want {
		t.Fatalf("outcomes\n got %s\nwant %s", got, want)
	}
	if len(imp.Summary.PackageHints) != 0 {
		t.Fatalf("hints %+v", imp.Summary.PackageHints)
	}
}

func TestImportIsolatedPerUser(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	other, otherUser := f.h.createUser(f.admin, "other", nil)
	mine := f.h.hub.Subscribe(eventsViewer(f.userID, false))
	defer mine.Close()
	theirs := f.h.hub.Subscribe(eventsViewer(otherUser.Id, false))
	defer theirs.Close()

	imp := waitImport(t, f.user, f.user.uploadImport(importBackup().File(), 202).Id)
	base := "/api/v1/me/imports/" + imp.Id.String()
	other.expectProblem(http.MethodGet, base, nil, 404, CodeNotFound)
	other.expectProblem(http.MethodGet, base+"/items", nil, 404, CodeNotFound)
	other.expectProblem(http.MethodDelete, base, nil, 404, CodeNotFound)
	if l := call[apigen.ImportList](other, http.MethodGet, "/api/v1/me/imports", nil, 200); len(l.Imports) != 0 {
		t.Fatalf("other sees %+v", l)
	}
	// The same file is a new import for another user.
	if o := other.uploadImport(importBackup().File(), 202); o.Id == imp.Id {
		t.Fatal("another user got the same import")
	}
	// Events: the owner hears progress and the end; the other user only
	// about their own import.
	finished := false
	timeout := time.After(10 * time.Second)
	for !finished {
		select {
		case e := <-mine.C:
			if fe, ok := e.Data.(importer.FinishedEvent); ok && e.Type == importer.EventFinished && fe.ImportID == imp.Id {
				finished = true
			}
		case <-timeout:
			t.Fatal("no import.finished for the owner")
		}
	}
	drain := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-theirs.C:
			if pe, ok := e.Data.(importer.ProgressEvent); ok && pe.ImportID == imp.Id {
				t.Fatalf("other user got %s", e.Type)
			}
			if fe, ok := e.Data.(importer.FinishedEvent); ok && fe.ImportID == imp.Id {
				t.Fatalf("other user got %s", e.Type)
			}
		case <-drain:
			// Deleting is the owner's: the record goes, the library stays.
			call[struct{}](f.user, http.MethodDelete, base, nil, 204)
			f.user.expectProblem(http.MethodGet, base, nil, 404, CodeNotFound)
			return
		}
	}
}

func TestImportUploadRules(t *testing.T) {
	t.Parallel()
	f := newImportFixture(t, true)
	file := importBackup().File()
	// Not a backup: 422, and nothing is created.
	res := f.user.upload("file", strings.NewReader("this is not a backup"))
	if res.status != 422 || res.problem(t).Code != "bad_backup" {
		t.Fatalf("garbage: %d %s", res.status, res.body)
	}
	res = f.user.upload("file", bytes.NewReader(backuptest.Backup{Series: []backuptest.Series{{SourceID: 1, URL: "/a", Title: "A", NotInLibrary: true}}}.File()))
	if res.status != 422 || res.problem(t).Code != "bad_backup" {
		t.Fatalf("empty library: %d %s", res.status, res.body)
	}
	res = f.user.upload("other", bytes.NewReader(file))
	if res.status != 400 {
		t.Fatalf("no file field: %d %s", res.status, res.body)
	}
	if l := call[apigen.ImportList](f.user, http.MethodGet, "/api/v1/me/imports", nil, 200); len(l.Imports) != 0 {
		t.Fatalf("bad uploads left imports: %+v", l)
	}

	// While one import runs, another file is refused; the same file
	// returns the running import.
	if err := f.h.queue.SetPaused(importer.KindPreview, true); err != nil {
		t.Fatal(err)
	}
	first := f.user.uploadImport(file, 202)
	other := backuptest.Backup{Series: []backuptest.Series{{SourceID: 1001, URL: "/series/3", Title: "A Quiet Orbit"}}}.File()
	f.user.expectProblemUpload(other, 409, "import_running")
	if again := f.user.uploadImport(file, 200); again.Id != first.Id {
		t.Fatal("re-upload made another import")
	}
	if err := f.h.queue.SetPaused(importer.KindPreview, false); err != nil {
		t.Fatal(err)
	}
	waitImport(t, f.user, first.Id)
	// Previewed: the same file still returns it; another file is a new
	// import.
	if again := f.user.uploadImport(file, 200); again.Id != first.Id || again.State != apigen.ImportStatePreview {
		t.Fatalf("re-upload %+v", again)
	}
	second := f.user.uploadImport(other, 202)
	waitImport(t, f.user, second.Id)

	// One backup is read at a time: while the gate is held, 429.
	if !f.h.gate.Acquire(context.Background(), 0) {
		t.Fatal("gate")
	}
	third := backuptest.Backup{Series: []backuptest.Series{{SourceID: 1001, URL: "/series/1", Title: "Third"}}}.File()
	res = f.user.upload("file", bytes.NewReader(third))
	f.h.gate.Release()
	if res.status != 429 || res.problem(t).Code != "busy" {
		t.Fatalf("busy: %d %s", res.status, res.body)
	}
	if ra, err := strconv.Atoi(res.header.Get("Retry-After")); err != nil || ra <= 0 {
		t.Fatalf("Retry-After %q", res.header.Get("Retry-After"))
	}
	f.user.uploadImport(third, 202)

	// Over 64 MiB: 413, read only up to the limit.
	big := io.LimitReader(zeroes{}, importer.MaxFileSize+1)
	res = f.user.upload("file", big)
	if res.status != 413 || res.problem(t).Code != "too_large" {
		t.Fatalf("too large: %d %s", res.status, res.body)
	}
	// Other operations keep the small limit.
	res = f.user.do(http.MethodPost, "/api/v1/me/library-views", strings.Repeat(" ", 2<<20))
	if res.status != 413 && res.status != 400 {
		t.Fatalf("large JSON body: %d", res.status)
	}
}

func (c *client) expectProblemUpload(file []byte, status int, code string) {
	c.h.t.Helper()
	res := c.upload("file", bytes.NewReader(file))
	if res.status != status || res.problem(c.h.t).Code != code {
		c.h.t.Fatalf("upload: %d %s, want %d %s", res.status, res.body, status, code)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
