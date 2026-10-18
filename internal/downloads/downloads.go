// SPDX-License-Identifier: GPL-3.0-or-later

// Package downloads downloads chapters into the shared library: one file
// per chapter, stored once for everyone, as a CBZ with ComicInfo.xml, on
// the default storage. Downloads are tasks: de-duplicated across users,
// retried with back-off, and capped per source across all users.
package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/cbz"
	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/storage"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

// Task kinds.
const (
	KindDownload = "download.chapter"
	KindMove     = "storage.move"
)

// Errors callers map to responses.
var (
	ErrNotFound   = errors.New("downloads: not found")
	ErrNotStored  = errors.New("downloads: the chapter isn't downloaded")
	ErrSameTarget = errors.New("downloads: the files are on that storage already")
)

const settingTemplate = "downloads.naming_template"

var adminView = plugins.Viewer{Admin: true, Grants: users.AdminGrants()}

// Service downloads chapters.
type Service struct {
	st       *store.Store
	host     *plugins.Host
	storages *storage.Manager
	meta     *meta.Service
	queue    *queue.Queue
	limits   *limits.Registry
	hub      *events.Hub
	log      *slog.Logger
	// tmpDir holds archives while they are built.
	tmpDir string
}

// NewService returns the service and registers its task kinds on q.
func NewService(st *store.Store, host *plugins.Host, storages *storage.Manager, m *meta.Service, q *queue.Queue,
	lim *limits.Registry, hub *events.Hub, tmpDir string, log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if lim == nil {
		lim = limits.NewRegistry()
	}
	s := &Service{st: st, host: host, storages: storages, meta: m, queue: q, limits: lim, hub: hub, log: log, tmpDir: tmpDir}
	q.Register(KindDownload, queue.Kind{Handler: s.handleDownload, Concurrency: 4, MaxAttempts: 5})
	q.Register(KindMove, queue.Kind{Handler: s.handleMove, Concurrency: 1, MaxAttempts: 3})
	return s
}

type downloadPayload struct {
	ChapterID uuid.UUID `json:"chapter_id"`
}

// Queued is the outcome of asking for downloads.
type Queued struct {
	Queued      int
	Downloaded  int
	Unavailable int
}

// Queue asks for chapters of a work to be downloaded. Chapters that are
// downloaded or queued already (by anyone) aren't queued again.
func (s *Service) Queue(ctx context.Context, by uuid.UUID, workID uuid.UUID, chapterIDs []uuid.UUID) (Queued, error) {
	have, err := s.st.DownloadedChapterIDs(ctx, workID)
	if err != nil {
		return Queued{}, err
	}
	all, err := s.st.ListChapters(ctx, workID)
	if err != nil {
		return Queued{}, err
	}
	var out Queued
	for _, id := range chapterIDs {
		i := slices.IndexFunc(all, func(c db.Chapter) bool { return c.ID == id })
		if i < 0 {
			return Queued{}, fmt.Errorf("%w: chapter %s isn't part of the work", ErrNotFound, id)
		}
		ch := all[i]
		switch {
		case slices.Contains(have, id):
			out.Downloaded++
			continue
		case !ch.Available || !ch.PreferredVariantID.Valid:
			out.Unavailable++
			continue
		}
		vs, err := s.st.ChapterVariants(ctx, id)
		if err != nil {
			return Queued{}, err
		}
		limitKey := ""
		for _, v := range vs {
			if v.SourceChapter.ID == ch.PreferredVariantID.UUID {
				limitKey = "source:" + v.SourceID
			}
		}
		if _, _, err := s.queue.Enqueue(ctx, queue.NewTask{
			Kind: KindDownload, Payload: downloadPayload{ChapterID: id}, DedupeKey: "download:" + id.String(),
			LimitKey: limitKey, CreatedBy: by,
		}); err != nil {
			return Queued{}, err
		}
		out.Queued++
	}
	return out, nil
}

