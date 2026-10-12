// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

// libraryViewer is the caller as the library sees them.
func (s *Server) libraryViewer(ctx context.Context) library.Viewer {
	p := auth.FromContext(ctx)
	return library.Viewer{UserID: p.User.ID, Grants: users.Effective(p.User), Admin: p.IsAdmin(), HideUnknown: s.deps.Meta.HideUnknown(ctx)}
}

// requireGrant refuses callers without a grant (admins have them all).
func requireGrant(ctx context.Context, name string, has func(users.Grants) bool) error {
	if has(users.Effective(auth.FromContext(ctx).User)) {
		return nil
	}
	return NewProblem(http.StatusForbidden, "grant_required", "your account may not do this (needs "+name+")")
}

func mayAdd(g users.Grants) bool           { return g.MayAdd }
func mayManageMatches(g users.Grants) bool { return g.MayManageMatches }

func libraryError(ctx context.Context, err error) error {
	var bad *meta.BadFilterError
	var metaErr *meta.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, library.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such work")
	case errors.Is(err, library.ErrNotInLibrary):
		return NewProblem(http.StatusNotFound, "not_in_library", "the work isn't in your library")
	case errors.Is(err, meta.ErrNotFound), errors.Is(err, meta.ErrNotConfigured), errors.As(err, &bad), errors.As(err, &metaErr):
		return metaError(err)
	}
	if p := pluginError(ctx, err); p != err { //nolint:errorlint // identity: was it mapped
		return p
	}
	return metaError(err)
}

func workCover(w db.Work) *string {
	if w.CoverUrl == "" {
		return nil
	}
	u := BasePath + "/works/" + w.ID.String() + "/cover"
	return &u
}

func toWorkSummary(w db.Work, attached int) apigen.WorkSummary {
	out := apigen.WorkSummary{
		Id: w.ID, EntryId: w.MetaEntryID, Title: w.Title, Type: nonEmpty(w.Type), Status: nonEmpty(w.Status),
		ContentRating: apigen.EntryContentRating(w.ContentRating), CoverUrl: workCover(w), AttachedSources: attached,
	}
	if w.Year != nil {
		out.Year = ptr(int(*w.Year))
	}
	return out
}

func (s *Server) toMatchSeries(se db.SourceSeries) apigen.MatchSeries {
	ev := library.EvidenceOf(se)
	out := apigen.MatchSeries{
		Id: se.ID, SourceId: se.SourceID, Key: se.Key, Title: se.Title, Url: nonEmpty(se.Url),
		CoverUrl: s.imageURL(se.SourceID, se.CoverRef), State: apigen.MatchSeriesState(se.State),
		Origin: apigen.MatchSeriesOrigin(se.Origin), Score: se.Score, Priority: int(se.Priority),
		Evidence: apigen.MatchEvidence{
			EntryTitle: nonEmpty(ev.EntryTitle), MatchedTitle: nonEmpty(ev.MatchedTitle), Query: nonEmpty(ev.Query),
			TitleScore: float32(ev.TitleScore), AuthorsMatch: ev.AuthorsMatch, SourceAuthors: nonNil(ev.SourceAuthors),
			ChapterCount: ev.ChapterCount,
		},
	}
	return out
}

// ListLibrary lists the caller's library.
func (s *Server) ListLibrary(ctx context.Context, req apigen.ListLibraryRequestObject) (apigen.ListLibraryResponseObject, error) {
	raw, err := decodePageToken(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	var after *library.Cursor
	if raw != "" {
		title, id, ok := strings.Cut(raw, "\x00")
		uid, uerr := uuid.Parse(id)
		if !ok || uerr != nil {
			return nil, fieldProblem("page_token", "not a token from this API")
		}
		after = &library.Cursor{SortTitle: title, ID: uid}
	}
	page, err := s.deps.Library.List(ctx, s.libraryViewer(ctx), deref(req.Params.Q), after, pageSize(req.Params.PageSize))
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(page.Entries))
	for i, e := range page.Entries {
		ids[i] = e.Work.ID
	}
	newCounts, err := s.deps.Chapters.NewCounts(ctx, auth.FromContext(ctx).User.ID, ids)
	if err != nil {
		return nil, err
	}
	out := apigen.ListLibrary200JSONResponse{Entries: make([]apigen.LibraryEntry, len(page.Entries))}
	for i, e := range page.Entries {
		out.Entries[i] = apigen.LibraryEntry{Work: toWorkSummary(e.Work, e.Attached), AddedAt: e.AddedAt, NewChapters: int(newCounts[e.Work.ID]), AutoDownload: e.AutoDownload}
	}
	if page.Next != nil {
		out.NextPageToken = encodePageToken(page.Next.SortTitle + "\x00" + page.Next.ID.String())
	}
	return out, nil
}

