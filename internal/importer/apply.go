// SPDX-License-Identifier: GPL-3.0-or-later

package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
)

// Task kinds of applying an import.
const (
	// KindApply applies an import's series.
	KindApply = "import.apply"
	// KindPlace places the chapters waiting for one work's chapters, after
	// they were rebuilt.
	KindPlace = "import.place"
	// KindPending retries an import's waiting chapters with a backoff
	// until PendingFor is over, then reports what was left.
	KindPending = "import.pending"
)

// What apply did with a series (its result); its outcome stays what the
// preview found.
const (
	ResultApplied         = "applied"
	ResultAlreadyImported = "already_imported"
	ResultSkipped         = "skipped"
)

// PendingFor is how long chapters an apply couldn't place wait for their
// work's chapters (a newly attached series has none until its first
// check) before they are reported as not placed.
const PendingFor = 24 * time.Hour

// Errors callers map to responses.
var (
	// ErrState: the import isn't in a state that allows this: apply needs
	// preview, or cancelled or failed after a finished preview.
	ErrState = errors.New("importer: the import isn't in a state for that")
	// ErrNotRunning: cancel of an import that isn't running.
	ErrNotRunning = errors.New("importer: the import isn't running")
	// ErrNoSuchItem: no series at that position.
	ErrNoSuchItem = errors.New("importer: no such series in the import")
	// ErrItemState: the series can't be resolved that way in its state.
	ErrItemState = errors.New("importer: the series can't be resolved in its state")
	// ErrNoSuchEntry: the catalogue has no such entry the user may see.
	ErrNoSuchEntry = errors.New("importer: no such catalogue entry")
)

// NotAllowedError: the user's grants don't allow adding the work.
type NotAllowedError struct{ Reason string }

func (e *NotAllowedError) Error() string { return e.Reason }

// Why a series isn't applied, for the user.
const (
	whyHidden       = "the work is above your content rating limit or only on sources you may not use"
	whyMayNotAdd    = "your account may not add works"
	whySource       = "you may not use this source"
	whyEntryGone    = "the catalogue entry is gone or above your content rating limit"
	whyKeptOther    = "kept the series already attached on this source; chapters are placed by number"
	whyRejected     = "the series was detached from this work before, so it isn't attached again; chapters are placed by number"
	whyUnavailable  = "the source didn't answer, so the series isn't attached; chapters are placed by number"
	whyNotPlacedFmt = "%d chapters couldn't be placed: Panelarr has no such chapters"
)

// Apply starts applying one of the user's imports, which must be in
// preview.
func (s *Service) Apply(ctx context.Context, userID, id uuid.UUID) (db.Import, error) {
	imp, err := s.Get(ctx, userID, id)
	if err != nil {
		return db.Import{}, err
	}
	switch imp.State {
	case StatePreview, StateCancelled, StateFailed:
	default:
		return db.Import{}, ErrState
	}
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		var err error
		imp, err = q.StartImportApply(ctx, db.StartImportApplyParams{ID: id, UserID: userID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrState // its preview didn't finish, or it moved on meanwhile
		case isUniqueViolation(err):
			return ErrRunning
		case err != nil:
			return err
		}
		t, _, err := s.queue.EnqueueWith(ctx, q, queue.NewTask{
			Kind: KindApply, Payload: payload{ImportID: id}, DedupeKey: "import.apply." + id.String(),
			Priority: -10, CreatedBy: userID,
		})
		if err != nil {
			return err
		}
		imp.TaskID = uuid.NullUUID{UUID: t.ID, Valid: true}
		return q.SetImportTask(ctx, db.SetImportTaskParams{ID: id, TaskID: imp.TaskID})
	})
	if err != nil {
		return db.Import{}, err
	}
	s.log.InfoContext(ctx, "import applying", "component", "audit", "actor", userID, "import", id)
	return imp, nil
}

