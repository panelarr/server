// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/reader"
	"github.com/panelarr/server/internal/store/db"
)

func readerError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, reader.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, reader.ErrNoCopy):
		return NewProblem(http.StatusNotFound, "no_copy", "no copy of this chapter is on a source you may use")
	case errors.Is(err, reader.ErrPageNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such page")
	case errors.Is(err, reader.ErrBadScope):
		return fieldProblem("scope", err.Error())
	case errors.Is(err, reader.ErrTooLarge):
		return NewProblem(http.StatusRequestEntityTooLarge, "too_large", err.Error())
	}
	return chaptersError(ctx, err)
}

func toProgress(p db.Progress) apigen.ChapterProgress {
	out := apigen.ChapterProgress{ChapterId: p.ChapterID, Page: int(p.Page), Completed: p.Completed, UpdatedAt: p.UpdatedAt}
	if p.Pages != nil {
		out.Pages = ptr(int(*p.Pages))
	}
	return out
}

func pageURL(chapterID uuid.UUID, index int, variantID *uuid.UUID) string {
	u := BasePath + "/chapters/" + chapterID.String() + "/pages/" + strconv.Itoa(index)
	if variantID != nil {
		u += "?variant_id=" + variantID.String()
	}
	return u
}

// GetChapterPages returns a chapter's pages for the reader.
func (s *Server) GetChapterPages(ctx context.Context, req apigen.GetChapterPagesRequestObject) (apigen.GetChapterPagesResponseObject, error) {
	p, err := s.deps.Reader.Pages(ctx, s.libraryViewer(ctx), req.ChapterId, req.Params.VariantId)
	if err != nil {
		return nil, readerError(ctx, err)
	}
	visible := map[string]bool{p.Variant.SourceID: true}
	chosen := p.Chapter
	chosen.Chosen = &p.Variant
	out := apigen.GetChapterPages200JSONResponse{
		WorkId: p.WorkID, Chapter: toUnified(chosen, visible), Variant: toVariant(p.Variant),
		Origin: apigen.ChapterPagesOrigin(p.Origin), Pages: make([]apigen.ReaderPage, len(p.Pages)),
		PrevChapterId: p.Prev, NextChapterId: p.Next,
	}
	// Page links name the copy unless it is the chosen one, so a pinned
	// or changed choice can't swap copies in the middle of reading.
	var variant *uuid.UUID
	if req.Params.VariantId != nil || p.Chapter.Chosen == nil || p.Chapter.Chosen.ID != p.Variant.ID {
		variant = &p.Variant.ID
	}
	for i, pg := range p.Pages {
		out.Pages[i] = apigen.ReaderPage{Index: pg.Index, Url: pageURL(req.ChapterId, pg.Index, variant)}
	}
	if p.Progress != nil {
		pr := toProgress(*p.Progress)
		out.Progress = &pr
	}
	return out, nil
}

// GetChapterPage serves one page image.
func (s *Server) GetChapterPage(ctx context.Context, req apigen.GetChapterPageRequestObject) (apigen.GetChapterPageResponseObject, error) {
	ct, data, err := s.deps.Reader.Page(ctx, s.libraryViewer(ctx), req.ChapterId, req.Params.VariantId, req.Index)
	if err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.GetChapterPage200ImageResponse{
		Body: bytes.NewReader(data), ContentType: ct, ContentLength: int64(len(data)),
		Headers: apigen.GetChapterPage200ResponseHeaders{CacheControl: ptr("private, max-age=86400")},
	}, nil
}

// SetChapterProgress saves where the caller is in a chapter.
func (s *Server) SetChapterProgress(ctx context.Context, req apigen.SetChapterProgressRequestObject) (apigen.SetChapterProgressResponseObject, error) {
	b := req.Body
	var pages *int32
	if b.Pages != nil {
		pages = ptr(int32(*b.Pages)) //nolint:gosec // bounded by the spec
	}
	p, err := s.deps.Reader.SetProgress(ctx, s.libraryViewer(ctx), req.ChapterId, int32(b.Page), pages, b.Completed) //nolint:gosec // bounded by the spec
	if err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.SetChapterProgress200JSONResponse(toProgress(p)), nil
}

// GetWorkProgress returns the caller's progress in a work.
func (s *Server) GetWorkProgress(ctx context.Context, req apigen.GetWorkProgressRequestObject) (apigen.GetWorkProgressResponseObject, error) {
	rows, cont, err := s.deps.Reader.WorkProgress(ctx, s.libraryViewer(ctx), req.WorkId)
	if err != nil {
		return nil, readerError(ctx, err)
	}
	out := apigen.GetWorkProgress200JSONResponse{Chapters: make([]apigen.ChapterProgress, len(rows)), ContinueChapterId: cont}
	for i, r := range rows {
		out.Chapters[i] = toProgress(r)
	}
	return out, nil
}

