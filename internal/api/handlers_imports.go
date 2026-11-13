// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/importer"
	"github.com/panelarr/server/internal/store/db"
)

// importBusyRetry is the Retry-After of an upload refused because another
// backup is being read.
const importBusyRetry = 10

// maxImportsListed caps GET /me/imports.
const maxImportsListed = 100

// importError maps importer errors. Another user's import is answered like
// a missing one.
func importError(ctx context.Context, err error) error {
	var bad *importer.BadBackupError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &bad):
		return NewProblem(http.StatusUnprocessableEntity, "bad_backup", bad.Error())
	case errors.Is(err, importer.ErrFileTooLarge):
		return tooLargeUpload()
	case errors.Is(err, importer.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such import")
	case errors.Is(err, importer.ErrRunning):
		return NewProblem(http.StatusConflict, "import_running", "another import of yours is running: wait for it, or delete it")
	case errors.Is(err, importer.ErrBusy):
		if w := responseWriter(ctx); w != nil {
			w.Header().Set("Retry-After", strconv.Itoa(importBusyRetry))
		}
		return NewProblem(http.StatusTooManyRequests, "busy", "the server is reading another backup; try again in a few seconds")
	}
	return err
}

func (s *Server) toImport(ctx context.Context, imp db.Import) (apigen.Import, error) {
	out := apigen.Import{
		Id: imp.ID, Format: apigen.ImportFormat(imp.Format), State: apigen.ImportState(imp.State),
		Message: nonEmpty(imp.Message), CreatedAt: imp.CreatedAt, FinishedAt: imp.FinishedAt, Outcomes: []apigen.ImportOutcomeCount{},
	}
	if err := json.Unmarshal(imp.Summary, &out.Summary); err != nil {
		return apigen.Import{}, err
	}
	if out.Summary.Categories == nil {
		out.Summary.Categories = []apigen.ImportCategory{}
	}
	if out.Summary.PackageHints == nil {
		out.Summary.PackageHints = []apigen.ImportPackageHint{}
	}
	counts, err := s.deps.Importer.Counts(ctx, imp.ID)
	if err != nil {
		return apigen.Import{}, err
	}
	for _, c := range counts {
		out.Progress.Total += int(c.Series)
		if c.Outcome != importer.OutcomePending {
			out.Progress.Done += int(c.Series)
		}
		out.Outcomes = append(out.Outcomes, apigen.ImportOutcomeCount{
			Outcome: apigen.ImportOutcome(c.Outcome), Series: int(c.Series), ChaptersRead: int(c.ChaptersRead), Bookmarks: int(c.Bookmarks),
		})
	}
	return out, nil
}

