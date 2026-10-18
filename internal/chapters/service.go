// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

// KindUpdateSeries checks one attached series for chapters.
const KindUpdateSeries = "update.series"

// Errors callers map to responses.
var (
	ErrNotFound       = errors.New("chapters: not found")
	ErrNotThisChapter = errors.New("chapters: the variant isn't a copy of this chapter")
)

const settingLanguages = "chapters.languages"

// adminView sees every source (background work).
var adminView = plugins.Viewer{Admin: true, Grants: users.AdminGrants()}

// Service keeps the works' chapters.
type Service struct {
	st     *store.Store
	host   *plugins.Host
	queue  *queue.Queue
	limits *limits.Registry
	hub    *events.Hub
	log    *slog.Logger
	onNew  func(ctx context.Context, workID uuid.UUID, chapterIDs []uuid.UUID)
}

// OnNewChapters sets what happens when a work gains chapters (e.g.
// automatic downloads).
func (s *Service) OnNewChapters(f func(ctx context.Context, workID uuid.UUID, chapterIDs []uuid.UUID)) {
	s.onNew = f
}

// NewService returns the service and registers its task kind on q.
func NewService(st *store.Store, host *plugins.Host, q *queue.Queue, lim *limits.Registry, hub *events.Hub, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if lim == nil {
		lim = limits.NewRegistry()
	}
	s := &Service{st: st, host: host, queue: q, limits: lim, hub: hub, log: log}
	q.Register(KindUpdateSeries, queue.Kind{Handler: s.handleUpdateSeries, Concurrency: 8, MaxAttempts: 4})
	return s
}

type updatePayload struct {
	SeriesID uuid.UUID `json:"series_id"`
}

// QueueUpdate queues a chapter check of an attached series.
func (s *Service) QueueUpdate(ctx context.Context, se db.SourceSeries, by uuid.UUID, priority int32, runAfter time.Time) (db.Task, error) {
	t, _, err := s.queue.Enqueue(ctx, queue.NewTask{
		Kind: KindUpdateSeries, Payload: updatePayload{SeriesID: se.ID}, DedupeKey: "update:" + se.ID.String(),
		LimitKey: "source:" + se.SourceID, CreatedBy: by, Priority: priority, RunAfter: runAfter,
	})
	return t, err
}

// SeriesAttached checks a newly attached series' chapters soon.
func (s *Service) SeriesAttached(ctx context.Context, se db.SourceSeries) {
	if _, err := s.QueueUpdate(ctx, se, uuid.Nil, 15, time.Time{}); err != nil {
		s.log.WarnContext(ctx, "queue chapter check", "series", se.ID, "err", err)
	}
}

// WorkChanged rebuilds a work's chapters after its series changed.
func (s *Service) WorkChanged(ctx context.Context, workID uuid.UUID) {
	if _, err := s.Unify(ctx, workID); err != nil {
		s.log.WarnContext(ctx, "unify chapters", "work", workID, "err", err)
	}
}

// SourcesChanged chooses the chapter copies again of every work with a
// series on these sources, after they became usable or stopped being
// usable: copies on an unusable source give way, and come back with it.
func (s *Service) SourcesChanged(ctx context.Context, sourceIDs []string) {
	works, err := s.st.WorksOnSources(ctx, sourceIDs)
	if err != nil {
		s.log.WarnContext(ctx, "works on changed sources", "sources", sourceIDs, "err", err)
		return
	}
	for _, id := range works {
		s.WorkChanged(ctx, id)
	}
}

// UnifyAll rebuilds every work's chapters from the chapters stored, without
// asking any source: after a change to how chapter names are read. It
// returns how many works it went through.
func (s *Service) UnifyAll(ctx context.Context) (int, error) {
	works, err := s.st.WorksWithAttachedSeries(ctx)
	if err != nil {
		return 0, err
	}
	for i, id := range works {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if _, err := s.Unify(ctx, id); err != nil {
			return i, err
		}
	}
	return len(works), nil
}

// Refresh queues chapter checks of every attached series of a work, ahead
// of scheduled ones.
func (s *Service) Refresh(ctx context.Context, workID, by uuid.UUID) (int, error) {
	series, err := s.st.AttachedSourceSeries(ctx, workID)
	if err != nil {
		return 0, err
	}
	for _, se := range series {
		if _, err := s.QueueUpdate(ctx, se, by, 20, time.Time{}); err != nil {
			return 0, err
		}
	}
	return len(series), nil
}

