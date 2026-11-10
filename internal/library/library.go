// SPDX-License-Identifier: GPL-3.0-or-later

// Package library manages works and users' libraries. A work is one actual
// work, created from a metadata entry and shared by everyone; each user
// follows works in their own library. Series on sources are matched to a
// work (automatically, by searching the sources with the entry's titles,
// or by hand) and feed its chapters.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/textmatch"
	"github.com/panelarr/server/internal/users"
)

// Errors callers map to responses.
var (
	ErrNotFound     = errors.New("library: not found")
	ErrNotInLibrary = errors.New("library: not in your library")
	ErrWrongWork    = errors.New("library: the series belongs to another work")
)

// Viewer is who is looking.
type Viewer struct {
	UserID      uuid.UUID
	Grants      users.Grants
	Admin       bool
	HideUnknown bool
}

func (v Viewer) meta() meta.Viewer {
	return meta.Viewer{Ceiling: v.Grants.ContentRatingCeiling, Admin: v.Admin}
}

func (v Viewer) plugins() plugins.Viewer {
	return plugins.Viewer{Grants: v.Grants, Admin: v.Admin, HideUnknown: v.HideUnknown}
}

// adminViewer sees everything; background work (matching) uses it.
var adminViewer = Viewer{Admin: true, Grants: users.AdminGrants()}

// Service manages works and libraries.
type Service struct {
	st     *store.Store
	meta   *meta.Service
	host   *plugins.Host
	queue  *queue.Queue
	limits *limits.Registry
	hub    *events.Hub
	log    *slog.Logger
	now    func() time.Time
	hooks  ChapterHooks
}

// ChapterHooks lets the chapters service follow a work's series.
type ChapterHooks interface {
	// SeriesAttached: a series was attached to a work; load its chapters.
	SeriesAttached(ctx context.Context, se db.SourceSeries)
	// WorkChanged: a work's series changed (detached, priorities); rebuild
	// its chapters.
	WorkChanged(ctx context.Context, workID uuid.UUID)
}

type noHooks struct{}

func (noHooks) SeriesAttached(context.Context, db.SourceSeries) {}
func (noHooks) WorkChanged(context.Context, uuid.UUID)          {}

// SetChapterHooks connects the chapters service.
func (s *Service) SetChapterHooks(h ChapterHooks) { s.hooks = h }

// NewService returns the library service and registers its task kinds on
// q.
func NewService(st *store.Store, m *meta.Service, host *plugins.Host, q *queue.Queue, lim *limits.Registry, hub *events.Hub, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if lim == nil {
		lim = limits.NewRegistry()
	}
	s := &Service{st: st, meta: m, host: host, queue: q, limits: lim, hub: hub, log: log, now: time.Now, hooks: noHooks{}}
	q.Register(KindMatchWork, queue.Kind{Handler: s.handleMatchWork, Concurrency: 2, MaxAttempts: 3})
	q.Register(KindMatchSource, queue.Kind{Handler: s.handleMatchSource, Concurrency: 8, MaxAttempts: 4})
	return s
}

// Rating ranks on the users' ceiling scale; -1 is unknown.
func ratingRank(r string) int { return slices.Index(users.ContentRatings, r) }

// Visible reports whether v may see a work: within their ceiling, and
// either not matched to any source yet or matched to one they may use.
func (v Viewer) Visible(w db.Work, attachedSources []string) bool {
	if v.Admin {
		return true
	}
	rank := ratingRank(w.ContentRating)
	if rank < 0 {
		if v.HideUnknown {
			return false
		}
	} else if rank > ratingRank(v.Grants.ContentRatingCeiling) {
		return false
	}
	if len(attachedSources) == 0 {
		return true
	}
	for _, s := range attachedSources {
		if v.Grants.AllSources || slices.Contains(v.Grants.AllowedSources, s) {
			return true
		}
	}
	return false
}