// NewChapters queues downloads of a work's new chapters for followers who
// asked for them and may download.
func (s *Service) NewChapters(ctx context.Context, workID uuid.UUID, chapterIDs []uuid.UUID) {
	ids, err := s.st.AutoDownloadUsers(ctx, workID)
	if err != nil {
		s.log.WarnContext(ctx, "auto download users", "work", workID, "err", err)
		return
	}
	for _, uid := range ids {
		u, err := s.st.GetUser(ctx, uid)
		if err != nil || u.Disabled || !users.Effective(u).MayDownload {
			continue
		}
		if _, err := s.Queue(ctx, uid, workID, chapterIDs); err != nil {
			s.log.WarnContext(ctx, "auto download", "work", workID, "err", err)
		}
		return // queued once; others would only find them queued
	}
}

// SetAutoDownload turns "download new chapters" on or off for a user's
// library entry.
func (s *Service) SetAutoDownload(ctx context.Context, userID, workID uuid.UUID, on bool) error {
	n, err := s.st.SetAutoDownload(ctx, db.SetAutoDownloadParams{UserID: userID, WorkID: workID, AutoDownload: on})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) template(ctx context.Context) string {
	raw, err := s.st.GetSetting(ctx, settingTemplate)
	if err != nil {
		return DefaultTemplate
	}
	var t string
	if json.Unmarshal(raw, &t) != nil || !strings.HasSuffix(t, ".cbz") {
		return DefaultTemplate
	}
	return t
}

// pluginError turns a plugin's rate limit into a retry hint and gone
// chapters into permanent failures.
func pluginError(err error) error {
	var pe *plugin.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case plugin.CodeResourceExhausted:
			return &queue.RetryAfterError{Err: err, After: pe.RetryAfter}
		case plugin.CodeNotFound, plugin.CodeInvalidArgument:
			return queue.Permanent(err)
		}
	}
	if errors.Is(err, plugins.ErrNotFound) {
		return queue.Permanent(err)
	}
	return err
}

