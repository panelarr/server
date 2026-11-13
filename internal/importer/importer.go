// SPDX-License-Identifier: GPL-3.0-or-later

// Package importer imports a user's library and reading state from
// another app's backup into their own account: the file is parsed once on
// upload, its series are matched in the background (the preview), and the
// user reviews the outcome before applying it. See docs/import.md.
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/importer/mihonbackup"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Import states.
const (
	StatePreviewing = "previewing"
	StatePreview    = "preview"
	StateApplying   = "applying"
	StateDone       = "done"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
)

// Series outcomes of a preview.
const (
	OutcomePending       = "pending"
	OutcomeReady         = "ready"
	OutcomeInLibrary     = "in_library"
	OutcomeNeedsChoice   = "needs_choice"
	OutcomeNotFound      = "not_found"
	OutcomeSourceMissing = "source_missing"
	OutcomeNotAllowed    = "not_allowed"
)

// FormatMihonBackup is a Suwayomi or Mihon backup (.tachibk).
const FormatMihonBackup = "mihon_backup"

// compatScheme is the scheme of the source IDs the Mihon backup format
// uses: sources are looked up by "mihon:<id>" (see the sources'
// compat_ids). Only this package knows it.
const compatScheme = "mihon"

// Events published to the importing user.
const (
	EventProgress = "import.progress"
	EventFinished = "import.finished"
)

// KindPreview is the task kind of matching an import's series.
const KindPreview = "import.preview"

// MaxFileSize is the largest backup accepted.
const MaxFileSize = mihonbackup.DefaultMaxFileSize

// Errors callers map to responses.
var (
	ErrNotFound = errors.New("importer: no such import")
	// ErrRunning: another import of the user is running.
	ErrRunning = errors.New("importer: another import is running")
	// ErrFileTooLarge: the upload is over MaxFileSize.
	ErrFileTooLarge = errors.New("importer: the file is too large")
	// ErrBusy: another backup is being read; try again shortly.
	ErrBusy = errors.New("importer: busy reading another backup")
	// ErrTooMany: the user keeps MaxImports imports already.
	ErrTooMany = errors.New("importer: too many imports kept")
)

// MaxImports is how many imports a user keeps: each holds the parsed
// series of a backup (tens of MiB for a huge one), so they can't pile up.
// Deleting one makes room.
const MaxImports = 20

// tempPattern names uploads while they are read.
const tempPattern = "panelarr-import-*"

// BadBackupError is an upload that isn't a usable backup.
type BadBackupError struct{ Err error }

func (e *BadBackupError) Error() string { return e.Err.Error() }
func (e *BadBackupError) Unwrap() error { return e.Err }

// ReadGate lets one backup be read at a time. A read may take a few
// hundred MiB (see mihonbackup's limits), and reads add up, so the whole
// server shares one gate: [ProcessGate].
type ReadGate struct{ ch chan struct{} }

// NewReadGate returns a gate of its own (tests).
func NewReadGate() *ReadGate { return &ReadGate{ch: make(chan struct{}, 1)} }

// ProcessGate is the gate every importer in the process uses by default.
var ProcessGate = NewReadGate()

