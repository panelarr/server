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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	if err := s.limits.For("source:" + vv.SourceID).Wait(ctx); err != nil {
		return "", nil, err
	}
	ct, data, err := s.host.Image(ctx, plugins.Viewer{Grants: v.Grants, Admin: v.Admin, HideUnknown: v.HideUnknown}, vv.SourceID, pages[index].ref)
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
	// The chapter read last; ties (chapters marked read together) go to
	// the later one in reading order.
	order := make(map[uuid.UUID]int, len(list))
	for i, c := range list {
		order[c.ID] = i
	}
	byChapter := make(map[uuid.UUID]db.Progress, len(rows))
	var last *db.Progress
	for i := range rows {
		r := &rows[i]
		byChapter[r.ChapterID] = *r
		if last == nil || r.UpdatedAt.After(last.UpdatedAt) ||
			(r.UpdatedAt.Equal(last.UpdatedAt) && order[r.ChapterID] > order[last.ChapterID]) {
			last = r
		}
	}
	start := 0
	if last != nil {
		if !last.Completed {
			id := last.ChapterID
			return rows, &id, nil
		}
		start = order[last.ChapterID] + 1
	}
	// Continue at the first available chapter from there that isn't read.
	var cont *uuid.UUID
	for _, c := range list[min(start, len(list)):] {
		if p, ok := byChapter[c.ID]; c.Available && (!ok || !p.Completed) {
			id := c.ID
			cont = &id
			break
		}
	}
	return rows, cont, nil
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
func (s *Service) History(ctx context.Context, userID uuid.UUID, before *db.Progress, limit int) ([]HistoryItem, error) {
	p := db.HistoryParams{UserID: userID, Lim: int32(limit)} //nolint:gosec // bounded
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