// Cancel stops one of the user's imports while it previews or applies.
// What was applied stays.
func (s *Service) Cancel(ctx context.Context, userID, id uuid.UUID) (db.Import, error) {
	imp, err := s.Get(ctx, userID, id)
	if err != nil {
		return db.Import{}, err
	}
	if imp.State != StatePreviewing && imp.State != StateApplying {
		return db.Import{}, ErrNotRunning
	}
	out, err := s.st.CancelImport(ctx, db.CancelImportParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Import{}, ErrNotRunning // ended meanwhile
	}
	if err != nil {
		return db.Import{}, err
	}
	if imp.TaskID.Valid {
		if _, err := s.queue.Cancel(ctx, imp.TaskID.UUID); err != nil && !errors.Is(err, queue.ErrFinished) && !errors.Is(err, queue.ErrNotFound) {
			s.log.WarnContext(ctx, "cancel import task", "import", id, "err", err)
		}
	}
	s.log.InfoContext(ctx, "import cancelled", "component", "audit", "actor", userID, "import", id)
	s.hub.Publish(EventFinished, FinishedEvent{ImportID: id, State: StateCancelled}, events.Audience{Users: []uuid.UUID{userID}})
	s.schedulePending(ctx, id, userID)
	return out, nil
}

// handleApply goes through an import's series that apply hasn't reached
// yet, in file order, so an apply interrupted by a restart picks up where
// it stopped.
func (s *Service) handleApply(ctx context.Context, t *queue.Task) (any, error) {
	var p payload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	imp, err := s.st.GetImportByID(ctx, p.ImportID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if imp.State != StateApplying {
		return nil, nil
	}
	err = s.apply(ctx, t, imp)
	if err == nil {
		return nil, nil
	}
	if ctx.Err() == nil && t.Attempts >= t.MaxAttempts {
		s.finishApply(context.WithoutCancel(ctx), imp, StateFailed, err.Error())
	}
	return nil, err
}

func (s *Service) apply(ctx context.Context, t *queue.Task, imp db.Import) error {
	// The grants as they are now: they may have changed since the preview.
	v, err := s.viewer(ctx, imp.UserID)
	if err != nil {
		return err
	}
	prog, err := s.st.ImportApplyProgress(ctx, imp.ID)
	if err != nil {
		return err
	}
	done, total := int64(prog.Done), int64(prog.Total)
	report := s.reporter(t, imp, "applying", &done, &total)
	report(true)
	m := &matcher{s: s, v: v, sources: map[string]sourceInfo{}}
	after := int32(-1)
	for {
		// Stop when the import was cancelled or deleted meanwhile.
		cur, err := s.st.GetImportByID(ctx, imp.ID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.State != StateApplying) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := s.st.ImportItemsToApply(ctx, db.ImportItemsToApplyParams{ImportID: imp.ID, After: after, Lim: 20})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, it := range rows {
			if err := ctx.Err(); err != nil {
				return err // cancelled, or the server stops
			}
			after = it.Idx
			if err := s.applyItem(ctx, m, imp, it); err != nil {
				return err
			}
			done++
			report(false)
			if s.afterApplyItem != nil {
				s.afterApplyItem(it.Idx)
			}
		}
	}
	report(true)
	s.finishApply(ctx, imp, StateDone, "")
	return nil
}

// reporter reports an import task's progress: on the task, and as
// import.progress to the user, throttled.
func (s *Service) reporter(t *queue.Task, imp db.Import, phase string, done, total *int64) func(force bool) {
	var last time.Time
	return func(force bool) {
		t.SetProgress(queue.Progress{Phase: phase, Done: *done, Total: *total, Unit: "series"})
		if force || time.Since(last) >= progressEvery {
			last = time.Now()
			s.hub.Publish(EventProgress, ProgressEvent{ImportID: imp.ID, Done: *done, Total: *total}, events.Audience{Users: []uuid.UUID{imp.UserID}})
		}
	}
}

// finishApply ends an apply and tells the user.
func (s *Service) finishApply(ctx context.Context, imp db.Import, state, message string) {
	if _, err := s.st.FinishImportApply(ctx, db.FinishImportApplyParams{ID: imp.ID, State: state, Message: message}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.WarnContext(ctx, "finish import apply", "import", imp.ID, "err", err)
		}
		return
	}
	s.log.InfoContext(ctx, "import applied", "component", "audit", "actor", imp.UserID, "import", imp.ID, "state", state)
	s.hub.Publish(EventFinished, FinishedEvent{ImportID: imp.ID, State: state}, events.Audience{Users: []uuid.UUID{imp.UserID}})
	s.schedulePending(ctx, imp.ID, imp.UserID)
}