func workParams(e *metav1.Entry) db.UpsertWorkParams {
	p := db.UpsertWorkParams{
		MetaEntryID: e.GetId(), Title: e.GetTitle(), SortTitle: textmatch.Normalize(e.GetTitle()),
		Type: meta.TypeName(e.GetType()), Status: meta.StatusName(e.GetStatus()),
		ContentRating: meta.RatingName(e.GetContentRating()), OriginalLanguage: e.GetOriginalLanguage(),
	}
	if y := e.GetDates().GetStart().GetYear(); y > 0 {
		p.Year = &y
	}
	if c := meta.CoverOf(e.GetImages()); c != nil {
		p.CoverUrl = c.GetUrl()
	}
	return p
}

// SourceRef names a series on a source.
type SourceRef struct {
	SourceID string
	Key      string
}

// Added is the outcome of adding a work to a library.
type Added struct {
	Work    db.Work
	AddedAt time.Time
	// New is false when the work was in the library already.
	New bool
}

// Add puts the work of a metadata entry into v's library, creating the
// work when nobody has it yet and starting its source matching. With
// attach, that series is attached to the work first.
func (s *Service) Add(ctx context.Context, v Viewer, entryID string, attach *SourceRef) (Added, error) {
	e, err := s.meta.GetEntry(ctx, entryID, "", v.meta())
	if err != nil {
		return Added{}, err
	}
	row, err := s.st.UpsertWork(ctx, workParams(e))
	if err != nil {
		return Added{}, err
	}
	w := workOf(row)
	if attach != nil {
		if _, err := s.AttachRef(ctx, v, w.ID, *attach, OriginManual); err != nil {
			return Added{}, err
		}
	}
	out := Added{Work: w}
	le, err := s.st.AddLibraryEntry(ctx, db.AddLibraryEntryParams{UserID: v.UserID, WorkID: w.ID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		cur, gerr := s.st.GetLibraryEntry(ctx, db.GetLibraryEntryParams{UserID: v.UserID, WorkID: w.ID})
		if gerr != nil {
			return Added{}, gerr
		}
		out.AddedAt = cur.AddedAt
	case err != nil:
		return Added{}, err
	default:
		out.New, out.AddedAt = true, le.AddedAt
		s.hub.Publish("library.changed", map[string]any{"work_id": w.ID}, events.Audience{Users: []uuid.UUID{v.UserID}})
	}
	if row.Inserted {
		if _, err := s.Rematch(ctx, w.ID, v.UserID, false); err != nil {
			s.log.WarnContext(ctx, "start matching", "work", w.ID, "err", err)
		}
	}
	return out, nil
}

func workOf(r db.UpsertWorkRow) db.Work {
	return db.Work{
		ID: r.ID, MetaEntryID: r.MetaEntryID, Title: r.Title, SortTitle: r.SortTitle, Type: r.Type, Status: r.Status,
		ContentRating: r.ContentRating, Year: r.Year, OriginalLanguage: r.OriginalLanguage, CoverUrl: r.CoverUrl,
		MetaSyncedAt: r.MetaSyncedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Remove takes a work out of v's library. Files are never deleted here.
func (s *Service) Remove(ctx context.Context, v Viewer, workID uuid.UUID) error {
	n, err := s.st.RemoveLibraryEntry(ctx, db.RemoveLibraryEntryParams{UserID: v.UserID, WorkID: workID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotInLibrary
	}
	s.hub.Publish("library.changed", map[string]any{"work_id": workID}, events.Audience{Users: []uuid.UUID{v.UserID}})
	return nil
}

// attachedSources maps works to the sources attached to them.
func (s *Service) attachedSources(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	rows, err := s.st.AttachedSourceIDsByWork(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]string{}
	for _, r := range rows {
		out[r.WorkID] = append(out[r.WorkID], r.SourceID)
	}
	return out, nil
}

// Entry is a work in a user's library.
type Entry struct {
	Work         db.Work
	AddedAt      time.Time
	Attached     int
	AutoDownload bool
}

// LibraryPage is one page of a library.
type LibraryPage struct {
	Entries []Entry
	// Next is the cursor of the next page; nil on the last.
	Next *Cursor
}

// Cursor continues a library listing.
type Cursor struct {
	SortTitle string
	ID        uuid.UUID
}

// List returns a page of v's library by title, hiding works v may no
// longer see (their ceiling or allowed sources changed).
func (s *Service) List(ctx context.Context, v Viewer, query string, after *Cursor, limit int) (LibraryPage, error) {
	var out LibraryPage
	cur := after
	for len(out.Entries) < limit {
		p := db.ListLibraryParams{UserID: v.UserID, Lim: int32(limit + 1)} //nolint:gosec // bounded
		if query != "" {
			p.Query.String, p.Query.Valid = query, true
		}
		if cur != nil {
			p.AfterTitle.String, p.AfterTitle.Valid = cur.SortTitle, true
			p.AfterID = uuid.NullUUID{UUID: cur.ID, Valid: true}
		}
		rows, err := s.st.ListLibrary(ctx, p)
		if err != nil {
			return LibraryPage{}, err
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.Work.ID
		}
		attached, err := s.attachedSources(ctx, ids)
		if err != nil {
			return LibraryPage{}, err
		}
		for _, r := range rows {
			cur = &Cursor{SortTitle: r.Work.SortTitle, ID: r.Work.ID}
			if !v.Visible(r.Work, attached[r.Work.ID]) {
				continue
			}
			if len(out.Entries) == limit {
				out.Next = &Cursor{SortTitle: out.Entries[limit-1].Work.SortTitle, ID: out.Entries[limit-1].Work.ID}
				return out, nil
			}
			out.Entries = append(out.Entries, Entry{Work: r.Work, AddedAt: r.AddedAt, Attached: len(attached[r.Work.ID]), AutoDownload: r.AutoDownload})
		}
		if len(rows) <= limit {
			return out, nil
		}
	}
	return out, nil
}

// VisibleWorks returns the works of v's library v may still see.
func (s *Service) VisibleWorks(ctx context.Context, v Viewer) ([]uuid.UUID, error) {
	works, err := s.st.LibraryWorks(ctx, v.UserID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(works))
	for i, w := range works {
		ids[i] = w.ID
	}
	attached, err := s.attachedSources(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := ids[:0]
	for _, w := range works {
		if v.Visible(w, attached[w.ID]) {
			out = append(out, w.ID)
		}
	}
	return out, nil
}

// Work is a work with what a viewer sees of it.
type Work struct {
	db.Work
	InLibrary bool
	AddedAt   *time.Time
	Sources   []WorkSource
}

// WorkSource is how a work stands on one source.
type WorkSource struct {
	Source     plugins.Source
	Status     string
	Message    string
	Queries    []string
	CheckedAt  *time.Time
	Attached   *db.SourceSeries
	Candidates []db.SourceSeries
}

// Get returns a work as v sees it, with its matching state per source v
// may use.
func (s *Service) Get(ctx context.Context, v Viewer, id uuid.UUID) (Work, error) {
	w, err := s.st.GetWork(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	if err != nil {
		return Work{}, err
	}
	attached, err := s.attachedSources(ctx, []uuid.UUID{id})
	if err != nil {
		return Work{}, err
	}
	if !v.Visible(w, attached[id]) {
		return Work{}, ErrNotFound
	}
	out := Work{Work: w}
	if le, err := s.st.GetLibraryEntry(ctx, db.GetLibraryEntryParams{UserID: v.UserID, WorkID: id}); err == nil {
		out.InLibrary, out.AddedAt = true, &le.AddedAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Work{}, err
	}
	srcs, err := s.host.Sources(ctx, v.plugins(), false)
	if err != nil {
		return Work{}, err
	}
	matches, err := s.st.ListWorkSourceMatches(ctx, id)
	if err != nil {
		return Work{}, err
	}
	series, err := s.st.ListSourceSeries(ctx, id)
	if err != nil {
		return Work{}, err
	}
	for _, src := range srcs {
		ws := WorkSource{Source: src, Status: "unchecked"}
		for _, m := range matches {
			if m.SourceID == src.ID {
				ws.Status, ws.Message, ws.Queries = m.Status, m.Message, m.Queries
				at := m.CheckedAt
				ws.CheckedAt = &at
			}
		}
		for _, se := range series {
			if se.SourceID != src.ID {
				continue
			}
			switch se.State {
			case StateAttached:
				se := se
				ws.Attached = &se
			case StateCandidate:
				ws.Candidates = append(ws.Candidates, se)
			}
		}
		out.Sources = append(out.Sources, ws)
	}
	return out, nil
}

// notify tells admins and everyone with the work in their library that it
// changed.
func (s *Service) notify(ctx context.Context, workID uuid.UUID) {
	ids, err := s.st.LibraryUserIDs(ctx, workID)
	if err != nil {
		s.log.WarnContext(ctx, "library users", "work", workID, "err", err)
	}
	s.hub.Publish("work.updated", map[string]any{"work_id": workID}, events.Audience{Admins: true, Users: ids})
}

// Evidence is why a series was matched to a work.
type Evidence struct {
	EntryTitle    string   `json:"entry_title,omitempty"`
	MatchedTitle  string   `json:"matched_title,omitempty"`
	Query         string   `json:"query,omitempty"`
	TitleScore    float64  `json:"title_score"`
	AuthorsMatch  *bool    `json:"authors_match,omitempty"`
	SourceAuthors []string `json:"source_authors,omitempty"`
	ChapterCount  *int     `json:"chapter_count,omitempty"`
	// BestEffort: the source's plugin only guessed the series (e.g. from
	// searching the site for a pasted URL), so a person should check it.
	BestEffort bool `json:"best_effort,omitempty"`
}

func (e Evidence) json() []byte {
	b, _ := json.Marshal(e)
	return b
}

// EvidenceOf decodes a series' evidence.
func EvidenceOf(se db.SourceSeries) Evidence {
	var e Evidence
	_ = json.Unmarshal(se.Evidence, &e)
	return e
}

// RefreshFromMeta copies the latest display fields (title, cover, status,
// rating, …) of up to limit followed works from the metadata service,
// least recently refreshed first. When the service merged an entry into
// another, the work follows the new entry, unless another work already
// has it: then both stay and the conflict is logged (merging works comes
// later).
func (s *Service) RefreshFromMeta(ctx context.Context, limit int) (updated, conflicts int, err error) {
	works, err := s.st.FollowedWorksBySync(ctx, int32(limit)) //nolint:gosec // bounded
	if err != nil {
		return 0, 0, err
	}
	for _, w := range works {
		e, err := s.meta.GetEntry(ctx, w.MetaEntryID, "", adminViewer.meta())
		if err != nil {
			if errors.Is(err, meta.ErrNotFound) {
				s.log.WarnContext(ctx, "work's entry is gone from the catalogue", "work", w.ID, "entry", w.MetaEntryID)
				continue
			}
			return updated, conflicts, err
		}
		p := workParams(e)
		up := db.UpdateWorkDisplayParams{
			ID: w.ID, MetaEntryID: p.MetaEntryID, Title: p.Title, SortTitle: p.SortTitle, Type: p.Type, Status: p.Status,
			ContentRating: p.ContentRating, Year: p.Year, OriginalLanguage: p.OriginalLanguage, CoverUrl: p.CoverUrl,
		}
		_, err = s.st.UpdateWorkDisplay(ctx, up)
		if isUniqueViolation(err) {
			conflicts++
			s.log.WarnContext(ctx, "work's entry was merged into another work's entry", "work", w.ID, "entry", w.MetaEntryID, "merged_into", p.MetaEntryID)
			up.MetaEntryID = w.MetaEntryID
			_, err = s.st.UpdateWorkDisplay(ctx, up)
		}
		if err != nil {
			return updated, conflicts, err
		}
		updated++
	}
	return updated, conflicts, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