func (s *Service) handleDownload(ctx context.Context, t *queue.Task) (any, error) {
	var p downloadPayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	if f, err := s.st.GetChapterFile(ctx, p.ChapterID); err == nil {
		return map[string]string{"skipped": "downloaded", "key": f.Key}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ch, err := s.st.GetChapter(ctx, p.ChapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, queue.Permanent(ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if !ch.Available || !ch.PreferredVariantID.Valid {
		return nil, queue.Permanent(errors.New("no source lists this chapter any more"))
	}
	vrows, err := s.st.ChapterVariants(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	var v *db.ChapterVariantsRow
	for i := range vrows {
		if vrows[i].SourceChapter.ID == ch.PreferredVariantID.UUID {
			v = &vrows[i]
		}
	}
	if v == nil {
		return nil, errors.New("the chosen copy is gone; trying again after the next update")
	}
	w, err := s.st.GetWork(ctx, ch.WorkID)
	if err != nil {
		return nil, err
	}
	stRow, backend, err := s.storages.Default(ctx)
	if err != nil {
		return nil, err
	}
	key := s.limits.For("source:" + v.SourceID)

	if err := key.Wait(ctx); err != nil {
		return nil, err
	}
	pages, err := s.host.Pages(ctx, adminView, v.SourceID, v.SourceChapter.Key)
	if err != nil {
		return nil, pluginError(err)
	}
	if len(pages) == 0 {
		return nil, errors.New("the source lists no pages for this chapter")
	}
	if err := os.MkdirAll(s.tmpDir, 0o750); err != nil {
		return nil, err
	}
	tmp := filepath.Join(s.tmpDir, uuid.NewString()+".cbz.tmp")
	arc, err := cbz.Create(tmp)
	if err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			arc.Abort()
		}
	}()
	// Page images have their own budget (see plugins.ImageKey).
	img := s.limits.For(plugins.ImageKey(v.SourceID))
	for i, pg := range pages {
		ref := pg.GetRef()
		if ref == "" {
			ref = pg.GetImageUrl()
		}
		// Errors that may pass are tried again (each attempt paced).
		ct, data, err := s.host.PageImage(ctx, adminView, v.SourceID, ref, img.Wait)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", i+1, pluginError(err))
		}
		if err := arc.AddPage(ct, bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("page %d: %w", i+1, err)
		}
		t.SetProgress(queue.Progress{Phase: "pages", Done: int64(i + 1), Total: int64(len(pages)), Unit: "pages"})
	}
	res, err := arc.Finish(s.comicInfo(ctx, w, ch, v))
	if err != nil {
		return nil, err
	}
	finished = true
	defer func() { _ = os.Remove(tmp) }()

	parts := NameParts{
		Work: w.Title, WorkID: w.ID.String()[:8], Volume: ch.Volume, Number: ch.Number, Title: ch.Title,
		Group: v.SourceChapter.Scanlator, Lang: v.SourceChapter.Lang, ExtraID: ch.ID.String()[:8],
	}
	fileKey := Render(s.template(ctx), parts)
	// Another chapter's file with this name (an odd template)? Keep both:
	// this one gets its ID in the name. Never write over a stored file.
	if _, err := s.st.ChapterFileByKey(ctx, db.ChapterFileByKeyParams{StorageID: stRow.ID, Key: fileKey}); err == nil {
		fileKey = strings.TrimSuffix(fileKey, ".cbz") + " [" + ch.ID.String()[:8] + "].cbz"
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err := backend.Put(ctx, fileKey, tmp); err != nil {
		return nil, fmt.Errorf("store the file: %w", err)
	}
	file, err := s.st.InsertChapterFile(ctx, db.InsertChapterFileParams{
		ChapterID: ch.ID, StorageID: stRow.ID, Key: fileKey, Pages: int32(res.Pages), Bytes: res.Bytes, //nolint:gosec // page counts are small
		Sha256: res.SHA256, FromVariantID: uuid.NullUUID{UUID: v.SourceChapter.ID, Valid: true},
	})
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			// The chapter was stored meanwhile (or the name taken): remove
			// our copy only if no record points at that name.
			if _, gerr := s.st.ChapterFileByKey(ctx, db.ChapterFileByKeyParams{StorageID: stRow.ID, Key: fileKey}); errors.Is(gerr, pgx.ErrNoRows) {
				_ = backend.Delete(ctx, fileKey)
			}
			if _, gerr := s.st.GetChapterFile(ctx, ch.ID); gerr == nil {
				return map[string]string{"skipped": "downloaded"}, nil
			}
			return nil, err
		}
		_ = backend.Delete(ctx, fileKey)
		return nil, err
	}
	s.publishDownloaded(ctx, w.ID, ch.ID)
	return map[string]any{"key": file.Key, "pages": res.Pages, "bytes": res.Bytes}, nil
}

func (s *Service) publishDownloaded(ctx context.Context, workID, chapterID uuid.UUID) {
	ids, err := s.st.LibraryUserIDs(ctx, workID)
	if err != nil {
		s.log.WarnContext(ctx, "library users", "work", workID, "err", err)
	}
	s.hub.Publish("chapter.downloaded", map[string]any{"work_id": workID, "chapter_id": chapterID},
		events.Audience{Admins: true, Users: ids})
}

var ageRatings = map[string]string{
	"safe": "Everyone", "suggestive": "Teen", "erotic": "Mature 17+", "pornographic": "Adults Only 18+",
}