// Acquire takes the gate, waiting at most wait. It reports whether it got
// it; the caller then releases it.
func (g *ReadGate) Acquire(ctx context.Context, wait time.Duration) bool {
	select {
	case g.ch <- struct{}{}:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g.ch <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Release gives the gate back.
func (g *ReadGate) Release() { <-g.ch }

// Options configure a Service.
type Options struct {
	// TempDir holds uploads while they are read (the system's default
	// when empty). Files are deleted as soon as they are parsed.
	TempDir string
	// Gate limits backup reads; ProcessGate when nil.
	Gate *ReadGate
	// GateWait is how long an upload waits for the gate before it is
	// refused as busy. Default 3 s.
	GateWait time.Duration
	// Limits of the backup reader; the defaults when zero.
	Limits mihonbackup.Limits
}

// Service runs imports.
type Service struct {
	st    *store.Store
	lib   *library.Service
	meta  *meta.Service
	host  *plugins.Host
	queue *queue.Queue
	hub   *events.Hub
	log   *slog.Logger
	opts  Options
	// beforeRead runs right before a backup is read (tests).
	beforeRead func()
	// afterApplyItem runs after apply went through a series (tests).
	afterApplyItem func(idx int32)
	now            func() time.Time
}

// NewService returns the importer and registers its task kinds on q.
func NewService(st *store.Store, lib *library.Service, m *meta.Service, host *plugins.Host, q *queue.Queue, hub *events.Hub, opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Gate == nil {
		opts.Gate = ProcessGate
	}
	if opts.GateWait == 0 {
		opts.GateWait = 3 * time.Second
	}
	s := &Service{st: st, lib: lib, meta: m, host: host, queue: q, hub: hub, log: log, opts: opts, now: time.Now}
	q.Register(KindPreview, queue.Kind{Handler: s.handlePreview, Concurrency: 2, MaxAttempts: 5})
	q.Register(KindApply, queue.Kind{Handler: s.handleApply, Concurrency: 2, MaxAttempts: 5})
	q.Register(KindPlace, queue.Kind{Handler: s.handlePlace, Concurrency: 2, MaxAttempts: 3})
	// Retried with its own backoff until the chapters are placed or
	// PendingFor is over.
	q.Register(KindPending, queue.Kind{Handler: s.handlePending, Concurrency: 2, MaxAttempts: 40})
	return s
}

// Summary is what an import found in its file, plus the preview's package
// hints. It is stored with the import.
type Summary struct {
	Series       int `json:"series"`
	OutOfLibrary int `json:"out_of_library"`
	Chapters     int `json:"chapters"`
	ChaptersRead int `json:"chapters_read"`
	InProgress   int `json:"in_progress"`
	Bookmarks    int `json:"bookmarks"`
	History      int `json:"history"`
	TrackerLinks int `json:"tracker_links"`
	// ClampedTimes counts read times in the future that apply capped at
	// the time it ran.
	ClampedTimes int           `json:"clamped_times,omitempty"`
	Categories   []Category    `json:"categories"`
	PackageHints []PackageHint `json:"package_hints,omitempty"`
}

// Category is a category of the backup with how many series it holds.
// Categories aren't imported yet.
type Category struct {
	Name   string `json:"name"`
	Series int    `json:"series"`
}

// PackageHint names a package a plugin offers that would provide sources
// the import is missing.
type PackageHint struct {
	PluginID    string `json:"plugin_id"`
	PackageID   string `json:"package_id"`
	PackageName string `json:"package_name"`
	// Series of the import it would attach.
	Series int `json:"series"`
}

// Chapter is a chapter of an import with something to apply: a read
// mark, a page or a bookmark. Stored compact, as JSON.
type Chapter struct {
	Key      string  `json:"key"`
	Number   float32 `json:"number"`
	Read     bool    `json:"read,omitempty"`
	Page     int64   `json:"page,omitempty"`
	ReadAt   int64   `json:"read_at,omitempty"` // ms since the epoch; 0 unknown
	Bookmark bool    `json:"bookmark,omitempty"`
}

// Candidate is a catalogue entry that may be a series' work.
type Candidate struct {
	EntryID      string  `json:"entry_id"`
	Title        string  `json:"title"`
	Year         int32   `json:"year,omitempty"`
	Score        float64 `json:"score"`
	MatchedTitle string  `json:"matched_title,omitempty"`
	AuthorsMatch *bool   `json:"authors_match,omitempty"`
}

// Upload reads a backup for v and starts its preview. When v imported the
// same file before (and that import didn't fail or get cancelled), it
// returns that import instead, with created false.
//
// The file is copied to a temporary file (hashed on the way) and deleted
// once parsed; the database keeps only the parsed series.
func (s *Service) Upload(ctx context.Context, v library.Viewer, file io.Reader) (db.Import, bool, error) {
	if s.opts.TempDir != "" {
		if err := os.MkdirAll(s.opts.TempDir, 0o750); err != nil {
			return db.Import{}, false, err
		}
	}
	f, err := os.CreateTemp(s.opts.TempDir, tempPattern)
	if err != nil {
		return db.Import{}, false, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), file)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return db.Import{}, false, ErrFileTooLarge
		}
		return db.Import{}, false, err
	}
	if n == 0 {
		return db.Import{}, false, &BadBackupError{fmt.Errorf("%w: the file is empty", mihonbackup.ErrNotBackup)}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if prev, err := s.st.FindImportByFile(ctx, db.FindImportByFileParams{UserID: v.UserID, FileSha256: sum}); err == nil {
		return prev, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Import{}, false, err
	}
	if err := s.SettleStale(ctx, v.UserID); err != nil {
		return db.Import{}, false, err
	}
	if _, err := s.st.RunningImport(ctx, v.UserID); err == nil {
		return db.Import{}, false, ErrRunning
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Import{}, false, err
	}
	if n, err := s.st.CountImports(ctx, v.UserID); err != nil {
		return db.Import{}, false, err
	} else if n >= MaxImports {
		return db.Import{}, false, ErrTooMany
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return db.Import{}, false, err
	}
	// The gate is held until the parsed backup has been stored and can be
	// collected: the model is what takes the memory.
	if !s.opts.Gate.Acquire(ctx, s.opts.GateWait) {
		if err := ctx.Err(); err != nil {
			return db.Import{}, false, err
		}
		return db.Import{}, false, ErrBusy
	}
	defer s.opts.Gate.Release()
	return s.parseAndStore(ctx, v, f, sum)
}

