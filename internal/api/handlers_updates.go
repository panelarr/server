// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/jobs"
)

// The updates feed: new chapters across the caller's library.

// updatesJob is the scheduled check of everyone's library.
const updatesJob = "updates.check"

func downloadState(u chapters.Update) apigen.UpdateItemDownloadState {
	switch {
	case u.Downloaded:
		return apigen.UpdateItemDownloadStateDownloaded
	case u.DownloadState == "running":
		return apigen.UpdateItemDownloadStateDownloading
	case u.DownloadState == "queued":
		return apigen.UpdateItemDownloadStateQueued
	}
	return apigen.UpdateItemDownloadStateNone
}

func toUpdateItem(u chapters.Update) apigen.UpdateItem {
	c := u.Chapter
	out := apigen.UpdateItem{
		ChapterId: c.ID, WorkId: c.WorkID, WorkTitle: u.WorkTitle, ChapterTitle: c.Title,
		ChapterDisplayTitle: chapters.DisplayTitle(c.Title), ChapterVolume: nonEmpty(c.Volume),
		New: u.New, Read: u.Completed, DownloadState: downloadState(u),
	}
	if c.AppearedAt != nil {
		out.AppearedAt = *c.AppearedAt
	}
	if u.WorkCoverUrl != "" {
		out.WorkCoverUrl = ptr(BasePath + "/works/" + c.WorkID.String() + "/cover")
	}
	if c.Number != nil {
		out.ChapterNumber = ptr(float32(*c.Number))
	}
	if u.ProgressPage != nil {
		out.Page = ptr(int(*u.ProgressPage))
	}
	if u.ProgressPages != nil {
		out.Pages = ptr(int(*u.ProgressPages))
	}
	return out
}

// GetMyUpdates lists the chapters that appeared in the caller's library.
func (s *Server) GetMyUpdates(ctx context.Context, req apigen.GetMyUpdatesRequestObject) (apigen.GetMyUpdatesResponseObject, error) {
	at, id, err := parseTimeCursor(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	var before *chapters.UpdateCursor
	if at != nil {
		before = &chapters.UpdateCursor{AppearedAt: *at, ChapterID: id}
	}
	v := s.libraryViewer(ctx)
	works, err := s.deps.Library.VisibleWorks(ctx, v)
	if err != nil {
		return nil, err
	}
	limit := pageSize(req.Params.PageSize)
	list, err := s.deps.Chapters.Updates(ctx, v.UserID, works, before, limit+1)
	if err != nil {
		return nil, err
	}
	out := apigen.GetMyUpdates200JSONResponse{Items: make([]apigen.UpdateItem, 0, len(list))}
	if len(list) > limit {
		list = list[:limit]
		last := list[len(list)-1].Chapter
		out.NextPageToken = timeCursor(*last.AppearedAt, last.ID)
	}
	for _, u := range list {
		out.Items = append(out.Items, toUpdateItem(u))
	}
	return out, nil
}

// GetMyUpdatesStatus says how the chapter checks of the caller's library
// stand.
func (s *Server) GetMyUpdatesStatus(ctx context.Context, _ apigen.GetMyUpdatesStatusRequestObject) (apigen.GetMyUpdatesStatusResponseObject, error) {
	works, err := s.deps.Library.VisibleWorks(ctx, s.libraryViewer(ctx))
	if err != nil {
		return nil, err
	}
	st, err := s.deps.Chapters.ChecksOf(ctx, works)
	if err != nil {
		return nil, err
	}
	out := apigen.GetMyUpdatesStatus200JSONResponse{LastCheckedAt: st.LastCheckedAt, Checking: st.Checking, Failing: st.Failing}
	job, err := s.deps.Jobs.Get(ctx, updatesJob)
	switch {
	case err == nil:
		out.NextCheckAt = job.NextRunAt
	case !errors.Is(err, jobs.ErrNotFound):
		return nil, err
	}
	return out, nil
}

// RefreshMyUpdates queues chapter checks of the caller's library.
func (s *Server) RefreshMyUpdates(ctx context.Context, _ apigen.RefreshMyUpdatesRequestObject) (apigen.RefreshMyUpdatesResponseObject, error) {
	v := s.libraryViewer(ctx)
	works, err := s.deps.Library.VisibleWorks(ctx, v)
	if err != nil {
		return nil, err
	}
	n, err := s.deps.Chapters.RefreshWorks(ctx, works, v.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.RefreshMyUpdates202JSONResponse{Queued: n}, nil
}
