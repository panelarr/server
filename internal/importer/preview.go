// SPDX-License-Identifier: GPL-3.0-or-later

package importer

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

type payload struct {
	ImportID uuid.UUID `json:"import_id"`
}

// Source states of a series, for the user.
const (
	sourceUsable     = "usable"
	sourceNotAllowed = "not_allowed"
	sourceMissing    = "missing"
)

// ProgressEvent is the data of import.progress.
type ProgressEvent struct {
	ImportID uuid.UUID `json:"import_id"`
	Done     int64     `json:"done"`
	Total    int64     `json:"total"`
}

// FinishedEvent is the data of import.finished.
type FinishedEvent struct {
	ImportID uuid.UUID `json:"import_id"`
	State    string    `json:"state"`
}

// progressEvery throttles import.progress events.
const progressEvery = 500 * time.Millisecond

// SettleStale marks the user's imports whose task ended without finishing
// them as failed, so they neither show as running forever nor block a new
// import.
func (s *Service) SettleStale(ctx context.Context, userID uuid.UUID) error {
	_, err := s.st.FailStaleImports(ctx, userID)
	return err
}

// handlePreview matches an import's series that aren't matched yet, in
// file order, so a preview interrupted by a restart picks up where it
// stopped.
func (s *Service) handlePreview(ctx context.Context, t *queue.Task) (any, error) {
	var p payload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	imp, err := s.st.GetImportByID(ctx, p.ImportID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // deleted meanwhile
	}
	if err != nil {
		return nil, err
	}
	if imp.State != StatePreviewing {
		return nil, nil
	}
	err = s.preview(ctx, t, imp)
	if err == nil {
		return nil, nil
	}
	if ctx.Err() == nil && t.Attempts >= t.MaxAttempts {
		s.finish(context.WithoutCancel(ctx), imp, StateFailed, nil, err.Error())
	}
	return nil, err
}

func (s *Service) preview(ctx context.Context, t *queue.Task, imp db.Import) error {
	v, err := s.viewer(ctx, imp.UserID)
	if err != nil {
		return err
	}
	counts, err := s.st.ImportOutcomeCounts(ctx, imp.ID)
	if err != nil {
		return err
	}
	var done, total int64
	for _, c := range counts {
		total += int64(c.Series)
		if c.Outcome != OutcomePending {
			done += int64(c.Series)
		}
	}
	m := &matcher{s: s, v: v, sources: map[string]sourceInfo{}}
	var last time.Time
	report := func(force bool) {
		t.SetProgress(queue.Progress{Phase: "matching", Done: done, Total: total, Unit: "series"})
		if force || time.Since(last) >= progressEvery {
			last = time.Now()
			s.hub.Publish(EventProgress, ProgressEvent{ImportID: imp.ID, Done: done, Total: total}, events.Audience{Users: []uuid.UUID{imp.UserID}})
		}
	}
	report(true)
	after := int32(-1)
	for {
		rows, err := s.st.PendingImportItems(ctx, db.PendingImportItemsParams{ImportID: imp.ID, After: after, Lim: 50})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, it := range rows {
			after = it.Idx
			res, err := m.match(ctx, it)
			if err != nil {
				return err
			}
			cands, err := json.Marshal(res.candidates)
			if err != nil {
				return err
			}
			if err := s.st.SetImportItemMatch(ctx, db.SetImportItemMatchParams{
				ImportID: imp.ID, Idx: it.Idx, Outcome: res.outcome, WorkID: res.workID, EntryID: res.entryID,
				SourceID: res.sourceID, SourceState: res.sourceState, Candidates: cands, Message: res.message,
			}); err != nil {
				return err
			}
			done++
			report(false)
		}
	}
	report(true)
	hints, err := s.packageHints(ctx, imp.ID)
	if err != nil {
		return err
	}
	s.finish(ctx, imp, StatePreview, hints, "")
	return nil
}

// viewer is the importing user with their grants as they are now.
func (s *Service) viewer(ctx context.Context, userID uuid.UUID) (library.Viewer, error) {
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return library.Viewer{}, err
	}
	return library.Viewer{UserID: u.ID, Grants: users.Effective(u), Admin: u.Role == auth.RoleAdmin, HideUnknown: s.meta.HideUnknown(ctx)}, nil
}