func (s *Service) parseAndStore(ctx context.Context, v library.Viewer, f *os.File, sum string) (db.Import, bool, error) {
	b, err := s.read(f)
	if err != nil {
		return db.Import{}, false, err
	}
	sumRaw, series, err := summaryOf(b)
	if err != nil {
		return db.Import{}, false, err
	}
	var imp db.Import
	err = s.st.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if n, err := q.CountImports(ctx, v.UserID); err != nil {
			return err
		} else if n >= MaxImports {
			return ErrTooMany
		}
		var err error
		imp, err = q.CreateImport(ctx, db.CreateImportParams{UserID: v.UserID, Format: FormatMihonBackup, FileSha256: sum, Summary: sumRaw})
		if isUniqueViolation(err) {
			return ErrRunning
		}
		if err != nil {
			return err
		}
		// The rows are built while they are sent, and each series of the
		// model is dropped once its row is built: the model and the rows
		// are never in memory together.
		next := seriesRows(b)
		built := 0
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"import_items"}, importItemColumns, pgx.CopyFromFunc(func() ([]any, error) {
			row, ok, err := next()
			if err == nil && ok {
				// The series dropped from the model and the rows sent are
				// garbage; collecting it now and then keeps the heap near
				// what is live instead of letting it grow to twice the model.
				if built += len(row.Chapters) + len(row.Title) + len(row.SeriesKey); built >= gcEvery {
					built = 0
					runtime.GC()
				}
			}
			if !ok || err != nil {
				return nil, err
			}
			return []any{
				imp.ID, row.Idx, row.Title, row.Author, row.Artist, row.SourceCompatID, row.SourceName, row.SeriesKey,
				row.AddedAt, row.Categories, row.Chapters, row.ChaptersRead, row.Bookmarks,
			}, nil
		})); err != nil {
			return err
		}
		t, _, err := s.queue.EnqueueWith(ctx, q, queue.NewTask{
			Kind: KindPreview, Payload: payload{ImportID: imp.ID}, DedupeKey: "import.preview." + imp.ID.String(),
			Priority: -10, CreatedBy: v.UserID,
		})
		if err != nil {
			return err
		}
		imp.TaskID = uuid.NullUUID{UUID: t.ID, Valid: true}
		return q.SetImportTask(ctx, db.SetImportTaskParams{ID: imp.ID, TaskID: imp.TaskID})
	})
	if err != nil {
		return db.Import{}, false, err
	}
	s.log.InfoContext(ctx, "import uploaded", "component", "audit", "actor", v.UserID, "import", imp.ID, "series", series)
	return imp, true, nil
}

// gcEvery is how many bytes of rows an upload builds between collections.
const gcEvery = 16 << 20

// importItemColumns are the columns an upload fills, in the order of the
// rows seriesRows builds (as InsertImportItems has them).
var importItemColumns = []string{
	"import_id", "idx", "title", "author", "artist", "source_compat_id", "source_name", "series_key",
	"added_at", "categories", "chapters", "chapters_read", "bookmarks",
}

