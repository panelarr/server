// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/chapters"
)

func chaptersError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, chapters.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such chapter")
	case errors.Is(err, chapters.ErrNotThisChapter):
		return fieldProblem("variant_id", "not a copy of this chapter")
	}
	return libraryError(ctx, err)
}

// visibleSources returns the IDs of the sources the caller may use.
func (s *Server) visibleSources(ctx context.Context) (map[string]bool, error) {
	srcs, err := s.deps.Plugins.Sources(ctx, s.sourceViewer(ctx), false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(srcs))
	for _, src := range srcs {
		out[src.ID] = true
	}
	return out, nil
}

func toVariant(v chapters.VariantView) apigen.ChapterVariant {
	out := apigen.ChapterVariant{
		Id: v.ID, SeriesId: v.SeriesID, SourceId: v.SourceID, SourceName: v.SourceName, Key: v.Key, Name: v.Name,
		Volume: nonEmpty(v.Volume), Scanlator: nonEmpty(v.Scanlator), Lang: nonEmpty(v.Lang), UploadedAt: v.UploadedAt,
		Priority: int(v.Priority), SourceHealth: apigen.ChapterVariantSourceHealth(v.Health),
	}
	if n := chapters.NumberOf(v.Number, v.Name); n != nil {
		out.Number = ptr(float32(*n))
	}
	if out.SourceHealth == "" {
		out.SourceHealth = apigen.ChapterVariantSourceHealthUnknown
	}
	return out
}

// toUnified shows a chapter; its chosen copy only when it is on a source
// the caller may use.
func toUnified(c chapters.Chapter, visible map[string]bool, downloaded bool) apigen.UnifiedChapter {
	out := apigen.UnifiedChapter{
		Id: c.ID, Volume: nonEmpty(c.Volume), Title: c.Title, Extra: c.Number == nil, Available: c.Available,
		New: c.New, Variants: c.Variants, Reason: c.Reason, Pinned: c.PinnedVariantID.Valid, CreatedAt: c.CreatedAt,
		Downloaded: downloaded,
	}
	if c.Number != nil {
		out.Number = ptr(float32(*c.Number))
	}
	if c.Chosen != nil && visible[c.Chosen.SourceID] {
		v := toVariant(*c.Chosen)
		out.Chosen = &v
	}
	return out
}