func seriesRef(r *apigen.SeriesRef) *library.SourceRef {
	if r == nil {
		return nil
	}
	return &library.SourceRef{SourceID: r.SourceId, Key: r.Key}
}

// AddToLibrary adds a catalogue entry to the caller's library.
func (s *Server) AddToLibrary(ctx context.Context, req apigen.AddToLibraryRequestObject) (apigen.AddToLibraryResponseObject, error) {
	if err := requireGrant(ctx, "may_add", mayAdd); err != nil {
		return nil, err
	}
	added, err := s.deps.Library.Add(ctx, s.libraryViewer(ctx), req.Body.EntryId, seriesRef(req.Body.Source))
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	entry := apigen.LibraryEntry{Work: toWorkSummary(added.Work, 0), AddedAt: added.AddedAt}
	if added.New {
		return apigen.AddToLibrary201JSONResponse(entry), nil
	}
	return apigen.AddToLibrary200JSONResponse(entry), nil
}

// AddFromSource adds a series seen on a source, or offers entries to pick.
func (s *Server) AddFromSource(ctx context.Context, req apigen.AddFromSourceRequestObject) (apigen.AddFromSourceResponseObject, error) {
	if err := requireGrant(ctx, "may_add", mayAdd); err != nil {
		return nil, err
	}
	res, err := s.deps.Library.AddFromSource(ctx, s.libraryViewer(ctx), *seriesRef(req.Body))
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	out := apigen.AddFromSource200JSONResponse{Candidates: []apigen.EntryCandidate{}}
	if res.Added != nil {
		out.Added = true
		out.Entry = &apigen.LibraryEntry{Work: toWorkSummary(res.Added.Work, 1), AddedAt: res.Added.AddedAt}
	}
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, apigen.EntryCandidate{
			Entry: toSummary(c.Entry), Score: float32(c.Score), MatchedTitle: nonEmpty(c.MatchedTitle), AuthorsMatch: c.AuthorsMatch,
		})
	}
	return out, nil
}

// RemoveFromLibrary takes a work out of the caller's library.
func (s *Server) RemoveFromLibrary(ctx context.Context, req apigen.RemoveFromLibraryRequestObject) (apigen.RemoveFromLibraryResponseObject, error) {
	if err := s.deps.Library.Remove(ctx, s.libraryViewer(ctx), req.WorkId); err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.RemoveFromLibrary204Response{}, nil
}

// GetWork returns a work with its matching per source.
func (s *Server) GetWork(ctx context.Context, req apigen.GetWorkRequestObject) (apigen.GetWorkResponseObject, error) {
	w, err := s.deps.Library.Get(ctx, s.libraryViewer(ctx), req.WorkId)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	attached := 0
	out := apigen.Work{
		Id: w.ID, EntryId: w.MetaEntryID, Title: w.Title, Type: nonEmpty(w.Type), Status: nonEmpty(w.Status),
		ContentRating: apigen.EntryContentRating(w.ContentRating), CoverUrl: workCover(w.Work),
		InLibrary: w.InLibrary, AddedAt: w.AddedAt, Sources: make([]apigen.WorkSourceState, 0, len(w.Sources)),
	}
	if w.Year != nil {
		out.Year = ptr(int(*w.Year))
	}
	for _, ws := range w.Sources {
		st := apigen.WorkSourceState{
			Source: s.toSourceInfo(ws.Source), Status: apigen.WorkSourceStateStatus(ws.Status), Message: nonEmpty(ws.Message),
			Queries: nonNil(ws.Queries), CheckedAt: ws.CheckedAt, Candidates: make([]apigen.MatchSeries, len(ws.Candidates)),
		}
		if ws.Attached != nil {
			ms := s.toMatchSeries(*ws.Attached)
			st.Attached = &ms
			attached++
		}
		for i, c := range ws.Candidates {
			st.Candidates[i] = s.toMatchSeries(c)
		}
		out.Sources = append(out.Sources, st)
	}
	out.AttachedSources = attached
	return apigen.GetWork200JSONResponse(out), nil
}