// read reads the backup. The file is deleted once read.
func (s *Service) read(f *os.File) (*mihonbackup.Backup, error) {
	if s.beforeRead != nil {
		s.beforeRead()
	}
	b, err := mihonbackup.Read(f, s.opts.Limits)
	_ = f.Close()
	_ = os.Remove(f.Name())
	if err != nil {
		for _, bad := range []error{mihonbackup.ErrNotBackup, mihonbackup.ErrUnsupported, mihonbackup.ErrTooLarge, mihonbackup.ErrEmpty} {
			if errors.Is(err, bad) {
				return nil, &BadBackupError{err}
			}
		}
		return nil, err
	}
	return b, nil
}

// summaryOf sums up a backup for its import, and counts the series it
// imports: those in the library. Series kept only for their history
// (Mihon) are counted, not imported.
func summaryOf(b *mihonbackup.Backup) ([]byte, int, error) {
	bs := b.Summary()
	sum := Summary{
		Series: bs.Series, OutOfLibrary: bs.OutOfLibrary, Chapters: bs.Chapters, ChaptersRead: bs.ChaptersRead,
		InProgress: bs.InProgress, Bookmarks: bs.Bookmarks, History: bs.History, TrackerLinks: bs.TrackerLinks,
		Categories: make([]Category, len(b.Categories)),
	}
	catIdx := make(map[string]int, len(b.Categories))
	for i, c := range b.Categories {
		sum.Categories[i] = Category{Name: c.Name}
		if _, ok := catIdx[c.Name]; !ok {
			catIdx[c.Name] = i
		}
	}
	n := 0
	for i := range b.Series {
		se := &b.Series[i]
		if !se.InLibrary {
			continue
		}
		n++
		for _, c := range se.Categories {
			if j, ok := catIdx[c]; ok {
				sum.Categories[j].Series++
			}
		}
	}
	raw, err := json.Marshal(sum)
	return raw, n, err
}

// seriesRows returns an iterator over the rows of the series in the
// backup's library, in order. It empties each series of b once its row is
// built, so the model shrinks as the rows go out.
func seriesRows(b *mihonbackup.Backup) func() (db.InsertImportItemsParams, bool, error) {
	i, idx := 0, int32(0)
	return func() (db.InsertImportItemsParams, bool, error) {
		for ; i < len(b.Series); i++ {
			se := &b.Series[i]
			if !se.InLibrary {
				*se = mihonbackup.Series{}
				continue
			}
			row, err := rowOf(se, idx)
			*se = mihonbackup.Series{}
			i++
			idx++
			return row, err == nil, err
		}
		b.Series = nil
		return db.InsertImportItemsParams{}, false, nil
	}
}

// rowOf is the row of one series in the library.
func rowOf(se *mihonbackup.Series, idx int32) (db.InsertImportItemsParams, error) {
	var chs []Chapter
	var read, marks int32
	for _, c := range se.Chapters {
		if !c.Read && !c.Bookmark && c.LastPageRead <= 0 {
			continue
		}
		ch := Chapter{Key: c.URL, Number: c.Number, Read: c.Read, Page: max(c.LastPageRead, 0), Bookmark: c.Bookmark}
		// Undated marks get the chapter's fetch date, else the series'
		// added date, so they sort below real reading.
		for _, t := range []time.Time{c.LastRead, c.DateFetch, se.DateAdded} {
			if !t.IsZero() {
				ch.ReadAt = t.UnixMilli()
				break
			}
		}
		if c.Read {
			read++
		}
		if c.Bookmark {
			marks++
		}
		chs = append(chs, ch)
	}
	if chs == nil {
		chs = []Chapter{}
	}
	raw, err := json.Marshal(chs)
	if err != nil {
		return db.InsertImportItemsParams{}, err
	}
	cats := se.Categories
	if cats == nil {
		cats = []string{}
	}
	row := db.InsertImportItemsParams{
		Idx: idx, Title: se.Title, Author: se.Author, Artist: se.Artist,
		SourceCompatID: fmt.Sprintf("%s:%d", compatScheme, se.SourceID), SourceName: se.SourceName,
		SeriesKey: se.URL, Categories: cats, Chapters: raw, ChaptersRead: read, Bookmarks: marks,
	}
	if !se.DateAdded.IsZero() {
		t := se.DateAdded
		row.AddedAt = &t
	}
	return row, nil
}