// prepared is a series ready for its chapters: the work in the user's
// library and the source series attached when it could be.
type prepared struct {
	outcome, message, entryID string
	workID                    uuid.NullUUID
	// sourceID is set when the backup's series is the work's attached
	// series on it: chapters are then placed by key first.
	sourceID string
	// changed: the entry was added or the series attached.
	changed bool
}

// prepareItem checks the user's grants as they are now, adds the work to
// their library when it isn't there yet and attaches the backup's series
// when the rules allow. outcome is set only when the series can't be
// applied.
func (s *Service) prepareItem(ctx context.Context, m *matcher, it db.ImportItem) (prepared, error) {
	p := prepared{entryID: it.EntryID, workID: it.WorkID}
	refuse := func(why string) (prepared, error) {
		p.outcome, p.message = OutcomeNotAllowed, why
		return p, nil
	}
	si, err := m.source(ctx, it.SourceCompatID)
	if err != nil {
		return prepared{}, err
	}
	if !p.workID.Valid && p.entryID != "" {
		w, err := s.st.GetWorkByEntry(ctx, p.entryID)
		switch {
		case err == nil:
			p.workID = uuid.NullUUID{UUID: w.ID, Valid: true}
		case !errors.Is(err, pgx.ErrNoRows):
			return prepared{}, err
		}
	}
	inLibrary := false
	if p.workID.Valid {
		w, err := s.lib.Get(ctx, m.v, p.workID.UUID)
		switch {
		case errors.Is(err, library.ErrNotFound):
			return refuse(whyHidden)
		case err != nil:
			return prepared{}, err
		}
		inLibrary, p.entryID = w.InLibrary, w.MetaEntryID
	}
	if p.entryID == "" {
		p.outcome, p.message = OutcomeNotFound, ""
		return p, nil
	}
	if !inLibrary {
		if !m.v.Grants.MayAdd {
			return refuse(whyMayNotAdd)
		}
		if si.state == sourceNotAllowed {
			return refuse(whySource)
		}
		var addedAt *time.Time
		if it.AddedAt != nil {
			at := earliest(*it.AddedAt, s.now())
			addedAt = &at
		}
		added, err := s.lib.AddWith(ctx, m.v, p.entryID, nil, library.AddOptions{AddedAt: addedAt})
		switch {
		case errors.Is(err, meta.ErrNotFound):
			return refuse(whyEntryGone)
		case err != nil:
			return prepared{}, err
		}
		p.workID = uuid.NullUUID{UUID: added.Work.ID, Valid: true}
		p.changed = added.New
	}
	if si.usable != nil {
		state, err := s.lib.AttachImported(ctx, m.v, p.workID.UUID, si.usable.ID, it.SeriesKey)
		if err != nil {
			return prepared{}, err
		}
		switch state {
		case library.ImportAttached:
			p.changed, p.sourceID = true, si.usable.ID
		case library.ImportAttachedBefore:
			p.sourceID = si.usable.ID
		case library.ImportKeptOther:
			p.message = whyKeptOther
		case library.ImportRejected:
			p.message = whyRejected
		case library.ImportUnavailable:
			p.message = whyUnavailable
		}
	}
	return p, nil
}

// applyItem applies one series: its work and source series, then its
// chapters' reading, recorded with the series in one transaction.
func (s *Service) applyItem(ctx context.Context, m *matcher, imp db.Import, it db.ImportItem) error {
	p, err := s.prepareItem(ctx, m, it)
	if err != nil {
		return err
	}
	return s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		row, err := q.LockImportItem(ctx, db.LockImportItemParams{ImportID: it.ImportID, Idx: it.Idx})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted meanwhile
		}
		if err != nil {
			return err
		}
		if r := row.ImportItem.Result; r == ResultApplied || r == ResultAlreadyImported {
			return nil // applied meanwhile (a resolve)
		}
		fin := db.FinishImportItemParams{
			ImportID: it.ImportID, Idx: it.Idx, Outcome: p.outcome, WorkID: p.workID, EntryID: p.entryID,
			SourceID: p.sourceID, Message: p.message, PendingChapters: nil,
		}
		if p.outcome != "" {
			// Not applied: keep what the preview found.
			fin.WorkID, fin.EntryID, fin.SourceID = it.WorkID, it.EntryID, it.SourceID
			return q.FinishImportItem(ctx, fin)
		}
		var chs []Chapter
		if err := json.Unmarshal(it.Chapters, &chs); err != nil {
			return err
		}
		res, err := s.place(ctx, q, imp.UserID, p.workID.UUID, p.sourceID, it.SeriesKey, chs, imp.CreatedAt)
		if err != nil {
			return err
		}
		fin.Placed, fin.Unplaced = res.placed, res.unplaced
		if err := res.count(ctx, q, it.ImportID); err != nil {
			return err
		}
		if err := res.setPending(&fin.Pending, &fin.PendingChapters, &fin.PendingUntil, s.now().Add(PendingFor)); err != nil {
			return err
		}
		fin.Outcome, fin.Result = it.Outcome, ResultAlreadyImported
		if p.changed || res.changed > 0 || len(res.pending) > 0 {
			fin.Result = ResultApplied
		}
		if res.unplaced > 0 && len(res.pending) == 0 {
			fin.Message = joinMessage(fin.Message, fmt.Sprintf(whyNotPlacedFmt, res.unplaced))
		}
		return q.FinishImportItem(ctx, fin)
	})
}

