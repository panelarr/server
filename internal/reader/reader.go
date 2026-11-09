// SPDX-License-Identifier: GPL-3.0-or-later

// Package reader serves what the reader needs: a chapter's pages (from
// the chosen copy's source, through its plugin), the neighbouring
// chapters, reading progress synced across devices, history, and each
// user's reader settings and app preferences.
package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/downloads"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors callers map to responses.
var (
	ErrNotImage     = errors.New("reader: the page isn't a supported image")
	ErrNotFound     = errors.New("reader: not found")
	ErrNoCopy       = errors.New("reader: no copy of this chapter on a source you may use")
	ErrPageNotFound = errors.New("reader: no such page")
)

// Origins of pages.
const (
	OriginSource = "source"
	OriginFile   = "file"
)

// Service serves the reader.
type Service struct {
	st       *store.Store
	host     *plugins.Host
	library  *library.Service
	chapters *chapters.Service
	limits   *limits.Registry
	hub      *events.Hub
	log      *slog.Logger
	pages    pageCache
	files    Files
}

// Files opens downloaded chapters.
type Files interface {
	Open(ctx context.Context, chapterID uuid.UUID) (*downloads.File, error)
}

// SetFiles lets the reader serve downloaded chapters from their files.
func (s *Service) SetFiles(f Files) { s.files = f }

// openFile opens a chapter's downloaded file, or returns nil (also when
// its storage can't be reached right now: then the source serves it).
func (s *Service) openFile(ctx context.Context, chapterID uuid.UUID) *downloads.File {
	if s.files == nil {
		return nil
	}
	f, err := s.files.Open(ctx, chapterID)
	if err != nil {
		if !errors.Is(err, downloads.ErrNotStored) {
			s.log.WarnContext(ctx, "open downloaded chapter", "chapter", chapterID, "err", err)
		}
		return nil
	}
	return f
}

// NewService returns the reader service.
func NewService(st *store.Store, host *plugins.Host, lib *library.Service, chs *chapters.Service, lim *limits.Registry, hub *events.Hub, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if lim == nil {
		lim = limits.NewRegistry()
	}
	return &Service{st: st, host: host, library: lib, chapters: chs, limits: lim, hub: hub, log: log,
		pages: pageCache{ttl: 15 * time.Minute, max: 500}}
}

// Page is one page of a chapter.
type Page struct {
	Index int
	ref   string
}

// Pages is what the reader shows for a chapter.
type Pages struct {
	WorkID  uuid.UUID
	Chapter chapters.Chapter
	Variant chapters.VariantView
	Origin  string
	Pages   []Page
	Prev    *uuid.UUID
	Next    *uuid.UUID
	// Progress is the viewer's, if they started the chapter.
	Progress *db.Progress
}

// Locate returns a chapter v may see and its work: what a reader that
// couldn't load the chapter offers to go back to.
func (s *Service) Locate(ctx context.Context, v library.Viewer, chapterID uuid.UUID) (db.Work, db.Chapter, error) {
	ch, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Work{}, db.Chapter{}, ErrNotFound
	}
	if err != nil {
		return db.Work{}, db.Chapter{}, err
	}
	w, err := s.library.Get(ctx, v, ch.WorkID)
	if errors.Is(err, library.ErrNotFound) {
		return db.Work{}, db.Chapter{}, ErrNotFound
	}
	if err != nil {
		return db.Work{}, db.Chapter{}, err
	}
	return w.Work, ch, nil
}