// ScheduleAll queues chapter checks of every attached series of works
// someone follows, spread over spread so sources aren't hit at once.
func (s *Service) ScheduleAll(ctx context.Context, spread time.Duration) (int, error) {
	series, err := s.st.AttachedSeriesOfFollowedWorks(ctx)
	if err != nil {
		return 0, err
	}
	for _, se := range series {
		var delay time.Duration
		if spread > 0 {
			delay = time.Duration(rand.Int64N(int64(spread))) //nolint:gosec // jitter, not security
		}
		if _, err := s.QueueUpdate(ctx, se, uuid.Nil, 0, time.Now().Add(delay)); err != nil {
			return 0, err
		}
	}
	return len(series), nil
}

func (s *Service) handleUpdateSeries(ctx context.Context, t *queue.Task) (any, error) {
	var p updatePayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	se, err := s.st.GetSourceSeries(ctx, p.SeriesID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // the work or series is gone
	}
	if err != nil {
		return nil, err
	}
	if se.State != "attached" {
		return map[string]string{"skipped": "not attached"}, nil
	}
	if err := s.limits.For("source:" + se.SourceID).Wait(ctx); err != nil {
		return nil, err
	}
	list, err := s.host.ListChapters(ctx, adminView, se.SourceID, se.Key, true)
	if err != nil {
		if serr := s.st.SetSeriesChecked(ctx, db.SetSeriesCheckedParams{ID: se.ID, CheckError: errorText(err)}); serr != nil {
			return nil, serr
		}
		var pe *plugin.Error
		switch {
		case errors.Is(err, plugins.ErrNotFound):
			// The source is off or no longer listed: nothing to check
			// until it's back (its chapters stay as they were).
			return map[string]string{"skipped": "source unavailable"}, nil
		case errors.As(err, &pe) && pe.Code == plugin.CodeResourceExhausted:
			return nil, &queue.RetryAfterError{Err: err, After: pe.RetryAfter}
		case errors.As(err, &pe) && pe.Code == plugin.CodeNotFound:
			return nil, queue.Permanent(err) // the series is gone from the site
		}
		return nil, err
	}
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		// Writes to a work's chapters take the work's lock first, so they
		// run one at a time per work (rebuilding the unified list locks the
		// same rows in another order).
		if err := q.LockWork(ctx, se.WorkID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		keys := make([]string, 0, len(list))
		for i, c := range list {
			params := db.UpsertSourceChapterParams{
				SourceSeriesID: se.ID, Key: c.GetKey(), Name: c.GetName(), Volume: c.GetVolume(),
				Scanlator: c.GetScanlator(), Lang: c.GetLang(), Url: c.GetUrl(), Position: int32(i), //nolint:gosec // small
				// What a series had when first listed isn't news.
				Announce: se.ListedAt != nil,
			}
			if c.Number != nil {
				n := c.GetNumber()
				params.Number = &n
			}
			if c.GetUploadedAt() != nil {
				at := c.GetUploadedAt().AsTime()
				params.UploadedAt = &at
			}
			if err := q.UpsertSourceChapter(ctx, params); err != nil {
				return err
			}
			keys = append(keys, c.GetKey())
		}
		if err := q.MarkSourceChaptersAbsent(ctx, db.MarkSourceChaptersAbsentParams{SourceSeriesID: se.ID, Listed: keys}); err != nil {
			return err
		}
		return q.SetSeriesChecked(ctx, db.SetSeriesCheckedParams{ID: se.ID})
	})
	if err != nil {
		return nil, err
	}
	added, err := s.Unify(ctx, se.WorkID)
	if err != nil {
		return nil, err
	}
	return map[string]int{"listed": len(list), "new_chapters": added}, nil
}

func errorText(err error) string {
	var pe *plugin.Error
	if errors.As(err, &pe) {
		if pe.Reason != "" {
			return string(pe.Reason) + ": " + pe.Message
		}
		return pe.Message
	}
	return err.Error()
}

func (s *Service) languages(ctx context.Context) []string {
	raw, err := s.st.GetSetting(ctx, settingLanguages)
	if err != nil {
		return nil
	}
	var langs []string
	_ = json.Unmarshal(raw, &langs)
	return langs
}

func variantOf(r db.WorkVariantsRow) Variant {
	c := r.SourceChapter
	return Variant{
		ID: c.ID, SeriesID: c.SourceSeriesID, SourceID: r.SourceID, Key: c.Key, Name: c.Name, Number: c.Number,
		Volume: VolumeOf(c.Volume, c.Name), Scanlator: c.Scanlator, Lang: c.Lang, UploadedAt: c.UploadedAt, Position: c.Position,
		Priority: r.SeriesPriority, Health: r.SourceHealth, Unusable: !r.SourceUsable, Announce: c.Announce,
	}
}