// placement is what placing a series' chapters did.
type placement struct {
	placed, unplaced int32
	// pending are the chapters to place later.
	pending []Chapter
	// changed counts progress rows and bookmarks written.
	changed int64
	// clamped counts read times from the future, capped at now.
	clamped int32
}

// count adds the read times placing capped to the import's report.
func (r placement) count(ctx context.Context, q *db.Queries, importID uuid.UUID) error {
	if r.clamped == 0 {
		return nil
	}
	return q.AddImportClampedTimes(ctx, db.AddImportClampedTimesParams{ID: importID, Clamped: r.clamped})
}

func (r placement) setPending(n *int32, raw *[]byte, until **time.Time, deadline time.Time) error {
	*n, *raw, *until = int32(len(r.pending)), nil, nil //nolint:gosec // a series' chapters
	if len(r.pending) == 0 {
		return nil
	}
	b, err := json.Marshal(r.pending)
	if err != nil {
		return err
	}
	*raw, *until = b, &deadline
	return nil
}

// mark is the reading to write on one Panelarr chapter, merged from the
// backup's chapters placed on it (two copies of one chapter, say).
type mark struct {
	progress, read bool
	page           int32
	at             time.Time
	bookmark       bool
	markAt         time.Time
}

// place writes a series' reading on its work's chapters for the user: by
// the source's chapter key when the backup's series is attached
// (sourceID), else by chapter number. Chapters it can't place yet are
// returned as pending, except those it never could (no number, and no key
// to go by), which count as unplaced. Reading is written with the import
// upsert: it never loses newer reading in Panelarr.
func (s *Service) place(ctx context.Context, q *db.Queries, userID, workID uuid.UUID, sourceID, seriesKey string, chs []Chapter, undated time.Time) (placement, error) {
	var out placement
	if len(chs) == 0 {
		return out, nil
	}
	byKey := map[string]uuid.UUID{}
	if sourceID != "" {
		keys := make([]string, len(chs))
		for i, c := range chs {
			keys[i] = c.Key
		}
		rows, err := q.ChaptersBySourceKey(ctx, db.ChaptersBySourceKeyParams{WorkID: workID, SourceID: sourceID, SeriesKey: seriesKey, Keys: keys})
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			byKey[r.Key] = r.ChapterID
		}
	}
	nums, err := q.ChapterNumbers(ctx, workID)
	if err != nil {
		return out, err
	}
	byNumber := make(map[float64]uuid.UUID, len(nums))
	for _, r := range nums {
		byNumber[r.Number] = r.ID
	}
	now := s.now()
	marks := map[uuid.UUID]*mark{}
	var order []uuid.UUID
	for _, c := range chs {
		id, ok := byKey[c.Key]
		if !ok && c.Number >= 0 {
			id, ok = byNumber[number(c.Number)]
		}
		if !ok {
			if c.Number < 0 && sourceID == "" {
				out.unplaced++
			} else {
				out.pending = append(out.pending, c)
			}
			continue
		}
		out.placed++
		if c.ReadAt != 0 && time.UnixMilli(c.ReadAt).After(now) {
			out.clamped++
		}
		mk := marks[id]
		if mk == nil {
			mk = &mark{}
			marks[id] = mk
			order = append(order, id)
		}
		at := readTime(c.ReadAt, undated, now)
		if c.Read || c.Page > 0 {
			if !mk.progress || at.After(mk.at) {
				mk.at, mk.page = at, int32(min(c.Page, math.MaxInt32)) //nolint:gosec // clamped
			}
			mk.progress = true
			mk.read = mk.read || c.Read
		}
		if c.Bookmark {
			if !mk.bookmark || at.After(mk.markAt) {
				mk.markAt = at
			}
			mk.bookmark = true
		}
	}
	slices.SortFunc(order, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	var pp db.ImportProgressParams
	bp := db.ImportBookmarksParams{UserID: userID}
	pp.UserID = userID
	for _, id := range order {
		mk := marks[id]
		if mk.progress {
			pp.ChapterIds = append(pp.ChapterIds, id)
			pp.Pages = append(pp.Pages, mk.page)
			pp.Completed = append(pp.Completed, mk.read)
			pp.ReadAt = append(pp.ReadAt, mk.at)
		}
		if mk.bookmark {
			bp.ChapterIds = append(bp.ChapterIds, id)
			bp.CreatedAt = append(bp.CreatedAt, mk.markAt)
		}
	}
	if len(pp.ChapterIds) > 0 {
		n, err := q.ImportProgress(ctx, pp)
		if err != nil {
			return out, err
		}
		out.changed += n
	}
	if len(bp.ChapterIds) > 0 {
		n, err := q.ImportBookmarks(ctx, bp)
		if err != nil {
			return out, err
		}
		out.changed += n
	}
	return out, nil
}