// chapterFor finds a chapter v may read and the copy to read: variantID
// when given, else the chosen copy, else (when that is on a source v may
// not use) the first copy v may use.
func (s *Service) chapterFor(ctx context.Context, v library.Viewer, chapterID uuid.UUID, variantID *uuid.UUID) (chapters.Detail, chapters.VariantView, error) {
	ch, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return chapters.Detail{}, chapters.VariantView{}, ErrNotFound
	}
	if err != nil {
		return chapters.Detail{}, chapters.VariantView{}, err
	}
	if _, err := s.library.Get(ctx, v, ch.WorkID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return chapters.Detail{}, chapters.VariantView{}, ErrNotFound
		}
		return chapters.Detail{}, chapters.VariantView{}, err
	}
	d, err := s.chapters.Get(ctx, ch.WorkID, chapterID, v.UserID)
	if err != nil {
		return chapters.Detail{}, chapters.VariantView{}, err
	}
	srcs, err := s.host.Sources(ctx, plugins.Viewer{Grants: v.Grants, Admin: v.Admin, HideUnknown: v.HideUnknown}, false)
	if err != nil {
		return chapters.Detail{}, chapters.VariantView{}, err
	}
	usable := func(vv chapters.VariantView) bool {
		return slices.ContainsFunc(srcs, func(src plugins.Source) bool { return src.ID == vv.SourceID })
	}
	if variantID != nil {
		for _, vv := range d.All {
			if vv.ID == *variantID && usable(vv) {
				return d, vv, nil
			}
		}
		return chapters.Detail{}, chapters.VariantView{}, ErrNotFound
	}
	if d.Chosen != nil && usable(*d.Chosen) {
		return d, *d.Chosen, nil
	}
	for _, vv := range d.All {
		if usable(vv) {
			return d, vv, nil
		}
	}
	return chapters.Detail{}, chapters.VariantView{}, ErrNoCopy
}

// sourcePages lists a copy's pages, cached briefly (page lists rarely
// change, and the reader asks for every page).
func (s *Service) sourcePages(ctx context.Context, v library.Viewer, vv chapters.VariantView) ([]Page, error) {
	if pages, ok := s.pages.get(vv.ID); ok {
		return pages, nil
	}
	if err := s.limits.For("source:" + vv.SourceID).Wait(ctx); err != nil {
		return nil, err
	}
	list, err := s.host.Pages(ctx, plugins.Viewer{Grants: v.Grants, Admin: v.Admin, HideUnknown: v.HideUnknown}, vv.SourceID, vv.Key)
	if err != nil {
		return nil, err
	}
	out := make([]Page, len(list))
	for i, p := range list {
		ref := p.GetRef()
		if ref == "" {
			ref = p.GetImageUrl()
		}
		out[i] = Page{Index: i, ref: ref}
	}
	s.pages.put(vv.ID, out)
	return out, nil
}

// Pages returns what the reader shows for a chapter: from its downloaded
// file when there is one (unless a copy is asked for), else from a source.
func (s *Service) Pages(ctx context.Context, v library.Viewer, chapterID uuid.UUID, variantID *uuid.UUID) (Pages, error) {
	var out Pages
	if variantID == nil {
		f, d, err := s.fileFor(ctx, v, chapterID)
		if err != nil {
			return Pages{}, err
		}
		if f != nil {
			defer f.Close()
			out = Pages{WorkID: d.WorkID, Chapter: d.Chapter, Origin: OriginFile, Pages: make([]Page, f.Archive.Pages())}
			for i := range out.Pages {
				out.Pages[i] = Page{Index: i}
			}
			for _, vv := range d.All {
				if f.FromVariantID.Valid && vv.ID == f.FromVariantID.UUID {
					out.Variant = vv
				}
			}
			if out.Variant.ID == uuid.Nil && d.Chosen != nil {
				out.Variant = *d.Chosen
			}
		}
	}
	if out.Origin == "" {
		d, vv, err := s.chapterFor(ctx, v, chapterID, variantID)
		if err != nil {
			return Pages{}, err
		}
		pages, err := s.sourcePages(ctx, v, vv)
		if err != nil {
			return Pages{}, err
		}
		out = Pages{WorkID: d.WorkID, Chapter: d.Chapter, Variant: vv, Origin: OriginSource, Pages: pages}
	}
	var err error
	out.Prev, out.Next, err = s.neighbours(ctx, out.WorkID, chapterID, v.UserID)
	if err != nil {
		return Pages{}, err
	}
	if p, err := s.st.GetProgress(ctx, db.GetProgressParams{UserID: v.UserID, ChapterID: chapterID}); err == nil {
		out.Progress = &p
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Pages{}, err
	}
	return out, nil
}

