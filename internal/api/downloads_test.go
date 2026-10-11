// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
)

func waitDownloads(t *testing.T, c *client, want int) apigen.DownloadPage {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		page := call[apigen.DownloadPage](c, http.MethodGet, "/api/v1/downloads?all=true&state=succeeded&page_size=100", nil, 200)
		if len(page.Items) >= want {
			return page
		}
		if time.Now().After(deadline) {
			all := call[apigen.DownloadPage](c, http.MethodGet, "/api/v1/downloads?all=true&page_size=100", nil, 200)
			t.Fatalf("%d of %d downloads done: %+v", len(page.Items), want, all.Items)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDownloads(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	admin := f.admin
	entry := call[apigen.LibraryEntry](f.user, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	chs := waitChapters(t, f.user, id, func(cs []apigen.UnifiedChapter) bool {
		return len(cs) == 3 && cs[0].Variants == 2 && cs[2].Variants == 2
	})

	// Downloading needs the grant.
	f.user.expectProblem(http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]string{"scope": "all"}, 403, "grant_required")
	downloader, _ := f.h.createUser(admin, "downloader", map[string]any{"may_download": true})
	call[apigen.LibraryEntry](downloader, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	q := call[apigen.DownloadsQueued](downloader, http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]string{"scope": "all"}, 202)
	if q.Queued != 3 {
		t.Fatalf("queued %+v", q)
	}
	// Asking again (anyone) doesn't make a second copy.
	call[apigen.DownloadsQueued](admin, http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]any{"chapter_ids": []string{chs[0].Id.String()}}, 202)
	waitDownloads(t, admin, 3)
	mine := call[apigen.DownloadPage](downloader, http.MethodGet, "/api/v1/downloads", nil, 200)
	if len(mine.Items) != 3 || mine.Items[0].WorkTitle != "The Lantern Keeper" {
		t.Fatalf("my downloads %+v", mine)
	}
	if again := call[apigen.DownloadsQueued](downloader, http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]string{"scope": "all"}, 202); again.Downloaded != 3 || again.Queued != 0 {
		t.Fatalf("again %+v", again)
	}

	// The files: one per chapter, named by the template, with ComicInfo.
	file, err := f.h.st.GetChapterFile(context.Background(), chs[0].Id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(file.Key, "The Lantern Keeper [") || !strings.HasSuffix(file.Key, "/Ch.0001.cbz") || file.Pages != 4 || len(file.Sha256) != 64 {
		t.Fatalf("file %+v", file)
	}
	zr, err := zip.OpenReader(filepath.Join(f.h.dataDir, "library", filepath.FromSlash(file.Key)))
	if err != nil {
		t.Fatal(err)
	}
	var info string
	for _, zf := range zr.File {
		if zf.Name == "ComicInfo.xml" {
			rc, _ := zf.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			info = string(b)
		}
	}
	_ = zr.Close()
	for _, want := range []string{"<Series>The Lantern Keeper</Series>", "<Number>1</Number>", "<Writer>A. Author</Writer>", "<Manga>YesAndRightToLeft</Manga>", "<PageCount>4</PageCount>", "panelarr:work=" + id} {
		if !strings.Contains(info, want) {
			t.Errorf("ComicInfo lacks %s:\n%s", want, info)
		}
	}

	// The reader serves downloaded chapters from the file.
	list := call[apigen.ChapterList](f.user, http.MethodGet, "/api/v1/works/"+id+"/chapters", nil, 200)
	if !list.Chapters[0].Downloaded {
		t.Fatal("not marked downloaded")
	}
	pages := call[apigen.ChapterPages](f.user, http.MethodGet, "/api/v1/chapters/"+chs[0].Id.String()+"/pages", nil, 200)
	if pages.Origin != apigen.ChapterPagesOriginFile || len(pages.Pages) != 4 || strings.Contains(pages.Pages[0].Url, "variant_id") {
		t.Fatalf("pages %+v", pages)
	}
	if res := f.user.do(http.MethodGet, pages.Pages[2].Url, nil); res.status != 200 || res.header.Get("Content-Type") != "image/png" {
		t.Fatalf("file page %d %v", res.status, res.header)
	}

	// Only admins delete files; the reader falls back to the source.
	f.user.expectProblem(http.MethodDelete, "/api/v1/chapters/"+chs[0].Id.String()+"/file", nil, 403, "forbidden")
	call[struct{}](admin, http.MethodDelete, "/api/v1/chapters/"+chs[0].Id.String()+"/file", nil, 204)
	admin.expectProblem(http.MethodDelete, "/api/v1/chapters/"+chs[0].Id.String()+"/file", nil, 404, CodeNotFound)
	if p := call[apigen.ChapterPages](f.user, http.MethodGet, "/api/v1/chapters/"+chs[0].Id.String()+"/pages", nil, 200); p.Origin != apigen.ChapterPagesOriginSource {
		t.Fatalf("after delete %s", p.Origin)
	}

	// Download new chapters automatically.
	f.user.expectProblem(http.MethodPut, "/api/v1/library/"+id+"/auto-download", map[string]bool{"enabled": true}, 403, "grant_required")
	call[struct{}](downloader, http.MethodPut, "/api/v1/library/"+id+"/auto-download", map[string]bool{"enabled": true}, 204)
	if lib := call[apigen.LibraryPage](downloader, http.MethodGet, "/api/v1/library", nil, 200); !lib.Entries[0].AutoDownload {
		t.Fatal("auto download not shown")
	}
	four := 4.0
	en := f.plugin.Sources[0].Series[0]
	en.Chapters = append([]*plugintest.Chapter{{Pages: 2, Chapter: &sourcev1.Chapter{Key: "/series/1/4", Name: "Chapter 4", Number: &four}}}, en.Chapters...)
	call[apigen.RefreshQueued](downloader, http.MethodPost, "/api/v1/works/"+id+"/refresh", nil, 202)
	waitDownloads(t, admin, 4)
}

func TestStorages(t *testing.T) {
	t.Parallel()
	f := newLibraryFixture(t)
	admin := f.admin
	backend := s3mem.New()
	if err := backend.CreateBucket("library"); err != nil {
		t.Fatal(err)
	}
	s3srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(s3srv.Close)

	list := call[apigen.StorageList](admin, http.MethodGet, "/api/v1/storages", nil, 200)
	if len(list.Storages) != 1 || list.Storages[0].Id != "local" || !list.Storages[0].Default || list.Storages[0].Root == nil {
		t.Fatalf("storages %+v", list)
	}
	s3 := map[string]any{"id": "s3", "name": "Bucket", "kind": "s3", "endpoint": s3srv.URL, "bucket": "library",
		"prefix": "panelarr", "region": "us-east-1", "path_style": true, "access_key": "key", "secret_key": "secret"}
	bad := map[string]any{}
	for k, v := range s3 {
		bad[k] = v
	}
	bad["bucket"] = "missing"
	admin.expectProblem(http.MethodPost, "/api/v1/storages", bad, 422, "storage_check_failed")
	admin.expectProblem(http.MethodPost, "/api/v1/storages", map[string]any{"id": "rel", "name": "x", "kind": "local", "root": "relative/dir"}, 400, CodeInvalidRequest)
	created := call[apigen.StorageInfo](admin, http.MethodPost, "/api/v1/storages", s3, 201)
	if !created.HasSecret || created.Default || strings.Contains(string(admin.do(http.MethodGet, "/api/v1/storages", nil).body), `:"secret"`) {
		t.Fatalf("created %+v", created)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/storages", s3, 409, "storage_id_taken")
	if r := call[apigen.StorageTest](admin, http.MethodPost, "/api/v1/storages/s3/test", nil, 200); !r.Ok {
		t.Fatalf("test %+v", r)
	}
	call[apigen.StorageInfo](admin, http.MethodPatch, "/api/v1/storages/s3", map[string]bool{"default": true}, 200)

	// Downloads go to the bucket; the reader reads them back with ranges.
	entry := call[apigen.LibraryEntry](admin, http.MethodPost, "/api/v1/library", map[string]string{"entry_id": "e1"}, 201)
	id := entry.Work.Id.String()
	chs := waitChapters(t, admin, id, func(cs []apigen.UnifiedChapter) bool { return len(cs) == 3 && cs[0].Variants == 2 })
	call[apigen.DownloadsQueued](admin, http.MethodPost, "/api/v1/works/"+id+"/downloads", map[string]any{"chapter_ids": []string{chs[0].Id.String()}}, 202)
	waitDownloads(t, admin, 1)
	file, err := f.h.st.GetChapterFile(context.Background(), chs[0].Id)
	if err != nil || file.StorageID != "s3" {
		t.Fatalf("file %+v %v", file, err)
	}
	pages := call[apigen.ChapterPages](admin, http.MethodGet, "/api/v1/chapters/"+chs[0].Id.String()+"/pages", nil, 200)
	if pages.Origin != apigen.ChapterPagesOriginFile || len(pages.Pages) != 4 {
		t.Fatalf("pages %+v", pages)
	}
	if res := admin.do(http.MethodGet, pages.Pages[3].Url, nil); res.status != 200 {
		t.Fatalf("page from bucket %d", res.status)
	}

	// Removing: not while files are on it, never the default.
	admin.expectProblem(http.MethodDelete, "/api/v1/storages/s3", nil, 409, "storage_default")
	call[apigen.StorageInfo](admin, http.MethodPatch, "/api/v1/storages/local", map[string]bool{"default": true}, 200)
	admin.expectProblem(http.MethodDelete, "/api/v1/storages/s3", nil, 409, "storage_in_use")

	// Move the files back to the local storage, then remove the bucket.
	task := call[apigen.TaskRef](admin, http.MethodPost, "/api/v1/storages/s3/move", map[string]string{"to": "local"}, 202)
	waitTask(t, admin, task.TaskId.String(), apigen.TaskStateSucceeded)
	if file, _ := f.h.st.GetChapterFile(context.Background(), chs[0].Id); file.StorageID != "local" {
		t.Fatalf("not moved: %+v", file)
	}
	if res := admin.do(http.MethodGet, pages.Pages[0].Url, nil); res.status != 200 {
		t.Fatalf("page after move %d", res.status)
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/storages/s3", nil, 204)
	admin.expectProblem(http.MethodPost, "/api/v1/storages/local/move", map[string]string{"to": "local"}, 400, CodeInvalidRequest)
}