// number is a backup's chapter number (a float32) as the float64 Panelarr
// stores: by its shortest decimal form, so 1.1 stays 1.1.
func number(f float32) float64 {
	n, err := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	if err != nil {
		return float64(f)
	}
	return n
}

// readTime is a chapter's last-read time: undated marks get undated (the
// import's upload), and a time in the future (a wrong clock, or a crafted
// file) is now, so it can't outrank later reading forever.
func readTime(ms int64, undated, now time.Time) time.Time {
	t := undated
	if ms != 0 {
		t = time.UnixMilli(ms).UTC()
	}
	return earliest(t, now)
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func joinMessage(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// schedulePending makes sure an import's waiting chapters are retried, and
// given up on after PendingFor.
func (s *Service) schedulePending(ctx context.Context, importID, userID uuid.UUID) {
	rows, err := s.st.PendingImportItemsOfImport(ctx, importID)
	if err != nil || len(rows) == 0 {
		if err != nil {
			s.log.WarnContext(ctx, "pending import chapters", "import", importID, "err", err)
		}
		return
	}
	if _, _, err := s.queue.Enqueue(context.WithoutCancel(ctx), queue.NewTask{
		Kind: KindPending, Payload: payload{ImportID: importID}, DedupeKey: "import.pending." + importID.String(),
		Priority: -10, CreatedBy: userID, RunAfter: s.now().Add(pendingBackoff(1)),
	}); err != nil {
		s.log.WarnContext(ctx, "queue pending import chapters", "import", importID, "err", err)
	}
}

// pendingBackoff is the wait before the attempt-th retry of waiting
// chapters: 5 minutes, doubling, at most 3 hours.
func pendingBackoff(attempt int32) time.Duration {
	d := 5 * time.Minute
	for i := int32(1); i < attempt && d < 3*time.Hour; i++ {
		d *= 2
	}
	return min(d, 3*time.Hour)
}

// WorkChaptersChanged queues placing the chapters imports left waiting for
// a work, after its chapters were rebuilt. Cheap when nothing waits.
func (s *Service) WorkChaptersChanged(ctx context.Context, workID uuid.UUID) {
	ok, err := s.st.WorkHasPendingImports(ctx, uuid.NullUUID{UUID: workID, Valid: true})
	if err != nil || !ok {
		if err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "pending imports of a work", "work", workID, "err", err)
		}
		return
	}
	if _, _, err := s.queue.Enqueue(context.WithoutCancel(ctx), queue.NewTask{
		Kind: KindPlace, Payload: placePayload{WorkID: workID}, DedupeKey: "import.place." + workID.String(), Priority: -5,
	}); err != nil {
		s.log.WarnContext(ctx, "queue placing imported chapters", "work", workID, "err", err)
	}
}