// neighbours finds the available chapters before and after one, in the
// unified reading order.
func (s *Service) neighbours(ctx context.Context, workID, chapterID, userID uuid.UUID) (prev, next *uuid.UUID, err error) {
	list, err := s.chapters.List(ctx, workID, userID)
	if err != nil {
		return nil, nil, err
	}
	at := slices.IndexFunc(list, func(c chapters.Chapter) bool { return c.ID == chapterID })
	if at < 0 {
		return nil, nil, nil
	}
	for i := at - 1; i >= 0; i-- {
		if list[i].Available {
			id := list[i].ID
			prev = &id
			break
		}
	}
	for i := at + 1; i < len(list); i++ {
		if list[i].Available {
			id := list[i].ID
			next = &id
			break
		}
	}
	return prev, next, nil
}

// fileFor opens a chapter's downloaded file if v may read the chapter.
func (s *Service) fileFor(ctx context.Context, v library.Viewer, chapterID uuid.UUID) (*downloads.File, chapters.Detail, error) {
	ch, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, chapters.Detail{}, ErrNotFound
	}
	if err != nil {
		return nil, chapters.Detail{}, err
	}
	if _, err := s.library.Get(ctx, v, ch.WorkID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return nil, chapters.Detail{}, ErrNotFound
		}
		return nil, chapters.Detail{}, err
	}
	f := s.openFile(ctx, chapterID)
	if f == nil {
		return nil, chapters.Detail{}, nil
	}
	d, err := s.chapters.Get(ctx, ch.WorkID, chapterID, v.UserID)
	if err != nil {
		_ = f.Close()
		return nil, chapters.Detail{}, err
	}
	return f, d, nil
}

// rasterTypes are the page image types served. Anything else (SVG can
// carry scripts, HTML, …) is refused: pages are served from our origin.
var rasterTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif", "image/jxl"}

func checkImage(contentType string) (string, error) {
	ct, _, _ := mime.ParseMediaType(contentType)
	if ct == "image/jpg" {
		ct = "image/jpeg"
	}
	if !slices.Contains(rasterTypes, ct) {
		return "", fmt.Errorf("%w: %q", ErrNotImage, contentType)
	}
	return ct, nil
}

// MaxPageBytes caps one page image.
const MaxPageBytes = plugins.MaxImageBytes

// Page returns one page image: from the downloaded file when there is one
// (unless a copy is asked for), else through the copy's source.
func (s *Service) Page(ctx context.Context, v library.Viewer, chapterID uuid.UUID, variantID *uuid.UUID, index int) (string, []byte, error) {
	if variantID == nil {
		f, _, err := s.fileFor(ctx, v, chapterID)
		if err != nil {
			return "", nil, err
		}
		if f != nil {
			defer f.Close()
			if index < 0 || index >= f.Archive.Pages() {
				return "", nil, ErrPageNotFound
			}
			ct, rc, _, err := f.Archive.Page(index)
			if err != nil {
				return "", nil, err
			}
			defer rc.Close()
			data, err := io.ReadAll(io.LimitReader(rc, MaxPageBytes+1))
			if err != nil {
				return "", nil, err
			}
			ct, err = checkImage(ct)
			return ct, data, err
		}
	}
	_, vv, err := s.chapterFor(ctx, v, chapterID, variantID)
	if err != nil {
		return "", nil, err
	}
	pages, err := s.sourcePages(ctx, v, vv)
	if err != nil {
		return "", nil, err
	}
	if index < 0 || index >= len(pages) {
		return "", nil, ErrPageNotFound
	}
	// Images have their own budget (the plugin's image limits, or the
	// network profile's).
	key := s.limits.For(plugins.ImageKey(vv.SourceID))
	if err := key.Acquire(ctx); err != nil {
		return "", nil, err
	}
	defer key.Release()
	// Errors that may pass are tried again, each attempt paced by the
	// budget.
	ct, data, err := s.host.PageImage(ctx, plugins.Viewer{Grants: v.Grants, Admin: v.Admin, HideUnknown: v.HideUnknown}, vv.SourceID, pages[index].ref, key.Wait)
	if err != nil {
		// The page list may be stale (expired links): list again next time.
		s.pages.drop(vv.ID)
		return "", nil, err
	}
	ct, err = checkImage(ct)
	return ct, data, err
}