// finish ends a preview and tells the user.
func (s *Service) finish(ctx context.Context, imp db.Import, state string, hints []PackageHint, message string) {
	extra := map[string]any{}
	if hints != nil {
		extra["package_hints"] = hints
	}
	raw, _ := json.Marshal(extra)
	if _, err := s.st.FinishImportPreview(ctx, db.FinishImportPreviewParams{ID: imp.ID, State: state, Summary: raw, Message: message}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.WarnContext(ctx, "finish import preview", "import", imp.ID, "err", err)
		}
		return
	}
	s.hub.Publish(EventFinished, FinishedEvent{ImportID: imp.ID, State: state}, events.Audience{Users: []uuid.UUID{imp.UserID}})
}

// sourceInfo is how a backup's source stands for the importing user.
type sourceInfo struct {
	// ids of every Panelarr source known by the compat ID, best first.
	ids []string
	// usable is the source the user may use, if any.
	usable *plugins.Source
	state  string
}

// matcher matches one import's series for one user.
type matcher struct {
	s       *Service
	v       library.Viewer
	sources map[string]sourceInfo
}

type matchResult struct {
	outcome, entryID, sourceID, sourceState, message string
	workID                                           uuid.NullUUID
	candidates                                       []Candidate
}

func (m *matcher) source(ctx context.Context, compatID string) (sourceInfo, error) {
	if si, ok := m.sources[compatID]; ok {
		return si, nil
	}
	pv := m.v.Plugins()
	srcs, err := m.s.host.SourcesByCompatID(ctx, pv, compatID)
	if err != nil {
		return sourceInfo{}, err
	}
	si := sourceInfo{state: sourceMissing}
	for i := range srcs {
		si.ids = append(si.ids, srcs[i].ID)
		switch {
		case si.usable == nil && pv.Visible(srcs[i]):
			si.usable = &srcs[i]
			si.state = sourceUsable
		case si.usable == nil && srcs[i].Usable():
			// Usable for others, not for this user.
			si.state = sourceNotAllowed
		}
	}
	m.sources[compatID] = si
	return si, nil
}