// comicInfo describes a chapter for other readers. The catalogue entry
// adds the people, summary and genres when the metadata service answers.
func (s *Service) comicInfo(ctx context.Context, w db.Work, ch db.Chapter, v *db.ChapterVariantsRow) cbz.ComicInfo {
	info := cbz.ComicInfo{
		Series: w.Title, Title: chapters.DisplayTitle(ch.Title), LanguageISO: v.SourceChapter.Lang, Web: v.SourceChapter.Url,
		Translator: v.SourceChapter.Scanlator, AgeRating: ageRatings[w.ContentRating],
		Notes: fmt.Sprintf("panelarr:work=%s chapter=%s entry=%s", w.ID, ch.ID, w.MetaEntryID),
	}
	if info.AgeRating == "" {
		info.AgeRating = "Unknown"
	}
	if ch.Number != nil {
		info.Number = strconv.FormatFloat(*ch.Number, 'f', -1, 64)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Volume)); err == nil {
		info.Volume = n
	}
	if w.Year != nil {
		info.Year = int(*w.Year)
	}
	switch w.Type {
	case "manga":
		info.Manga = "YesAndRightToLeft"
	case "manhwa", "manhua", "webtoon":
		info.Manga = "Yes"
	default:
		info.Manga = "No"
	}
	e, err := s.meta.GetEntry(ctx, w.MetaEntryID, "en", meta.Viewer{Admin: true})
	if err != nil {
		return info
	}
	var writers, artists, genres []string
	for _, c := range e.GetCredits() {
		switch c.GetRole() {
		case metadatav1.PersonRole_PERSON_ROLE_AUTHOR, metadatav1.PersonRole_PERSON_ROLE_WRITER, metadatav1.PersonRole_PERSON_ROLE_ORIGINAL_CREATOR:
			writers = append(writers, c.GetPerson().GetName())
		case metadatav1.PersonRole_PERSON_ROLE_ARTIST, metadatav1.PersonRole_PERSON_ROLE_ILLUSTRATOR:
			artists = append(artists, c.GetPerson().GetName())
		}
	}
	for _, t := range e.GetTags() {
		if t.GetTag().GetIsGenre() {
			genres = append(genres, t.GetTag().GetName())
		}
	}
	info.Writer, info.Penciller, info.Genre = strings.Join(writers, ", "), strings.Join(artists, ", "), strings.Join(genres, ", ")
	for _, d := range e.GetDescriptions() {
		if d.GetFormat() != metadatav1.DescriptionFormat_DESCRIPTION_FORMAT_HTML {
			info.Summary = d.GetText()
			break
		}
	}
	return info
}

// File is an open downloaded chapter.
type File struct {
	db.ChapterFile
	Archive *cbz.Archive
	io.Closer
}

// Open opens a chapter's downloaded file, or ErrNotStored.
func (s *Service) Open(ctx context.Context, chapterID uuid.UUID) (*File, error) {
	f, err := s.st.GetChapterFile(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotStored
	}
	if err != nil {
		return nil, err
	}
	b, err := s.storages.Backend(ctx, f.StorageID)
	if err != nil {
		return nil, err
	}
	sf, err := b.Open(ctx, f.Key)
	if err != nil {
		return nil, err
	}
	arc, err := cbz.Open(sf, sf.Size())
	if err != nil {
		_ = sf.Close()
		return nil, err
	}
	return &File{ChapterFile: f, Archive: arc, Closer: sf}, nil
}

// DeleteFile deletes a chapter's downloaded file (an admin's action).
func (s *Service) DeleteFile(ctx context.Context, chapterID uuid.UUID) error {
	f, err := s.st.GetChapterFile(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotStored
	}
	if err != nil {
		return err
	}
	b, err := s.storages.Backend(ctx, f.StorageID)
	if err != nil {
		return err
	}
	if err := b.Delete(ctx, f.Key); err != nil {
		return err
	}
	if _, err := s.st.DeleteChapterFile(ctx, f.ID); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "chapter file deleted", "component", "audit", "chapter", chapterID, "key", f.Key)
	return nil
}

