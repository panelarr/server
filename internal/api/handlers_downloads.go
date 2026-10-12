// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/downloads"
	"github.com/panelarr/server/internal/storage"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

func mayDownload(g users.Grants) bool { return g.MayDownload }

// storageError maps storage errors; failures of the write/read probe are
// 422 with the reason.
func storageError(err error, probing bool) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrUnknown):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such storage")
	case errors.Is(err, storage.ErrIDTaken):
		return NewProblem(http.StatusConflict, "storage_id_taken", "a storage with this ID exists")
	case errors.Is(err, storage.ErrInvalidID):
		return fieldProblem("id", err.Error())
	case errors.Is(err, storage.ErrInUse):
		return NewProblem(http.StatusConflict, "storage_in_use", err.Error())
	case errors.Is(err, storage.ErrIsDefault):
		return NewProblem(http.StatusConflict, "storage_default", err.Error())
	case errors.Is(err, storage.ErrRootNeeded):
		return fieldProblem("root", err.Error())
	case errors.Is(err, storage.ErrBadConfig):
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, downloads.ErrSameTarget):
		return fieldProblem("to", err.Error())
	case probing:
		return NewProblem(http.StatusUnprocessableEntity, "storage_check_failed", err.Error())
	}
	return err
}

func (s *Server) toStorage(ctx context.Context, row db.Storage) (apigen.StorageInfo, error) {
	n, err := s.deps.Store.CountFilesByStorage(ctx, row.ID)
	if err != nil {
		return apigen.StorageInfo{}, err
	}
	out := apigen.StorageInfo{
		Id: row.ID, Name: row.Name, Kind: apigen.StorageInfoKind(row.Kind), Default: row.IsDefault,
		HasSecret: row.Secret != "", Files: n,
	}
	cfg, err := storage.ConfigOf(row)
	if err != nil {
		return apigen.StorageInfo{}, err
	}
	if c := cfg.Local; c != nil {
		out.Root = nonEmpty(c.Root)
	}
	if c := cfg.S3; c != nil {
		out.Endpoint, out.Region, out.Bucket = nonEmpty(c.Endpoint), nonEmpty(c.Region), nonEmpty(c.Bucket)
		out.Prefix, out.AccessKey, out.PathStyle = nonEmpty(c.Prefix), nonEmpty(c.AccessKey), &c.PathStyle
	}
	return out, nil
}

// settingsInto applies the request's settings to a kind's config.
func settingsInto(kind string, cur storage.Config, root, endpoint, region, bucket, prefix, accessKey *string, pathStyle *bool) storage.Config {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	switch kind {
	case storage.KindLocal:
		c := storage.LocalConfig{}
		if cur.Local != nil {
			c = *cur.Local
		}
		set(&c.Root, root)
		return storage.Config{Local: &c}
	default:
		c := storage.S3Config{}
		if cur.S3 != nil {
			c = *cur.S3
		}
		set(&c.Endpoint, endpoint)
		set(&c.Region, region)
		set(&c.Bucket, bucket)
		set(&c.Prefix, prefix)
		set(&c.AccessKey, accessKey)
		if pathStyle != nil {
			c.PathStyle = *pathStyle
		}
		return storage.Config{S3: &c}
	}
}