type placePayload struct {
	WorkID uuid.UUID `json:"work_id"`
}

func (s *Service) handlePlace(ctx context.Context, t *queue.Task) (any, error) {
	var p placePayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	rows, err := s.st.PendingImportItemsOnWork(ctx, uuid.NullUUID{UUID: p.WorkID, Valid: true})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if _, err := s.placePending(ctx, r.ImportID, r.Idx); err != nil {
			return nil, err
		}
	}
	return map[string]int{"series": len(rows)}, nil
}

func (s *Service) handlePending(ctx context.Context, t *queue.Task) (any, error) {
	var p payload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	rows, err := s.st.PendingImportItemsOfImport(ctx, p.ImportID)
	if err != nil {
		return nil, err
	}
	var next *time.Time
	waiting := 0
	for _, r := range rows {
		left, err := s.placePending(ctx, p.ImportID, r.Idx)
		if err != nil {
			return nil, err
		}
		if left > 0 && r.PendingUntil != nil {
			waiting++
			if next == nil || r.PendingUntil.Before(*next) {
				next = r.PendingUntil
			}
		}
	}
	if waiting == 0 {
		return nil, nil
	}
	// Again later; once the deadline is past, the next attempt gives up.
	wait := min(pendingBackoff(t.Attempts+1), max(next.Sub(s.now()), 0)+time.Second)
	return nil, &queue.RetryAfterError{Err: fmt.Errorf("%d series still have chapters to place", waiting), After: wait}
}

// placePending places a series' waiting chapters that its work has now,
// and returns how many still wait. Past its deadline, the rest are given
// up and reported.
func (s *Service) placePending(ctx context.Context, importID uuid.UUID, idx int32) (int32, error) {
	var left int32
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		row, err := q.LockImportItem(ctx, db.LockImportItemParams{ImportID: importID, Idx: idx})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		it := row.ImportItem
		if it.Pending == 0 || !it.WorkID.Valid {
			return nil
		}
		upd := db.SetImportItemPendingParams{ImportID: importID, Idx: idx, Placed: it.Placed, Unplaced: it.Unplaced, Message: it.Message}
		if it.PendingUntil != nil && !s.now().Before(*it.PendingUntil) {
			upd.Unplaced += it.Pending
			upd.Message = joinMessage(it.Message, fmt.Sprintf(whyNotPlacedFmt, upd.Unplaced))
			return q.SetImportItemPending(ctx, upd)
		}
		var chs []Chapter
		if err := json.Unmarshal(it.PendingChapters, &chs); err != nil {
			return err
		}
		res, err := s.place(ctx, q, row.UserID, it.WorkID.UUID, it.SourceID, it.SeriesKey, chs, row.ImportCreatedAt)
		if err != nil {
			return err
		}
		if res.placed == 0 && res.unplaced == 0 {
			left = it.Pending
			return nil // nothing new
		}
		upd.Placed += res.placed
		upd.Unplaced += res.unplaced
		if err := res.count(ctx, q, importID); err != nil {
			return err
		}
		deadline := s.now().Add(PendingFor)
		if it.PendingUntil != nil {
			deadline = *it.PendingUntil
		}
		if err := res.setPending(&upd.Pending, &upd.PendingChapters, &upd.PendingUntil, deadline); err != nil {
			return err
		}
		left = upd.Pending
		return q.SetImportItemPending(ctx, upd)
	})
	return left, err
}

// Resolution is the user's choice for a series: a catalogue entry, or
// skipping it.
type Resolution struct {
	EntryID string
	Skip    bool
}