// GetWorkCover serves a work's cover from the image cache.
func (s *Server) GetWorkCover(ctx context.Context, req apigen.GetWorkCoverRequestObject) (apigen.GetWorkCoverResponseObject, error) {
	w, err := s.deps.Library.Get(ctx, s.libraryViewer(ctx), req.WorkId)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	if w.CoverUrl == "" {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "the work has no cover")
	}
	img, err := s.deps.Images.Get(ctx, w.CoverUrl)
	if err != nil {
		if errors.Is(err, imagecache.ErrNotImage) {
			return nil, NewProblem(http.StatusNotFound, CodeNotFound, "the work has no usable cover")
		}
		return nil, NewProblem(http.StatusBadGateway, "image_unavailable", "the cover couldn't be fetched")
	}
	f, err := os.Open(img.Path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return apigen.GetWorkCover200ImageResponse{
		Body: f, ContentType: img.ContentType, ContentLength: st.Size(),
		Headers: apigen.GetWorkCover200ResponseHeaders{CacheControl: ptr("private, max-age=86400")},
	}, nil
}

// MatchWork queues matching of a work.
func (s *Server) MatchWork(ctx context.Context, req apigen.MatchWorkRequestObject) (apigen.MatchWorkResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, libraryError(ctx, err)
	}
	force := req.Body != nil && deref(req.Body.Force)
	t, err := s.deps.Library.Rematch(ctx, req.WorkId, v.UserID, force)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.MatchWork202JSONResponse{TaskId: t.ID}, nil
}

// AttachSeries attaches a series by source and key, or by URL.
func (s *Server) AttachSeries(ctx context.Context, req apigen.AttachSeriesRequestObject) (apigen.AttachSeriesResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	b := req.Body
	v := s.libraryViewer(ctx)
	var (
		se  db.SourceSeries
		err error
	)
	switch {
	case b.Url != nil && b.SourceId == nil && b.Key == nil:
		se, err = s.deps.Library.AttachURL(ctx, v, req.WorkId, *b.Url)
		if errors.Is(err, plugins.ErrNotFound) {
			return nil, NewProblem(http.StatusNotFound, "url_unknown", "no source recognises this URL")
		}
	case b.Url == nil && b.SourceId != nil && b.Key != nil:
		se, err = s.deps.Library.AttachRef(ctx, v, req.WorkId, library.SourceRef{SourceID: *b.SourceId, Key: *b.Key}, library.OriginManual)
	default:
		return nil, fieldProblem("url", "give either source_id and key, or url")
	}
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.AttachSeries201JSONResponse(s.toMatchSeries(se)), nil
}

// SearchWorkSource searches one of the work's sources to fix its match.
func (s *Server) SearchWorkSource(ctx context.Context, req apigen.SearchWorkSourceRequestObject) (apigen.SearchWorkSourceResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	query, filters, err := searchRequest(req.Body)
	if err != nil {
		return nil, err
	}
	res, err := s.deps.Library.SearchSource(ctx, s.libraryViewer(ctx), req.WorkId, req.SourceId, query, page(req.Body.Page), filters)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	out := s.toPage(req.SourceId, res.Page)
	for i := range out.Series {
		if se, ok := res.Known[out.Series[i].Key]; ok {
			out.Series[i].Match = &apigen.SeriesMatchRef{SeriesId: se.ID, State: apigen.SeriesMatchRefState(se.State)}
		}
	}
	return apigen.SearchWorkSource200JSONResponse(out), nil
}

// UpdateWorkSeries sets a series' priority.
func (s *Server) UpdateWorkSeries(ctx context.Context, req apigen.UpdateWorkSeriesRequestObject) (apigen.UpdateWorkSeriesResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	se, err := s.deps.Library.SetPriority(ctx, s.libraryViewer(ctx), req.WorkId, req.SeriesId, int32(req.Body.Priority)) //nolint:gosec // bounded by the spec
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.UpdateWorkSeries200JSONResponse(s.toMatchSeries(se)), nil
}

// AttachCandidate attaches a candidate.
func (s *Server) AttachCandidate(ctx context.Context, req apigen.AttachCandidateRequestObject) (apigen.AttachCandidateResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	se, err := s.deps.Library.Attach(ctx, s.libraryViewer(ctx), req.WorkId, req.SeriesId)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.AttachCandidate200JSONResponse(s.toMatchSeries(se)), nil
}

// DetachSeries rejects a series for the work.
func (s *Server) DetachSeries(ctx context.Context, req apigen.DetachSeriesRequestObject) (apigen.DetachSeriesResponseObject, error) {
	if err := requireGrant(ctx, "may_manage_matches", mayManageMatches); err != nil {
		return nil, err
	}
	se, err := s.deps.Library.Detach(ctx, s.libraryViewer(ctx), req.WorkId, req.SeriesId)
	if err != nil {
		return nil, libraryError(ctx, err)
	}
	return apigen.DetachSeries200JSONResponse(s.toMatchSeries(se)), nil
}