// ListWorkChapters lists a work's unified chapters.
func (s *Server) ListWorkChapters(ctx context.Context, req apigen.ListWorkChaptersRequestObject) (apigen.ListWorkChaptersResponseObject, error) {
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	visible, err := s.visibleSources(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.deps.Chapters.List(ctx, req.WorkId, v.UserID)
	if err != nil {
		return nil, err
	}
	have, err := s.deps.Downloads.Downloaded(ctx, req.WorkId)
	if err != nil {
		return nil, err
	}
	out := apigen.ListWorkChapters200JSONResponse{Chapters: make([]apigen.UnifiedChapter, len(list))}
	for i, c := range list {
		out.Chapters[i] = toUnified(c, visible, have[c.ID])
	}
	if req.Params.Order != nil && *req.Params.Order == apigen.ListWorkChaptersParamsOrderDesc {
		slices.Reverse(out.Chapters)
	}
	return out, nil
}

func (s *Server) chapterDetail(ctx context.Context, d chapters.Detail) (apigen.ChapterDetail, error) {
	visible, err := s.visibleSources(ctx)
	if err != nil {
		return apigen.ChapterDetail{}, err
	}
	have, err := s.deps.Downloads.Downloaded(ctx, d.WorkID)
	if err != nil {
		return apigen.ChapterDetail{}, err
	}
	out := apigen.ChapterDetail{Chapter: toUnified(d.Chapter, visible, have[d.ID]), Variants: []apigen.ChapterVariant{}}
	for _, v := range d.All {
		if visible[v.SourceID] {
			out.Variants = append(out.Variants, toVariant(v))
		}
	}
	return out, nil
}

// GetWorkChapter returns one chapter with its copies.
func (s *Server) GetWorkChapter(ctx context.Context, req apigen.GetWorkChapterRequestObject) (apigen.GetWorkChapterResponseObject, error) {
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	d, err := s.deps.Chapters.Get(ctx, req.WorkId, req.ChapterId, v.UserID)
	if err != nil {
		return nil, chaptersError(ctx, err)
	}
	out, err := s.chapterDetail(ctx, d)
	if err != nil {
		return nil, err
	}
	return apigen.GetWorkChapter200JSONResponse(out), nil
}

// PinChapterVariant pins (or unpins) a chapter's copy.
func (s *Server) PinChapterVariant(ctx context.Context, req apigen.PinChapterVariantRequestObject) (apigen.PinChapterVariantResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	if _, err := s.deps.Chapters.Get(ctx, req.WorkId, req.ChapterId, v.UserID); err != nil {
		return nil, chaptersError(ctx, err)
	}
	if _, err := s.deps.Chapters.Pin(ctx, req.ChapterId, req.Body.VariantId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	d, err := s.deps.Chapters.Get(ctx, req.WorkId, req.ChapterId, v.UserID)
	if err != nil {
		return nil, chaptersError(ctx, err)
	}
	out, err := s.chapterDetail(ctx, d)
	if err != nil {
		return nil, err
	}
	return apigen.PinChapterVariant200JSONResponse(out), nil
}

// ListSeriesChapters lists one source's chapters of a work.
func (s *Server) ListSeriesChapters(ctx context.Context, req apigen.ListSeriesChaptersRequestObject) (apigen.ListSeriesChaptersResponseObject, error) {
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	se, list, err := s.deps.Chapters.SeriesChapters(ctx, req.WorkId, req.SeriesId)
	if err != nil {
		return nil, chaptersError(ctx, err)
	}
	if _, err := s.deps.Plugins.Source(ctx, se.SourceID, s.sourceViewer(ctx)); err != nil {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "no such series")
	}
	out := apigen.ListSeriesChapters200JSONResponse{
		Series: s.toMatchSeries(se), CheckedAt: se.CheckedAt, CheckError: nonEmpty(se.CheckError),
		Chapters: make([]apigen.SeriesChapter, len(list)),
	}
	for i, c := range list {
		sc := apigen.SeriesChapter{
			Key: c.Key, Name: c.Name, Volume: nonEmpty(c.Volume), Scanlator: nonEmpty(c.Scanlator), Lang: nonEmpty(c.Lang),
			Url: nonEmpty(c.Url), UploadedAt: c.UploadedAt, Present: c.Present,
		}
		if c.Number != nil {
			sc.Number = ptr(float32(*c.Number))
		}
		if c.ChapterID.Valid {
			sc.ChapterId = &c.ChapterID.UUID
		}
		out.Chapters[i] = sc
	}
	return out, nil
}

// RefreshWork queues chapter checks of a work's series now.
func (s *Server) RefreshWork(ctx context.Context, req apigen.RefreshWorkRequestObject) (apigen.RefreshWorkResponseObject, error) {
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, chaptersError(ctx, err)
	}
	n, err := s.deps.Chapters.Refresh(ctx, req.WorkId, v.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.RefreshWork202JSONResponse{Queued: n}, nil
}

// MarkWorkSeen clears the caller's "new" badges of a work.
func (s *Server) MarkWorkSeen(ctx context.Context, req apigen.MarkWorkSeenRequestObject) (apigen.MarkWorkSeenResponseObject, error) {
	ok, err := s.deps.Chapters.MarkSeen(ctx, s.libraryViewer(ctx).UserID, req.WorkId)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, NewProblem(http.StatusNotFound, "not_in_library", "the work isn't in your library")
	}
	return apigen.MarkWorkSeen204Response{}, nil
}