// Resolve sets the work of one of an import's series by hand, or skips it.
// Before apply the choice waits for apply; after, the series is applied
// right away.
func (s *Service) Resolve(ctx context.Context, userID, id uuid.UUID, idx int32, r Resolution) (db.ImportItem, error) {
	imp, err := s.Get(ctx, userID, id)
	if err != nil {
		return db.ImportItem{}, err
	}
	if imp.State == StatePreviewing || imp.State == StateApplying {
		return db.ImportItem{}, ErrRunning
	}
	it, err := s.st.GetImportItem(ctx, db.GetImportItemParams{ImportID: id, Idx: idx})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ImportItem{}, ErrNoSuchItem
	}
	if err != nil {
		return db.ImportItem{}, err
	}
	choice := db.SetImportItemChoiceParams{
		ImportID: id, Idx: idx, Outcome: it.Outcome, WorkID: it.WorkID, EntryID: it.EntryID, SourceID: it.SourceID, SourceState: it.SourceState,
	}
	if r.Skip {
		if it.Result != "" {
			return db.ImportItem{}, ErrItemState
		}
		switch it.Outcome {
		case OutcomeReady, OutcomeInLibrary, OutcomeSourceMissing, OutcomeNeedsChoice, OutcomeNotFound, OutcomeNotAllowed:
		default:
			return db.ImportItem{}, ErrItemState
		}
		choice.Result = ResultSkipped
		if err := s.setChoice(ctx, choice); err != nil {
			return db.ImportItem{}, err
		}
		s.log.InfoContext(ctx, "import series skipped", "component", "audit", "actor", userID, "import", id, "idx", idx)
		return s.st.GetImportItem(ctx, db.GetImportItemParams{ImportID: id, Idx: idx})
	}
	switch {
	case it.Result == ResultSkipped:
	case it.Result == "" && (it.Outcome == OutcomeNeedsChoice || it.Outcome == OutcomeNotFound):
	default:
		return db.ImportItem{}, ErrItemState
	}
	v, err := s.viewer(ctx, userID)
	if err != nil {
		return db.ImportItem{}, err
	}
	if _, err := s.meta.GetEntry(ctx, r.EntryID, "", v.Meta()); errors.Is(err, meta.ErrNotFound) {
		return db.ImportItem{}, ErrNoSuchEntry
	} else if err != nil {
		return db.ImportItem{}, err
	}
	m := &matcher{s: s, v: v, sources: map[string]sourceInfo{}}
	si, err := m.source(ctx, it.SourceCompatID)
	if err != nil {
		return db.ImportItem{}, err
	}
	choice.EntryID, choice.WorkID, choice.SourceState, choice.SourceID = r.EntryID, uuid.NullUUID{}, si.state, ""
	if si.usable != nil {
		choice.SourceID = si.usable.ID
	}
	inLibrary := false
	w, err := s.st.GetWorkByEntry(ctx, r.EntryID)
	switch {
	case err == nil:
		lw, err := s.lib.Get(ctx, v, w.ID)
		if errors.Is(err, library.ErrNotFound) {
			return db.ImportItem{}, &NotAllowedError{whyHidden}
		}
		if err != nil {
			return db.ImportItem{}, err
		}
		choice.WorkID, inLibrary = uuid.NullUUID{UUID: w.ID, Valid: true}, lw.InLibrary
	case !errors.Is(err, pgx.ErrNoRows):
		return db.ImportItem{}, err
	}
	switch {
	case inLibrary:
		choice.Outcome = OutcomeInLibrary
	case !v.Grants.MayAdd:
		return db.ImportItem{}, &NotAllowedError{whyMayNotAdd}
	case si.state == sourceNotAllowed:
		return db.ImportItem{}, &NotAllowedError{whySource}
	case si.usable != nil:
		choice.Outcome = OutcomeReady
	default:
		choice.Outcome = OutcomeSourceMissing
	}
	if err := s.setChoice(ctx, choice); err != nil {
		return db.ImportItem{}, err
	}
	s.log.InfoContext(ctx, "import series resolved", "component", "audit", "actor", userID, "import", id, "idx", idx, "entry", r.EntryID)
	if imp.State != StatePreview {
		// Applied already: apply this series now.
		it, err := s.st.GetImportItem(ctx, db.GetImportItemParams{ImportID: id, Idx: idx})
		if err != nil {
			return db.ImportItem{}, err
		}
		if err := s.applyItem(ctx, m, imp, it); err != nil {
			return db.ImportItem{}, err
		}
		s.schedulePending(ctx, id, userID)
	}
	return s.st.GetImportItem(ctx, db.GetImportItemParams{ImportID: id, Idx: idx})
}

// setChoice records a resolution unless the series was applied or the
// import started running meanwhile.
func (s *Service) setChoice(ctx context.Context, p db.SetImportItemChoiceParams) error {
	n, err := s.st.SetImportItemChoice(ctx, p)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrItemState
	}
	return nil
}