// Downloaded returns which of a work's chapters have files.
func (s *Service) Downloaded(ctx context.Context, workID uuid.UUID) (map[uuid.UUID]bool, error) {
	ids, err := s.st.DownloadedChapterIDs(ctx, workID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

type movePayload struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Move queues moving every file from one storage to another.
func (s *Service) Move(ctx context.Context, by uuid.UUID, from, to string) (db.Task, error) {
	if from == to {
		return db.Task{}, ErrSameTarget
	}
	for _, id := range []string{from, to} {
		if _, err := s.storages.Get(ctx, id); err != nil {
			return db.Task{}, err
		}
	}
	t, _, err := s.queue.Enqueue(ctx, queue.NewTask{
		Kind: KindMove, Payload: movePayload{From: from, To: to}, DedupeKey: "move:" + from, CreatedBy: by,
	})
	return t, err
}

const moveBatch = 100

func (s *Service) handleMove(ctx context.Context, t *queue.Task) (any, error) {
	var p movePayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	src, err := s.storages.Backend(ctx, p.From)
	if err != nil {
		return nil, queue.Permanent(err)
	}
	dst, err := s.storages.Backend(ctx, p.To)
	if err != nil {
		return nil, queue.Permanent(err)
	}
	total, err := s.st.CountFilesByStorage(ctx, p.From)
	if err != nil {
		return nil, err
	}
	moved := int64(0)
	if err := os.MkdirAll(s.tmpDir, 0o750); err != nil {
		return nil, err
	}
	for {
		files, err := s.st.FilesOnStorage(ctx, db.FilesOnStorageParams{StorageID: p.From, Lim: moveBatch})
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			break
		}
		for _, f := range files {
			if err := s.moveOne(ctx, src, dst, p.To, f); err != nil {
				return nil, fmt.Errorf("move %s: %w", f.Key, err)
			}
			moved++
			t.SetProgress(queue.Progress{Phase: "moving", Done: moved, Total: total, Unit: "files"})
		}
	}
	s.log.InfoContext(ctx, "files moved", "component", "audit", "from", p.From, "to", p.To, "files", moved)
	return map[string]int64{"moved": moved}, nil
}

// moveOne copies a file through a local temporary copy, points its record
// at the new place, then removes the old copy.
func (s *Service) moveOne(ctx context.Context, src, dst storage.Storage, dstID string, f db.ChapterFile) error {
	in, err := src.Open(ctx, f.Key)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.tmpDir, uuid.NewString()+".move.tmp")
	out, err := os.Create(tmp) //nolint:gosec // a name the server makes
	if err != nil {
		_ = in.Close()
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	_, err = io.Copy(out, io.NewSectionReader(in, 0, in.Size()))
	_ = in.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := dst.Put(ctx, f.Key, tmp); err != nil {
		return err
	}
	if err := s.st.MoveChapterFile(ctx, db.MoveChapterFileParams{ID: f.ID, StorageID: dstID, Key: f.Key}); err != nil {
		_ = dst.Delete(ctx, f.Key)
		return err
	}
	return src.Delete(ctx, f.Key)
}

// Item is a download task with its chapter and work.
type Item struct {
	Task          db.Task
	ChapterID     uuid.UUID
	WorkID        uuid.UUID
	WorkTitle     string
	ChapterNumber *float64
	ChapterTitle  string
}

// List returns download tasks, newest first: all for admins, otherwise the
// ones userID asked for.
func (s *Service) List(ctx context.Context, userID *uuid.UUID, state string, before *db.Task, limit int) ([]Item, error) {
	p := db.ListDownloadTasksParams{Lim: int32(limit)} //nolint:gosec // bounded
	if userID != nil {
		p.UserID = uuid.NullUUID{UUID: *userID, Valid: true}
	}
	if state != "" {
		p.State.String, p.State.Valid = state, true
	}
	if before != nil {
		p.Before = &before.CreatedAt
		p.BeforeID = uuid.NullUUID{UUID: before.ID, Valid: true}
	}
	rows, err := s.st.ListDownloadTasks(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = Item{Task: r.Task, ChapterID: r.ChapterID, WorkID: r.WorkID, WorkTitle: r.WorkTitle, ChapterNumber: r.ChapterNumber, ChapterTitle: r.ChapterTitle}
	}
	return out, nil
}