// SetProgress records where v is in a chapter. Reaching the last page
// (when the page count is known) marks it read.
func (s *Service) SetProgress(ctx context.Context, v library.Viewer, chapterID uuid.UUID, page int32, pages *int32, completed *bool) (db.Progress, error) {
	ch, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Progress{}, ErrNotFound
	}
	if err != nil {
		return db.Progress{}, err
	}
	if _, err := s.library.Get(ctx, v, ch.WorkID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return db.Progress{}, ErrNotFound
		}
		return db.Progress{}, err
	}
	done := pages != nil && page >= *pages-1
	if completed != nil {
		done = *completed
	}
	if pages != nil && page >= *pages {
		page = *pages - 1
	}
	p, err := s.st.UpsertProgress(ctx, db.UpsertProgressParams{
		UserID: v.UserID, ChapterID: chapterID, WorkID: ch.WorkID, Page: page, Pages: pages, Completed: done,
	})
	if err != nil {
		return db.Progress{}, err
	}
	s.hub.Publish("progress.synced", map[string]any{"work_id": ch.WorkID, "chapter_id": chapterID, "page": p.Page, "completed": p.Completed},
		events.Audience{Users: []uuid.UUID{v.UserID}})
	return p, nil
}

// MarkRead marks chapters of a work read (or unread: forgets progress).
func (s *Service) MarkRead(ctx context.Context, v library.Viewer, workID uuid.UUID, chapterIDs []uuid.UUID, read bool) error {
	if _, err := s.library.Get(ctx, v, workID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	all, err := s.st.ListChapters(ctx, workID)
	if err != nil {
		return err
	}
	for _, id := range chapterIDs {
		if !slices.ContainsFunc(all, func(c db.Chapter) bool { return c.ID == id }) {
			return fmt.Errorf("%w: chapter %s isn't part of the work", ErrNotFound, id)
		}
	}
	if !read {
		return s.st.DeleteProgress(ctx, db.DeleteProgressParams{UserID: v.UserID, ChapterIds: chapterIDs})
	}
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		for _, id := range chapterIDs {
			cur, err := q.GetProgress(ctx, db.GetProgressParams{UserID: v.UserID, ChapterID: id})
			page, pages := int32(0), (*int32)(nil)
			if err == nil {
				page, pages = cur.Page, cur.Pages
				if pages != nil {
					page = *pages - 1
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := q.UpsertProgress(ctx, db.UpsertProgressParams{
				UserID: v.UserID, ChapterID: id, WorkID: workID, Page: page, Pages: pages, Completed: true,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.hub.Publish("progress.synced", map[string]any{"work_id": workID}, events.Audience{Users: []uuid.UUID{v.UserID}})
	return nil
}

// WorkProgress returns v's progress in a work, and where to continue: the
// chapter last read, or the next one when that was finished.
func (s *Service) WorkProgress(ctx context.Context, v library.Viewer, workID uuid.UUID) ([]db.Progress, *uuid.UUID, error) {
	if _, err := s.library.Get(ctx, v, workID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	rows, err := s.st.WorkProgress(ctx, db.WorkProgressParams{UserID: v.UserID, WorkID: workID})
	if err != nil {
		return nil, nil, err
	}
	list, err := s.chapters.List(ctx, workID, v.UserID)
	if err != nil {
		return nil, nil, err
	}
	order := make([]readingChapter, len(list))
	for i, c := range list {
		order[i] = readingChapter{ID: c.ID, Available: c.Available}
	}
	return rows, continueAt(order, rows), nil
}

// readingChapter is a chapter in reading order, for continueAt.
type readingChapter struct {
	ID        uuid.UUID
	Available bool
}

// continueAt is where to continue a work: the chapter read last when it
// isn't finished, else the first available chapter after it that isn't
// read (from the start when nothing was read). Ties (chapters marked read
// together) go to the later one in reading order.
func continueAt(order []readingChapter, rows []db.Progress) *uuid.UUID {
	pos := make(map[uuid.UUID]int, len(order))
	for i, c := range order {
		pos[c.ID] = i
	}
	byChapter := make(map[uuid.UUID]db.Progress, len(rows))
	var last *db.Progress
	for i := range rows {
		r := &rows[i]
		byChapter[r.ChapterID] = *r
		if last == nil || r.UpdatedAt.After(last.UpdatedAt) ||
			(r.UpdatedAt.Equal(last.UpdatedAt) && pos[r.ChapterID] > pos[last.ChapterID]) {
			last = r
		}
	}
	start := 0
	if last != nil {
		if !last.Completed {
			id := last.ChapterID
			return &id
		}
		start = pos[last.ChapterID] + 1
	}
	for _, c := range order[min(start, len(order)):] {
		if p, ok := byChapter[c.ID]; c.Available && (!ok || !p.Completed) {
			id := c.ID
			return &id
		}
	}
	return nil
}

// HistoryQuery narrows a user's history: one entry per series (the latest
// chapter read), one series, or series whose title has Title.
type HistoryQuery struct {
	PerSeries bool
	WorkID    uuid.UUID
	Title     string
}

// HideHistory takes a series out of a user's history up to now (its
// read marks stay); reading it again brings it back.
func (s *Service) HideHistory(ctx context.Context, userID, workID uuid.UUID) error {
	return s.st.HideHistory(ctx, db.HideHistoryParams{UserID: userID, WorkID: workID})
}

// ClearHistory takes everything a user read up to now out of their
// history (read marks stay).
func (s *Service) ClearHistory(ctx context.Context, userID uuid.UUID) error {
	return s.st.ClearHistory(ctx, userID)
}

// HistoryItem is a chapter a user read.
type HistoryItem struct {
	Progress     db.Progress
	Chapter      db.Chapter
	WorkTitle    string
	WorkCoverURL string
}

// History returns a user's recently read chapters, newest first, before a
// cursor.
func (s *Service) History(ctx context.Context, userID uuid.UUID, q HistoryQuery, before *db.Progress, limit int) ([]HistoryItem, error) {
	p := db.HistoryParams{UserID: userID, Lim: int32(limit), PerSeries: q.PerSeries} //nolint:gosec // bounded
	if t := strings.TrimSpace(q.Title); t != "" {
		p.Q = pgtype.Text{String: t, Valid: true}
	}
	if q.WorkID != uuid.Nil {
		p.WorkID = uuid.NullUUID{UUID: q.WorkID, Valid: true}
	}
	if before != nil {
		p.Before = &before.UpdatedAt
		p.BeforeChapter = uuid.NullUUID{UUID: before.ChapterID, Valid: true}
	}
	rows, err := s.st.History(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryItem, len(rows))
	for i, r := range rows {
		out[i] = HistoryItem{Progress: r.Progress, Chapter: r.Chapter, WorkTitle: r.WorkTitle, WorkCoverURL: r.WorkCoverUrl}
	}
	return out, nil
}

// pageCache keeps page lists for a while.
type pageCache struct {
	ttl time.Duration
	max int

	mu sync.Mutex
	m  map[uuid.UUID]cachedPages
}

type cachedPages struct {
	pages []Page
	at    time.Time
}

func (c *pageCache) get(id uuid.UUID) ([]Page, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || time.Since(e.at) > c.ttl {
		return nil, false
	}
	return e.pages, true
}

func (c *pageCache) put(id uuid.UUID, pages []Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= c.max {
		c.m = map[uuid.UUID]cachedPages{}
	}
	c.m[id] = cachedPages{pages: pages, at: time.Now()}
}

func (c *pageCache) drop(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// SetBookmarks bookmarks chapters of a work for v, or removes their
// bookmarks, and returns the work's bookmarked chapters.
func (s *Service) SetBookmarks(ctx context.Context, v library.Viewer, workID uuid.UUID, chapterIDs []uuid.UUID, on bool) ([]uuid.UUID, error) {
	if _, err := s.library.Get(ctx, v, workID); err != nil {
		if errors.Is(err, library.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	all, err := s.st.ListChapters(ctx, workID)
	if err != nil {
		return nil, err
	}
	for _, id := range chapterIDs {
		if !slices.ContainsFunc(all, func(c db.Chapter) bool { return c.ID == id }) {
			return nil, fmt.Errorf("%w: chapter %s isn't part of the work", ErrNotFound, id)
		}
	}
	if on {
		err = s.st.AddBookmarks(ctx, db.AddBookmarksParams{UserID: v.UserID, ChapterIds: chapterIDs, WorkID: workID})
	} else {
		err = s.st.DeleteBookmarks(ctx, db.DeleteBookmarksParams{UserID: v.UserID, ChapterIds: chapterIDs})
	}
	if err != nil {
		return nil, err
	}
	s.hub.Publish("bookmarks.changed", map[string]any{"work_id": workID}, events.Audience{Users: []uuid.UUID{v.UserID}})
	return s.Bookmarks(ctx, v.UserID, workID)
}

// Bookmarks lists a user's bookmarked chapters of a work, oldest bookmark
// first.
func (s *Service) Bookmarks(ctx context.Context, userID, workID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := s.st.ListBookmarks(ctx, db.ListBookmarksParams{UserID: userID, WorkID: workID})
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, err
}

// BookmarkCounts counts a user's bookmarks per work.
func (s *Service) BookmarkCounts(ctx context.Context, userID uuid.UUID, workIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := s.st.CountBookmarks(ctx, db.CountBookmarksParams{UserID: userID, WorkIds: workIDs})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int64, len(rows))
	for _, r := range rows {
		out[r.WorkID] = r.Bookmarks
	}
	return out, nil
}

// ReadingStats is a user's reading of one work, for the library.
type ReadingStats struct {
	// Chapters and Unread count available chapters (all, and those not
	// finished); Downloaded those stored.
	Chapters, Unread, Downloaded int64
	// Started: the user read some of it.
	Started bool
	// LastReadAt is when the user last read in it; LatestChapterAt when
	// its newest chapter came out (or was first seen); LastUpdatedAt when
	// a later check last found a chapter of it (nil when none ever was).
	LastReadAt, LatestChapterAt, LastUpdatedAt *time.Time
	// Continue is the chapter "Continue" opens.
	Continue *uuid.UUID
}

// LibraryStats is a user's reading of some works, in a few queries.
func (s *Service) LibraryStats(ctx context.Context, userID uuid.UUID, workIDs []uuid.UUID) (map[uuid.UUID]ReadingStats, error) {
	chs, err := s.st.LibraryChapters(ctx, workIDs)
	if err != nil {
		return nil, err
	}
	prog, err := s.st.LibraryProgress(ctx, db.LibraryProgressParams{UserID: userID, WorkIds: workIDs})
	if err != nil {
		return nil, err
	}
	files, err := s.st.CountDownloadedByWork(ctx, workIDs)
	if err != nil {
		return nil, err
	}
	// Reading order as in the work's chapter list: by number, chapters
	// without one after, oldest first.
	slices.SortStableFunc(chs, func(a, b db.LibraryChaptersRow) int {
		switch {
		case a.Number == nil && b.Number == nil:
			return a.CreatedAt.Compare(b.CreatedAt)
		case a.Number == nil:
			return 1
		case b.Number == nil:
			return -1
		case *a.Number < *b.Number:
			return -1
		case *a.Number > *b.Number:
			return 1
		}
		return 0
	})
	byWork := map[uuid.UUID][]db.Progress{}
	finished := map[uuid.UUID]bool{}
	out := make(map[uuid.UUID]ReadingStats, len(workIDs))
	for _, p := range prog {
		byWork[p.WorkID] = append(byWork[p.WorkID], p)
		finished[p.ChapterID] = p.Completed
		st := out[p.WorkID]
		st.Started = true
		if st.LastReadAt == nil || p.UpdatedAt.After(*st.LastReadAt) {
			at := p.UpdatedAt
			st.LastReadAt = &at
		}
		out[p.WorkID] = st
	}
	order := map[uuid.UUID][]readingChapter{}
	for _, c := range chs {
		order[c.WorkID] = append(order[c.WorkID], readingChapter{ID: c.ID, Available: c.Available})
		if !c.Available {
			continue
		}
		st := out[c.WorkID]
		st.Chapters++
		if !finished[c.ID] {
			st.Unread++
		}
		at := c.CreatedAt
		if c.UploadedAt != nil {
			at = *c.UploadedAt
		}
		if st.LatestChapterAt == nil || at.After(*st.LatestChapterAt) {
			st.LatestChapterAt = &at
		}
		if c.AppearedAt != nil && (st.LastUpdatedAt == nil || c.AppearedAt.After(*st.LastUpdatedAt)) {
			appeared := *c.AppearedAt
			st.LastUpdatedAt = &appeared
		}
		out[c.WorkID] = st
	}
	for _, f := range files {
		st := out[f.WorkID]
		st.Downloaded = f.Downloaded
		out[f.WorkID] = st
	}
	for id, list := range order {
		st := out[id]
		st.Continue = continueAt(list, byWork[id])
		out[id] = st
	}
	return out, nil
}