// SweepTemp deletes uploads a previous run left in the temporary
// directory: a process that stopped mid-upload never deleted its file.
// Call it at startup, before serving. Without a TempDir of its own the
// importer shares the system's, which other processes use, so nothing is
// swept.
func (s *Service) SweepTemp() (int, error) {
	if s.opts.TempDir == "" {
		return 0, nil
	}
	names, err := filepath.Glob(filepath.Join(s.opts.TempDir, tempPattern))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		if fi, err := os.Lstat(name); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(name); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return n, err
			}
			continue
		}
		n++
	}
	if n > 0 {
		s.log.Info("deleted uploads left from a previous run", "files", n)
	}
	return n, nil
}

// Get returns one of the user's imports.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (db.Import, error) {
	if err := s.SettleStale(ctx, userID); err != nil {
		return db.Import{}, err
	}
	imp, err := s.st.GetImport(ctx, db.GetImportParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Import{}, ErrNotFound
	}
	return imp, err
}

// ImportCursor continues a list of imports: the last one listed.
type ImportCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ImportsPage is a page of a user's imports.
type ImportsPage struct {
	Imports []db.Import
	// Next continues the list; nil on the last page.
	Next *ImportCursor
}

// List returns a page of the user's imports, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, after *ImportCursor, limit int) (ImportsPage, error) {
	if err := s.SettleStale(ctx, userID); err != nil {
		return ImportsPage{}, err
	}
	p := db.ListImportsParams{UserID: userID, Lim: int32(limit + 1)} //nolint:gosec // bounded
	if after != nil {
		p.BeforeCreated = &after.CreatedAt
		p.BeforeID = uuid.NullUUID{UUID: after.ID, Valid: true}
	}
	rows, err := s.st.ListImports(ctx, p)
	if err != nil {
		return ImportsPage{}, err
	}
	out := ImportsPage{Imports: rows}
	if len(rows) > limit {
		out.Imports = rows[:limit]
		last := rows[limit-1]
		out.Next = &ImportCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// Counts are an import's series per outcome, with their read chapters and
// bookmarks.
func (s *Service) Counts(ctx context.Context, id uuid.UUID) ([]db.ImportOutcomeCountsRow, error) {
	return s.st.ImportOutcomeCounts(ctx, id)
}

// ApplyProgress is how far an apply is: series gone through out of those
// to apply.
func (s *Service) ApplyProgress(ctx context.Context, id uuid.UUID) (db.ImportApplyProgressRow, error) {
	return s.st.ImportApplyProgress(ctx, id)
}

// ResultCounts are an import's series per result of apply.
func (s *Service) ResultCounts(ctx context.Context, id uuid.UUID) ([]db.ImportResultCountsRow, error) {
	return s.st.ImportResultCounts(ctx, id)
}

// ItemsPage is a page of an import's series.
type ItemsPage struct {
	Items []db.ListImportItemsRow
	// Next is the idx to continue after; -1 on the last page.
	Next int32
}

// Items lists an import's series by their order in the file, optionally
// only those with one outcome or one result.
func (s *Service) Items(ctx context.Context, userID, id uuid.UUID, outcome, result string, after int32, limit int) (ItemsPage, error) {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return ItemsPage{}, err
	}
	p := db.ListImportItemsParams{ImportID: id, After: after, Lim: int32(limit + 1)} //nolint:gosec // bounded
	if outcome != "" {
		p.Outcome = pgtype.Text{String: outcome, Valid: true}
	}
	if result != "" {
		p.Result = pgtype.Text{String: result, Valid: true}
	}
	rows, err := s.st.ListImportItems(ctx, p)
	if err != nil {
		return ItemsPage{}, err
	}
	out := ItemsPage{Items: rows, Next: -1}
	if len(rows) > limit {
		out.Items = rows[:limit]
		out.Next = rows[limit-1].Idx
	}
	return out, nil
}

// Delete removes one of the user's imports with its series, stopping its
// preview if it runs. What it already imported stays.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	imp, err := s.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if imp.TaskID.Valid && (imp.State == StatePreviewing || imp.State == StateApplying) {
		if _, err := s.queue.Cancel(ctx, imp.TaskID.UUID); err != nil && !errors.Is(err, queue.ErrFinished) && !errors.Is(err, queue.ErrNotFound) {
			return err
		}
	}
	if _, err := s.st.DeleteImport(ctx, db.DeleteImportParams{ID: id, UserID: userID}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "import deleted", "component", "audit", "actor", userID, "import", id)
	return nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