// ListMyImports lists the caller's imports.
func (s *Server) ListMyImports(ctx context.Context, _ apigen.ListMyImportsRequestObject) (apigen.ListMyImportsResponseObject, error) {
	imps, err := s.deps.Importer.List(ctx, callerID(ctx), maxImportsListed)
	if err != nil {
		return nil, err
	}
	out := apigen.ListMyImports200JSONResponse{Imports: make([]apigen.Import, len(imps))}
	for i, imp := range imps {
		if out.Imports[i], err = s.toImport(ctx, imp); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateMyImport reads an uploaded backup and starts its preview.
func (s *Server) CreateMyImport(ctx context.Context, req apigen.CreateMyImportRequestObject) (apigen.CreateMyImportResponseObject, error) {
	if req.Body == nil {
		return nil, fieldProblem("file", "send the backup as the multipart field file")
	}
	for {
		part, err := req.Body.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, fieldProblem("file", "send the backup as the multipart field file")
		}
		if err != nil {
			var mb *http.MaxBytesError
			if errors.As(err, &mb) {
				return nil, tooLargeUpload()
			}
			return nil, NewProblem(http.StatusBadRequest, CodeInvalidRequest, "not a valid multipart body")
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		imp, created, err := s.deps.Importer.Upload(ctx, s.libraryViewer(ctx), part)
		_ = part.Close()
		if err != nil {
			return nil, importError(ctx, err)
		}
		out, err := s.toImport(ctx, imp)
		if err != nil {
			return nil, err
		}
		if created {
			return apigen.CreateMyImport202JSONResponse(out), nil
		}
		return apigen.CreateMyImport200JSONResponse(out), nil
	}
}

func tooLargeUpload() error {
	return NewProblem(http.StatusRequestEntityTooLarge, "too_large", "the file is over "+strconv.Itoa(importer.MaxFileSize>>20)+" MiB")
}

// GetMyImport returns one of the caller's imports.
func (s *Server) GetMyImport(ctx context.Context, req apigen.GetMyImportRequestObject) (apigen.GetMyImportResponseObject, error) {
	imp, err := s.deps.Importer.Get(ctx, callerID(ctx), req.Id)
	if err != nil {
		return nil, importError(ctx, err)
	}
	out, err := s.toImport(ctx, imp)
	if err != nil {
		return nil, err
	}
	return apigen.GetMyImport200JSONResponse(out), nil
}

// DeleteMyImport deletes one of the caller's imports.
func (s *Server) DeleteMyImport(ctx context.Context, req apigen.DeleteMyImportRequestObject) (apigen.DeleteMyImportResponseObject, error) {
	if err := s.deps.Importer.Delete(ctx, callerID(ctx), req.Id); err != nil {
		return nil, importError(ctx, err)
	}
	return apigen.DeleteMyImport204Response{}, nil
}

// ListMyImportItems lists the series of one of the caller's imports.
func (s *Server) ListMyImportItems(ctx context.Context, req apigen.ListMyImportItemsRequestObject) (apigen.ListMyImportItemsResponseObject, error) {
	raw, err := decodePageToken(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	after := int64(-1)
	if raw != "" {
		after, err = strconv.ParseInt(raw, 10, 32)
		if err != nil || after < 0 {
			return nil, fieldProblem("page_token", "not a token from this API")
		}
	}
	outcome := ""
	if req.Params.Outcome != nil {
		outcome = string(*req.Params.Outcome)
	}
	page, err := s.deps.Importer.Items(ctx, callerID(ctx), req.Id, outcome, int32(after), pageSize(req.Params.PageSize))
	if err != nil {
		return nil, importError(ctx, err)
	}
	out := apigen.ListMyImportItems200JSONResponse{Items: make([]apigen.ImportItem, len(page.Items))}
	for i, it := range page.Items {
		item := apigen.ImportItem{
			Idx: int(it.Idx), Title: it.Title, Author: it.Author, Artist: it.Artist, SourceName: it.SourceName,
			SourceCompatId: it.SourceCompatID, SeriesKey: it.SeriesKey, AddedAt: it.AddedAt, Categories: nonNil(it.Categories),
			Outcome: apigen.ImportOutcome(it.Outcome), Message: nonEmpty(it.Message), EntryId: nonEmpty(it.EntryID),
			SourceId: nonEmpty(it.SourceID), Chapters: int(it.Chapters), ChaptersRead: int(it.ChaptersRead),
			Bookmarks: int(it.Bookmarks), Candidates: []apigen.ImportCandidate{},
		}
		if it.WorkID.Valid {
			item.WorkId = ptr(it.WorkID.UUID)
		}
		var cands []importer.Candidate
		if err := json.Unmarshal(it.Candidates, &cands); err != nil {
			return nil, err
		}
		for _, c := range cands {
			ic := apigen.ImportCandidate{EntryId: c.EntryID, Title: c.Title, Score: float32(c.Score), MatchedTitle: nonEmpty(c.MatchedTitle), AuthorsMatch: c.AuthorsMatch}
			if c.Year > 0 {
				ic.Year = ptr(int(c.Year))
			}
			item.Candidates = append(item.Candidates, ic)
		}
		out.Items[i] = item
	}
	if page.Next >= 0 {
		out.NextPageToken = encodePageToken(strconv.Itoa(int(page.Next)))
	}
	return out, nil
}
