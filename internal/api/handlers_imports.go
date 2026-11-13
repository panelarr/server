// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/importer"
	"github.com/panelarr/server/internal/store/db"
)

// importBusyRetry is the Retry-After of an upload refused because another
// backup is being read.
const importBusyRetry = 10

// importError maps importer errors. Another user's import is answered like
// a missing one.
func importError(ctx context.Context, err error) error {
	var bad *importer.BadBackupError
	var na *importer.NotAllowedError
	switch {
	case errors.As(err, &na):
		return NewProblem(http.StatusUnprocessableEntity, "not_allowed", na.Reason)
	case err == nil:
		return nil
	case errors.As(err, &bad):
		return NewProblem(http.StatusUnprocessableEntity, "bad_backup", bad.Error())
	case errors.Is(err, importer.ErrFileTooLarge):
		return tooLargeUpload()
	case errors.Is(err, importer.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such import")
	case errors.Is(err, importer.ErrRunning):
		return NewProblem(http.StatusConflict, "import_running", "an import of yours is running: wait for it, cancel it or delete it")
	case errors.Is(err, importer.ErrTooMany):
		return NewProblem(http.StatusConflict, "too_many_imports", "you keep "+strconv.Itoa(importer.MaxImports)+" imports already: delete one first")
	case errors.Is(err, importer.ErrState):
		return NewProblem(http.StatusConflict, "import_state", "only an import whose preview finished can be applied: in preview, or cancelled or failed after it")
	case errors.Is(err, importer.ErrNotRunning):
		return NewProblem(http.StatusConflict, "import_not_running", "the import isn't running")
	case errors.Is(err, importer.ErrNoSuchItem):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such series in the import")
	case errors.Is(err, importer.ErrItemState):
		return NewProblem(http.StatusConflict, "import_item_state", "the series can't be resolved that way in its state")
	case errors.Is(err, importer.ErrNoSuchEntry):
		return fieldProblem("entry_id", "no such catalogue entry")
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
		Results: []apigen.ImportResultCount{},
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
	results, err := s.deps.Importer.ResultCounts(ctx, imp.ID)
	if err != nil {
		return apigen.Import{}, err
	}
	for _, r := range results {
		out.Results = append(out.Results, apigen.ImportResultCount{Result: apigen.ImportResult(r.Result), Series: int(r.Series)})
	}
	if imp.State == importer.StateApplying {
		p, err := s.deps.Importer.ApplyProgress(ctx, imp.ID)
		if err != nil {
			return apigen.Import{}, err
		}
		out.Progress.Done, out.Progress.Total = int(p.Done), int(p.Total)
	}
	for _, c := range counts {
		if imp.State != importer.StateApplying {
			out.Progress.Total += int(c.Series)
			if c.Outcome != importer.OutcomePending {
				out.Progress.Done += int(c.Series)
			}
		}
		out.Outcomes = append(out.Outcomes, apigen.ImportOutcomeCount{
			Outcome: apigen.ImportOutcome(c.Outcome), Series: int(c.Series), ChaptersRead: int(c.ChaptersRead), Bookmarks: int(c.Bookmarks),
		})
	}
	return out, nil
}

// ListMyImports lists the caller's imports.
func (s *Server) ListMyImports(ctx context.Context, req apigen.ListMyImportsRequestObject) (apigen.ListMyImportsResponseObject, error) {
	raw, err := decodePageToken(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	var after *importer.ImportCursor
	if raw != "" {
		c, ok := parseImportCursor(raw)
		if !ok {
			return nil, fieldProblem("page_token", "not a token from this API")
		}
		after = &c
	}
	page, err := s.deps.Importer.List(ctx, callerID(ctx), after, pageSize(req.Params.PageSize))
	if err != nil {
		return nil, err
	}
	out := apigen.ListMyImports200JSONResponse{Imports: make([]apigen.Import, len(page.Imports))}
	for i, imp := range page.Imports {
		if out.Imports[i], err = s.toImport(ctx, imp); err != nil {
			return nil, err
		}
	}
	if page.Next != nil {
		out.NextPageToken = encodePageToken(page.Next.CreatedAt.UTC().Format(time.RFC3339Nano) + " " + page.Next.ID.String())
	}
	return out, nil
}

func parseImportCursor(raw string) (importer.ImportCursor, bool) {
	at, id, ok := strings.Cut(raw, " ")
	if !ok {
		return importer.ImportCursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return importer.ImportCursor{}, false
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return importer.ImportCursor{}, false
	}
	return importer.ImportCursor{CreatedAt: t, ID: u}, true
}

// ApplyMyImport starts applying one of the caller's imports.
func (s *Server) ApplyMyImport(ctx context.Context, req apigen.ApplyMyImportRequestObject) (apigen.ApplyMyImportResponseObject, error) {
	imp, err := s.deps.Importer.Apply(ctx, callerID(ctx), req.Id)
	if err != nil {
		return nil, importError(ctx, err)
	}
	out, err := s.toImport(ctx, imp)
	if err != nil {
		return nil, err
	}
	return apigen.ApplyMyImport202JSONResponse(out), nil
}

// CancelMyImport stops one of the caller's imports while it runs.
func (s *Server) CancelMyImport(ctx context.Context, req apigen.CancelMyImportRequestObject) (apigen.CancelMyImportResponseObject, error) {
	imp, err := s.deps.Importer.Cancel(ctx, callerID(ctx), req.Id)
	if err != nil {
		return nil, importError(ctx, err)
	}
	out, err := s.toImport(ctx, imp)
	if err != nil {
		return nil, err
	}
	return apigen.CancelMyImport200JSONResponse(out), nil
}

// ResolveMyImportItem chooses the work of one of an import's series, or
// skips it.
func (s *Server) ResolveMyImportItem(ctx context.Context, req apigen.ResolveMyImportItemRequestObject) (apigen.ResolveMyImportItemResponseObject, error) {
	if req.Body == nil {
		return nil, fieldProblem("entry_id", "give entry_id or skip")
	}
	var r importer.Resolution
	switch {
	case req.Body.Skip != nil && *req.Body.Skip && req.Body.EntryId != nil:
		return nil, fieldProblem("skip", "give entry_id or skip, not both")
	case req.Body.Skip != nil && *req.Body.Skip:
		r.Skip = true
	case req.Body.EntryId != nil && strings.TrimSpace(*req.Body.EntryId) != "":
		r.EntryID = *req.Body.EntryId
	default:
		return nil, fieldProblem("entry_id", "give entry_id or skip")
	}
	it, err := s.deps.Importer.Resolve(ctx, callerID(ctx), req.Id, req.Idx, r)
	if err != nil {
		return nil, importError(ctx, err)
	}
	item, err := toImportItem(db.ListImportItemsRow{
		ImportID: it.ImportID, Idx: it.Idx, Title: it.Title, Author: it.Author, Artist: it.Artist, SourceCompatID: it.SourceCompatID,
		SourceName: it.SourceName, SeriesKey: it.SeriesKey, AddedAt: it.AddedAt, Categories: it.Categories, Outcome: it.Outcome,
		WorkID: it.WorkID, EntryID: it.EntryID, SourceID: it.SourceID, SourceState: it.SourceState, Candidates: it.Candidates,
		ChaptersRead: it.ChaptersRead, Bookmarks: it.Bookmarks, Pending: it.Pending, Message: it.Message, Result: it.Result, AppliedAt: it.AppliedAt,
		Placed: it.Placed, Unplaced: it.Unplaced, Chapters: chapterCount(it.Chapters),
	})
	if err != nil {
		return nil, err
	}
	return apigen.ResolveMyImportItem200JSONResponse(item), nil
}

// chapterCount counts an import series' stored chapters.
func chapterCount(raw []byte) int32 {
	var chs []json.RawMessage
	if json.Unmarshal(raw, &chs) != nil {
		return 0
	}
	return int32(len(chs)) //nolint:gosec // a series' chapters
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
	result := ""
	if req.Params.Result != nil {
		result = string(*req.Params.Result)
	}
	page, err := s.deps.Importer.Items(ctx, callerID(ctx), req.Id, outcome, result, int32(after), pageSize(req.Params.PageSize))
	if err != nil {
		return nil, importError(ctx, err)
	}
	out := apigen.ListMyImportItems200JSONResponse{Items: make([]apigen.ImportItem, len(page.Items))}
	for i, it := range page.Items {
		if out.Items[i], err = toImportItem(it); err != nil {
			return nil, err
		}
	}
	if page.Next >= 0 {
		out.NextPageToken = encodePageToken(strconv.Itoa(int(page.Next)))
	}
	return out, nil
}

func toImportItem(it db.ListImportItemsRow) (apigen.ImportItem, error) {
	item := apigen.ImportItem{
		Idx: int(it.Idx), Title: it.Title, Author: it.Author, Artist: it.Artist, SourceName: it.SourceName,
		SourceCompatId: it.SourceCompatID, SeriesKey: it.SeriesKey, AddedAt: it.AddedAt, Categories: nonNil(it.Categories),
		Outcome: apigen.ImportOutcome(it.Outcome), Message: nonEmpty(it.Message), EntryId: nonEmpty(it.EntryID),
		SourceId: nonEmpty(it.SourceID), Chapters: int(it.Chapters), ChaptersRead: int(it.ChaptersRead),
		Bookmarks: int(it.Bookmarks), Candidates: []apigen.ImportCandidate{}, AppliedAt: it.AppliedAt,
		Placed: int(it.Placed), Unplaced: int(it.Unplaced), Pending: int(it.Pending),
	}
	if it.Result != "" {
		item.Result = ptr(apigen.ImportResult(it.Result))
	}
	if it.WorkID.Valid {
		item.WorkId = ptr(it.WorkID.UUID)
	}
	var cands []importer.Candidate
	if err := json.Unmarshal(it.Candidates, &cands); err != nil {
		return apigen.ImportItem{}, err
	}
	for _, c := range cands {
		ic := apigen.ImportCandidate{EntryId: c.EntryID, Title: c.Title, Score: float32(c.Score), MatchedTitle: nonEmpty(c.MatchedTitle), AuthorsMatch: c.AuthorsMatch}
		if c.Year > 0 {
			ic.Year = ptr(int(c.Year))
		}
		item.Candidates = append(item.Candidates, ic)
	}
	return item, nil
}