// Unify rebuilds a work's canonical chapters from its attached series and
// returns how many chapters are new.
func (s *Service) Unify(ctx context.Context, workID uuid.UUID) (int, error) {
	langs := s.languages(ctx)
	var added []uuid.UUID
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		added = added[:0]
		if err := q.LockWork(ctx, workID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		rows, err := q.WorkVariants(ctx, workID)
		if err != nil {
			return err
		}
		vs := make([]Variant, len(rows))
		for i, r := range rows {
			vs[i] = variantOf(r)
		}
		existing, err := q.ListChapters(ctx, workID)
		if err != nil {
			return err
		}
		byNumber := map[float64]db.Chapter{}
		byExtra := map[string]db.Chapter{}
		for _, c := range existing {
			if c.Number != nil {
				byNumber[*c.Number] = c
			} else {
				byExtra[c.ExtraKey] = c
			}
		}
		kept := map[uuid.UUID]bool{}
		for _, g := range GroupVariants(vs) {
			var cur db.Chapter
			var ok bool
			if g.Number != nil {
				cur, ok = byNumber[*g.Number]
			} else {
				cur, ok = byExtra[g.ExtraKey]
			}
			var pinned *uuid.UUID
			if ok && cur.PinnedVariantID.Valid {
				pinned = &cur.PinnedVariantID.UUID
			}
			choice := Choose(g, pinned, langs)
			preferred := uuid.NullUUID{UUID: choice.Variant.ID, Valid: true}
			if !ok {
				cur, err = q.InsertChapter(ctx, db.InsertChapterParams{
					WorkID: workID, Number: g.Number, ExtraKey: g.ExtraKey, Volume: g.Volume(),
					Title: choice.Variant.Name, PreferredVariantID: preferred, Reason: choice.Reason,
					Announce: slices.ContainsFunc(g.Variants, func(v Variant) bool { return v.Announce }),
				})
				if err != nil {
					return err
				}
				added = append(added, cur.ID)
			} else if err := q.UpdateChapter(ctx, db.UpdateChapterParams{
				ID: cur.ID, Volume: g.Volume(), Title: choice.Variant.Name, PreferredVariantID: preferred,
				Reason: choice.Reason, Available: true,
			}); err != nil {
				return err
			}
			kept[cur.ID] = true
			ids := make([]uuid.UUID, len(g.Variants))
			for i, v := range g.Variants {
				ids[i] = v.ID
			}
			if err := q.AssignVariants(ctx, db.AssignVariantsParams{ChapterID: uuid.NullUUID{UUID: cur.ID, Valid: true}, Ids: ids}); err != nil {
				return err
			}
		}
		for _, c := range existing {
			if kept[c.ID] || !c.Available {
				continue
			}
			if err := q.UpdateChapter(ctx, db.UpdateChapterParams{ID: c.ID, Volume: c.Volume, Title: c.Title, Available: false}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.publish(ctx, workID, len(added))
	if len(added) > 0 && s.onNew != nil {
		s.onNew(ctx, workID, added)
	}
	return len(added), nil
}

func (s *Service) publish(ctx context.Context, workID uuid.UUID, added int) {
	ids, err := s.st.LibraryUserIDs(ctx, workID)
	if err != nil {
		s.log.WarnContext(ctx, "library users", "work", workID, "err", err)
	}
	to := events.Audience{Admins: true, Users: ids}
	if added > 0 {
		s.hub.Publish("chapters.new", map[string]any{"work_id": workID, "new": added}, to)
	}
	s.hub.Publish("work.updated", map[string]any{"work_id": workID}, to)
}

// Pin forces a variant for a chapter (nil unpins), then re-chooses.
func (s *Service) Pin(ctx context.Context, chapterID uuid.UUID, variantID *uuid.UUID) (db.Chapter, error) {
	ch, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Chapter{}, ErrNotFound
	}
	if err != nil {
		return db.Chapter{}, err
	}
	pin := uuid.NullUUID{}
	if variantID != nil {
		vs, err := s.st.ChapterVariants(ctx, chapterID)
		if err != nil {
			return db.Chapter{}, err
		}
		found := false
		for _, v := range vs {
			if v.SourceChapter.ID == *variantID {
				found = true
			}
		}
		if !found {
			return db.Chapter{}, ErrNotThisChapter
		}
		pin = uuid.NullUUID{UUID: *variantID, Valid: true}
	}
	if err := s.st.SetChapterPin(ctx, db.SetChapterPinParams{ID: chapterID, PinnedVariantID: pin}); err != nil {
		return db.Chapter{}, err
	}
	if _, err := s.Unify(ctx, ch.WorkID); err != nil {
		return db.Chapter{}, err
	}
	return s.st.GetChapter(ctx, chapterID)
}