// match finds the work of one series, in this order: (1) a work that has
// the same series attached on one of the backup source's Panelarr
// sources; (2) the catalogue, by the source's own details of the series;
// (3) when the source isn't usable, the catalogue by the backup's title
// and people. Then the user's grants decide the outcome.
func (m *matcher) match(ctx context.Context, it db.PendingImportItemsRow) (matchResult, error) {
	si, err := m.source(ctx, it.SourceCompatID)
	if err != nil {
		return matchResult{}, err
	}
	res := matchResult{sourceState: si.state, candidates: []Candidate{}}
	if si.usable != nil {
		res.sourceID = si.usable.ID
	}
	var workID uuid.UUID
	if len(si.ids) > 0 {
		row, err := m.s.st.AttachedSeriesByKey(ctx, db.AttachedSeriesByKeyParams{SourceIds: si.ids, Key: it.SeriesKey})
		switch {
		case err == nil:
			workID = row.WorkID
		case !errors.Is(err, pgx.ErrNoRows):
			return matchResult{}, err
		}
	}
	var strongAbove bool
	if workID == uuid.Nil {
		names, people := []string{it.Title}, append(splitPeople(it.Author), splitPeople(it.Artist)...)
		if si.usable != nil {
			if se, err := m.s.host.Series(ctx, m.v.Plugins(), si.usable.ID, it.SeriesKey); err == nil {
				names = append([]string{se.GetTitle()}, se.GetAltTitles()...)
				if it.Title != "" && !slices.Contains(names, it.Title) {
					names = append(names, it.Title)
				}
				people = append(slices.Clone(se.GetAuthors()), se.GetArtists()...)
				if len(people) == 0 {
					people = append(splitPeople(it.Author), splitPeople(it.Artist)...)
				}
			} else if ctx.Err() != nil {
				return matchResult{}, ctx.Err()
			} else {
				m.s.log.InfoContext(ctx, "import: series details unavailable, matching by the backup's title", "source", si.usable.ID, "err", err)
			}
		}
		// The catalogue is searched past the user's ceiling, so a series
		// above it shows as not allowed instead of not found; entries
		// above it are never offered.
		all := library.Viewer{UserID: m.v.UserID, Admin: true, Grants: users.AdminGrants()}
		em, err := m.s.lib.MatchEntry(ctx, all, names, people)
		if err != nil {
			return matchResult{}, err
		}
		for _, c := range em.Candidates {
			if !m.v.AllowsRating(meta.RatingName(c.Entry.GetContentRating())) {
				continue
			}
			res.candidates = append(res.candidates, Candidate{
				EntryID: c.Entry.GetId(), Title: c.Entry.GetTitle(), Year: c.Entry.GetYear(),
				Score: c.Score, MatchedTitle: c.MatchedTitle, AuthorsMatch: c.AuthorsMatch,
			})
		}
		if em.Strong != nil {
			if !m.v.AllowsRating(meta.RatingName(em.Strong.Entry.GetContentRating())) {
				strongAbove = true
			} else {
				res.entryID = em.Strong.Entry.GetId()
				w, err := m.s.st.GetWorkByEntry(ctx, res.entryID)
				switch {
				case err == nil:
					workID = w.ID
				case !errors.Is(err, pgx.ErrNoRows):
					return matchResult{}, err
				}
			}
		}
	}
	inLibrary := false
	if workID != uuid.Nil {
		w, err := m.s.lib.Get(ctx, m.v, workID)
		switch {
		case errors.Is(err, library.ErrNotFound):
			return m.notAllowed(res, "the work is above your content rating limit or only on sources you may not use"), nil
		case err != nil:
			return matchResult{}, err
		}
		res.workID = uuid.NullUUID{UUID: workID, Valid: true}
		res.entryID = w.MetaEntryID
		inLibrary = w.InLibrary
	}
	switch {
	case strongAbove:
		return m.notAllowed(res, "the work is above your content rating limit"), nil
	case inLibrary:
		res.outcome = OutcomeInLibrary
	case !m.v.Grants.MayAdd:
		return m.notAllowed(res, "your account may not add works"), nil
	case si.state == sourceNotAllowed:
		return m.notAllowed(res, "you may not use this source"), nil
	case res.entryID != "" && si.usable != nil:
		res.outcome = OutcomeReady
	case res.entryID != "":
		res.outcome = OutcomeSourceMissing
	case len(res.candidates) > 0:
		res.outcome = OutcomeNeedsChoice
	default:
		res.outcome = OutcomeNotFound
	}
	return res, nil
}

func (m *matcher) notAllowed(res matchResult, why string) matchResult {
	res.outcome, res.message = OutcomeNotAllowed, why
	res.candidates = []Candidate{}
	return res
}

// splitPeople splits a backup's author or artist field, which apps fill
// with a comma-separated list.
func splitPeople(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// packageHints names the packages that would provide the sources the
// import is missing: packages of enabled plugins, not installed yet, that
// list one of those sources' compat IDs. A plugin that fails to answer is
// skipped.
func (s *Service) packageHints(ctx context.Context, importID uuid.UUID) ([]PackageHint, error) {
	missing, err := s.st.ImportMissingSources(ctx, importID)
	if err != nil || len(missing) == 0 {
		return []PackageHint{}, err
	}
	series := make(map[string]int, len(missing))
	for _, r := range missing {
		series[r.SourceCompatID] = int(r.Series)
	}
	ps, err := s.host.List(ctx)
	if err != nil {
		return nil, err
	}
	hints := []PackageHint{}
	for _, p := range ps {
		if !p.Enabled || !plugins.HasPackages(p.Info) {
			continue
		}
		pkgs, err := s.host.Packages(ctx, p.ID, plugins.PackageQuery{})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.log.InfoContext(ctx, "import: list packages for hints", "plugin", p.ID, "err", err)
			continue
		}
		for _, pkg := range pkgs {
			if pkg.GetInstalled() {
				continue
			}
			seen := map[string]bool{}
			n := 0
			for _, ps := range pkg.GetSources() {
				for _, id := range ps.GetCompatIds() {
					if !seen[id] {
						seen[id] = true
						n += series[id]
					}
				}
			}
			if n > 0 {
				hints = append(hints, PackageHint{PluginID: p.ID, PackageID: pkg.GetId(), PackageName: pkg.GetName(), Series: n})
			}
		}
	}
	slices.SortStableFunc(hints, func(a, b PackageHint) int { return b.Series - a.Series })
	return hints, nil
}