// ListStorages lists the storages.
func (s *Server) ListStorages(ctx context.Context, _ apigen.ListStoragesRequestObject) (apigen.ListStoragesResponseObject, error) {
	rows, err := s.deps.Storages.List(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListStorages200JSONResponse{Storages: make([]apigen.StorageInfo, len(rows))}
	for i, r := range rows {
		if out.Storages[i], err = s.toStorage(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateStorage adds a storage after checking it works.
func (s *Server) CreateStorage(ctx context.Context, req apigen.CreateStorageRequestObject) (apigen.CreateStorageResponseObject, error) {
	b := req.Body
	kind := string(b.Kind)
	cfg := settingsInto(kind, storage.Config{}, b.Root, b.Endpoint, b.Region, b.Bucket, b.Prefix, b.AccessKey, b.PathStyle)
	row, err := s.deps.Storages.Create(ctx, storage.NewStorage{ID: b.Id, Name: b.Name, Kind: kind, Config: cfg, Secret: deref(b.SecretKey)})
	if err != nil {
		return nil, storageError(err, true)
	}
	out, err := s.toStorage(ctx, row)
	if err != nil {
		return nil, err
	}
	return apigen.CreateStorage201JSONResponse(out), nil
}

// UpdateStorage changes a storage or makes it the default.
func (s *Server) UpdateStorage(ctx context.Context, req apigen.UpdateStorageRequestObject) (apigen.UpdateStorageResponseObject, error) {
	b := req.Body
	row, err := s.deps.Storages.Get(ctx, req.StorageId)
	if err != nil {
		return nil, storageError(err, false)
	}
	changes := b.Name != nil || b.Root != nil || b.Endpoint != nil || b.Region != nil || b.Bucket != nil ||
		b.Prefix != nil || b.AccessKey != nil || b.PathStyle != nil || b.SecretKey != nil
	if changes {
		cur, err := storage.ConfigOf(row)
		if err != nil {
			return nil, err
		}
		cfg := settingsInto(row.Kind, cur, b.Root, b.Endpoint, b.Region, b.Bucket, b.Prefix, b.AccessKey, b.PathStyle)
		if _, err := s.deps.Storages.Update(ctx, req.StorageId, deref(b.Name), cfg, deref(b.SecretKey)); err != nil {
			return nil, storageError(err, true)
		}
	}
	if b.Default != nil && *b.Default {
		if err := s.deps.Storages.SetDefault(ctx, req.StorageId); err != nil {
			return nil, storageError(err, false)
		}
	}
	row, err = s.deps.Storages.Get(ctx, req.StorageId)
	if err != nil {
		return nil, err
	}
	out, err := s.toStorage(ctx, row)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateStorage200JSONResponse(out), nil
}

// DeleteStorage removes an empty storage.
func (s *Server) DeleteStorage(ctx context.Context, req apigen.DeleteStorageRequestObject) (apigen.DeleteStorageResponseObject, error) {
	if err := s.deps.Storages.Delete(ctx, req.StorageId); err != nil {
		return nil, storageError(err, false)
	}
	return apigen.DeleteStorage204Response{}, nil
}

// TestStorage checks a storage now.
func (s *Server) TestStorage(ctx context.Context, req apigen.TestStorageRequestObject) (apigen.TestStorageResponseObject, error) {
	if _, err := s.deps.Storages.Get(ctx, req.StorageId); err != nil {
		return nil, storageError(err, false)
	}
	if err := s.deps.Storages.Test(ctx, req.StorageId); err != nil {
		return apigen.TestStorage200JSONResponse{Ok: false, Error: ptr(err.Error())}, nil
	}
	return apigen.TestStorage200JSONResponse{Ok: true}, nil
}

// MoveStorage queues moving every file to another storage.
func (s *Server) MoveStorage(ctx context.Context, req apigen.MoveStorageRequestObject) (apigen.MoveStorageResponseObject, error) {
	t, err := s.deps.Downloads.Move(ctx, auth.FromContext(ctx).User.ID, req.StorageId, req.Body.To)
	if err != nil {
		return nil, storageError(err, false)
	}
	return apigen.MoveStorage202JSONResponse{TaskId: t.ID}, nil
}

// DownloadChapters queues downloads of a work's chapters.
func (s *Server) DownloadChapters(ctx context.Context, req apigen.DownloadChaptersRequestObject) (apigen.DownloadChaptersResponseObject, error) {
	if err := requireGrant(ctx, "may_download", mayDownload); err != nil {
		return nil, err
	}
	v := s.libraryViewer(ctx)
	if _, err := s.deps.Library.Get(ctx, v, req.WorkId); err != nil {
		return nil, libraryError(ctx, err)
	}
	b := req.Body
	var ids []uuid.UUID
	switch {
	case b.ChapterIds != nil && b.Scope == nil:
		ids = *b.ChapterIds
	case b.ChapterIds == nil && b.Scope != nil:
		list, err := s.deps.Chapters.List(ctx, req.WorkId, v.UserID)
		if err != nil {
			return nil, err
		}
		read := map[uuid.UUID]bool{}
		if *b.Scope == apigen.DownloadChaptersRequestScopeUnread {
			prog, err := s.deps.Store.WorkProgress(ctx, db.WorkProgressParams{UserID: v.UserID, WorkID: req.WorkId})
			if err != nil {
				return nil, err
			}
			for _, p := range prog {
				read[p.ChapterID] = p.Completed
			}
		}
		for _, c := range list {
			if c.Available && !read[c.ID] {
				ids = append(ids, c.ID)
			}
		}
	default:
		return nil, fieldProblem("scope", "give either chapter_ids or scope")
	}
	q, err := s.deps.Downloads.Queue(ctx, v.UserID, req.WorkId, ids)
	if errors.Is(err, downloads.ErrNotFound) {
		return nil, fieldProblem("chapter_ids", err.Error())
	}
	if err != nil {
		return nil, err
	}
	return apigen.DownloadChapters202JSONResponse{Queued: q.Queued, Downloaded: q.Downloaded, Unavailable: q.Unavailable}, nil
}

// ListDownloads lists downloads.
func (s *Server) ListDownloads(ctx context.Context, req apigen.ListDownloadsRequestObject) (apigen.ListDownloadsResponseObject, error) {
	at, id, err := parseTimeCursor(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	var user *uuid.UUID
	if !p.IsAdmin() || !deref(req.Params.All) {
		user = &p.User.ID
	}
	var before *db.Task
	if at != nil {
		before = &db.Task{CreatedAt: *at, ID: id}
	}
	state := ""
	if req.Params.State != nil {
		state = string(*req.Params.State)
	}
	limit := pageSize(req.Params.PageSize)
	items, err := s.deps.Downloads.List(ctx, user, state, before, limit+1)
	if err != nil {
		return nil, err
	}
	out := apigen.ListDownloads200JSONResponse{Items: []apigen.DownloadItem{}}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1].Task
		out.NextPageToken = timeCursor(last.CreatedAt, last.ID)
	}
	for _, it := range items {
		d := apigen.DownloadItem{Task: toTask(it.Task), WorkId: it.WorkID, WorkTitle: it.WorkTitle, ChapterId: it.ChapterID, ChapterTitle: it.ChapterTitle}
		if it.ChapterNumber != nil {
			d.ChapterNumber = ptr(float32(*it.ChapterNumber))
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}

// SetAutoDownload turns automatic downloads of new chapters on or off.
func (s *Server) SetAutoDownload(ctx context.Context, req apigen.SetAutoDownloadRequestObject) (apigen.SetAutoDownloadResponseObject, error) {
	if req.Body.Enabled {
		if err := requireGrant(ctx, "may_download", mayDownload); err != nil {
			return nil, err
		}
	}
	if err := s.deps.Downloads.SetAutoDownload(ctx, s.libraryViewer(ctx).UserID, req.WorkId, req.Body.Enabled); err != nil {
		if errors.Is(err, downloads.ErrNotFound) {
			return nil, NewProblem(http.StatusNotFound, "not_in_library", "the work isn't in your library")
		}
		return nil, err
	}
	return apigen.SetAutoDownload204Response{}, nil
}

// DeleteChapterFile deletes a chapter's downloaded file.
func (s *Server) DeleteChapterFile(ctx context.Context, req apigen.DeleteChapterFileRequestObject) (apigen.DeleteChapterFileResponseObject, error) {
	if err := s.deps.Downloads.DeleteFile(ctx, req.ChapterId); err != nil {
		if errors.Is(err, downloads.ErrNotStored) {
			return nil, NewProblem(http.StatusNotFound, CodeNotFound, "the chapter isn't downloaded")
		}
		return nil, err
	}
	return apigen.DeleteChapterFile204Response{}, nil
}