// MarkChaptersRead marks chapters read or unread.
func (s *Server) MarkChaptersRead(ctx context.Context, req apigen.MarkChaptersReadRequestObject) (apigen.MarkChaptersReadResponseObject, error) {
	if err := s.deps.Reader.MarkRead(ctx, s.libraryViewer(ctx), req.WorkId, req.Body.ChapterIds, req.Body.Read); err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.MarkChaptersRead204Response{}, nil
}

// GetMyHistory lists the chapters the caller read.
func (s *Server) GetMyHistory(ctx context.Context, req apigen.GetMyHistoryRequestObject) (apigen.GetMyHistoryResponseObject, error) {
	at, id, err := parseTimeCursor(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	var before *db.Progress
	if at != nil {
		before = &db.Progress{UpdatedAt: *at, ChapterID: id}
	}
	limit := pageSize(req.Params.PageSize)
	items, err := s.deps.Reader.History(ctx, s.libraryViewer(ctx).UserID, before, limit+1)
	if err != nil {
		return nil, err
	}
	out := apigen.GetMyHistory200JSONResponse{Items: []apigen.HistoryItem{}}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1].Progress
		out.NextPageToken = timeCursor(last.UpdatedAt, last.ChapterID)
	}
	for _, it := range items {
		h := apigen.HistoryItem{
			WorkId: it.Progress.WorkID, WorkTitle: it.WorkTitle, ChapterId: it.Chapter.ID, ChapterTitle: it.Chapter.Title,
			Page: int(it.Progress.Page), Completed: it.Progress.Completed, UpdatedAt: it.Progress.UpdatedAt,
		}
		if it.WorkCoverURL != "" {
			h.WorkCoverUrl = ptr(BasePath + "/works/" + it.Progress.WorkID.String() + "/cover")
		}
		if it.Chapter.Number != nil {
			h.ChapterNumber = ptr(float32(*it.Chapter.Number))
		}
		if it.Progress.Pages != nil {
			h.Pages = ptr(int(*it.Progress.Pages))
		}
		out.Items = append(out.Items, h)
	}
	return out, nil
}

// GetReaderSettings returns the caller's reader settings for a place.
func (s *Server) GetReaderSettings(ctx context.Context, req apigen.GetReaderSettingsRequestObject) (apigen.GetReaderSettingsResponseObject, error) {
	st, err := s.deps.Reader.GetSettings(ctx, s.libraryViewer(ctx).UserID, deref(req.Params.SourceId), req.Params.WorkId)
	if err != nil {
		return nil, err
	}
	out := apigen.GetReaderSettings200JSONResponse{Effective: st.Effective}
	if st.Global != nil {
		out.Global = &st.Global
	}
	if st.Source != nil {
		out.Source = &st.Source
	}
	if st.Work != nil {
		out.Work = &st.Work
	}
	return out, nil
}

// PutReaderSettings replaces one level of the caller's reader settings.
func (s *Server) PutReaderSettings(ctx context.Context, req apigen.PutReaderSettingsRequestObject) (apigen.PutReaderSettingsResponseObject, error) {
	if err := s.deps.Reader.PutSettings(ctx, s.libraryViewer(ctx).UserID, req.Scope, *req.Body); err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.PutReaderSettings204Response{}, nil
}

// DeleteReaderSettings removes one level of the caller's reader settings.
func (s *Server) DeleteReaderSettings(ctx context.Context, req apigen.DeleteReaderSettingsRequestObject) (apigen.DeleteReaderSettingsResponseObject, error) {
	if err := s.deps.Reader.DeleteSettings(ctx, s.libraryViewer(ctx).UserID, req.Scope); err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.DeleteReaderSettings204Response{}, nil
}

// GetMyPreferences returns the caller's app preferences.
func (s *Server) GetMyPreferences(ctx context.Context, _ apigen.GetMyPreferencesRequestObject) (apigen.GetMyPreferencesResponseObject, error) {
	prefs, err := s.deps.Reader.Preferences(ctx, s.libraryViewer(ctx).UserID)
	if err != nil {
		return nil, err
	}
	return apigen.GetMyPreferences200JSONResponse(prefs), nil
}

// PutMyPreferences replaces the caller's app preferences.
func (s *Server) PutMyPreferences(ctx context.Context, req apigen.PutMyPreferencesRequestObject) (apigen.PutMyPreferencesResponseObject, error) {
	if err := s.deps.Reader.SetPreferences(ctx, s.libraryViewer(ctx).UserID, *req.Body); err != nil {
		return nil, readerError(ctx, err)
	}
	return apigen.PutMyPreferences204Response{}, nil
}
