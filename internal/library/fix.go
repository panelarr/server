// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/textmatch"
)

// workSeries returns a series of a work that v may see, or ErrNotFound.
func (s *Service) workSeries(ctx context.Context, v Viewer, workID, seriesID uuid.UUID) (db.SourceSeries, error) {
	if _, err := s.Get(ctx, v, workID); err != nil {
		return db.SourceSeries{}, err
	}
	se, err := s.st.GetSourceSeries(ctx, seriesID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && se.WorkID != workID) {
		return db.SourceSeries{}, ErrNotFound
	}
	if err != nil {
		return db.SourceSeries{}, err
	}
	if _, err := s.host.Source(ctx, se.SourceID, v.plugins()); err != nil {
		return db.SourceSeries{}, ErrNotFound
	}
	return se, nil
}

// Attach makes a candidate (or a rejected series) the work's series on its
// source; the one attached before becomes a candidate.
func (s *Service) Attach(ctx context.Context, v Viewer, workID, seriesID uuid.UUID) (db.SourceSeries, error) {
	se, err := s.workSeries(ctx, v, workID, seriesID)
	if err != nil {
		return db.SourceSeries{}, err
	}
	var out db.SourceSeries
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.DemoteAttached(ctx, db.DemoteAttachedParams{WorkID: workID, SourceID: se.SourceID}); err != nil {
			return err
		}
		out, err = q.SetSourceSeriesState(ctx, db.SetSourceSeriesStateParams{ID: se.ID, State: StateAttached, Origin: OriginManual})
		if err != nil {
			return err
		}
		return q.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: workID, SourceID: se.SourceID, Status: MatchMatched, Message: "attached by hand", Queries: []string{},
		})
	})
	if err != nil {
		return db.SourceSeries{}, err
	}
	s.log.InfoContext(ctx, "series attached", "component", "audit", "actor", v.UserID, "work", workID, "series", seriesID)
	s.hooks.SeriesAttached(ctx, out)
	s.hooks.WorkChanged(ctx, workID)
	s.notify(ctx, workID)
	return out, nil
}

// Detach rejects a series for the work: matching won't attach it again.
func (s *Service) Detach(ctx context.Context, v Viewer, workID, seriesID uuid.UUID) (db.SourceSeries, error) {
	se, err := s.workSeries(ctx, v, workID, seriesID)
	if err != nil {
		return db.SourceSeries{}, err
	}
	out, err := s.st.SetSourceSeriesState(ctx, db.SetSourceSeriesStateParams{ID: se.ID, State: StateRejected, Origin: se.Origin})
	if err != nil {
		return db.SourceSeries{}, err
	}
	if se.State == StateAttached {
		if err := s.st.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: workID, SourceID: se.SourceID, Status: MatchCandidates, Message: "detached by hand", Queries: []string{},
		}); err != nil {
			return db.SourceSeries{}, err
		}
	}
	s.log.InfoContext(ctx, "series detached", "component", "audit", "actor", v.UserID, "work", workID, "series", seriesID)
	s.hooks.WorkChanged(ctx, workID)
	s.notify(ctx, workID)
	return out, nil
}

// SetPriority sets a series' priority for choosing chapter variants.
func (s *Service) SetPriority(ctx context.Context, v Viewer, workID, seriesID uuid.UUID, priority int32) (db.SourceSeries, error) {
	if _, err := s.workSeries(ctx, v, workID, seriesID); err != nil {
		return db.SourceSeries{}, err
	}
	if _, err := s.st.SetSourcePriority(ctx, db.SetSourcePriorityParams{ID: seriesID, WorkID: workID, Priority: priority}); err != nil {
		return db.SourceSeries{}, err
	}
	s.hooks.WorkChanged(ctx, workID)
	s.notify(ctx, workID)
	return s.st.GetSourceSeries(ctx, seriesID)
}

// AttachRef attaches a series given by source and key (e.g. picked while
// browsing).
func (s *Service) AttachRef(ctx context.Context, v Viewer, workID uuid.UUID, ref SourceRef, origin string) (db.SourceSeries, error) {
	if _, err := s.Get(ctx, v, workID); err != nil {
		return db.SourceSeries{}, err
	}
	se, err := s.host.Series(ctx, v.plugins(), ref.SourceID, ref.Key)
	if err != nil {
		return db.SourceSeries{}, err
	}
	if se.GetKey() == "" {
		se.Key = ref.Key
	}
	return s.attachSeries(ctx, v, workID, ref.SourceID, se.GetKey(), se.GetTitle(), se.GetUrl(), se.GetCoverUrl(), origin)
}

// AttachURL attaches the series a pasted URL points to.
func (s *Service) AttachURL(ctx context.Context, v Viewer, workID uuid.UUID, rawURL string) (db.SourceSeries, error) {
	if _, err := s.Get(ctx, v, workID); err != nil {
		return db.SourceSeries{}, err
	}
	r, err := s.host.ResolveURL(ctx, v.plugins(), rawURL)
	if err != nil {
		return db.SourceSeries{}, err
	}
	se := r.Series
	return s.attachSeries(ctx, v, workID, r.Source.ID, se.GetKey(), se.GetTitle(), se.GetUrl(), se.GetCoverUrl(), OriginURL)
}

func (s *Service) attachSeries(ctx context.Context, v Viewer, workID uuid.UUID, sourceID, key, title, url, cover, origin string) (db.SourceSeries, error) {
	var out db.SourceSeries
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.DemoteAttached(ctx, db.DemoteAttachedParams{WorkID: workID, SourceID: sourceID}); err != nil {
			return err
		}
		row, err := q.UpsertSourceSeries(ctx, db.UpsertSourceSeriesParams{
			WorkID: workID, SourceID: sourceID, Key: key, Title: title, Url: url, CoverRef: cover,
			State: StateAttached, Origin: origin, Score: 1, Evidence: Evidence{MatchedTitle: title, TitleScore: 1}.json(),
		})
		if err != nil {
			return err
		}
		if out, err = q.SetSourceSeriesState(ctx, db.SetSourceSeriesStateParams{ID: row.ID, State: StateAttached, Origin: origin}); err != nil {
			return err
		}
		return q.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: workID, SourceID: sourceID, Status: MatchMatched, Message: "attached by hand", Queries: []string{},
		})
	})
	if err != nil {
		return db.SourceSeries{}, err
	}
	s.log.InfoContext(ctx, "series attached", "component", "audit", "actor", v.UserID, "work", workID, "source", sourceID)
	s.hooks.SeriesAttached(ctx, out)
	s.hooks.WorkChanged(ctx, workID)
	s.notify(ctx, workID)
	return out, nil
}

// EntryCandidate is a catalogue entry that may be a source's series.
type EntryCandidate struct {
	Entry        *metav1.EntrySummary
	Score        float64
	MatchedTitle string
	AuthorsMatch *bool
}

// FromSource is the outcome of adding a series found on a source.
type FromSource struct {
	// Added is set when the series' catalogue entry was clear: the work is
	// in the library with the series attached.
	Added *Added
	// Candidates otherwise: the caller picks one and adds it with the
	// series attached.
	Candidates []EntryCandidate
}

// AddFromSource finds the catalogue entry of a series seen on a source and,
// when it is clear (the exact title and a shared author, one such entry),
// adds that work with the series attached.
func (s *Service) AddFromSource(ctx context.Context, v Viewer, ref SourceRef) (FromSource, error) {
	se, err := s.host.Series(ctx, v.plugins(), ref.SourceID, ref.Key)
	if err != nil {
		return FromSource{}, err
	}
	names := append([]string{se.GetTitle()}, se.GetAltTitles()...)
	people := append(slices.Clone(se.GetAuthors()), se.GetArtists()...)
	seen := map[string]*EntryCandidate{}
	var order []string
	for i, q := range names {
		if i >= 3 {
			break
		}
		res, err := s.meta.Search(ctx, meta.Query{Text: q, PageSize: 10}, v.meta())
		if err != nil {
			return FromSource{}, err
		}
		for _, h := range res.GetHits() {
			id := h.GetEntry().GetId()
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = &EntryCandidate{Entry: h.GetEntry()}
			order = append(order, id)
		}
	}
	var cands []*EntryCandidate
	for i, id := range order {
		c := seen[id]
		titles := []string{c.Entry.GetTitle()}
		if i < 5 { // full titles and people for the first few
			if e, err := s.meta.GetEntry(ctx, id, "", v.meta()); err == nil {
				titles = entryTitles(e, "")
				if ep := entryPeople(e); len(ep) > 0 && len(people) > 0 {
					m := textmatch.PeopleOverlap(ep, people)
					c.AuthorsMatch = &m
				}
			}
		}
		c.Score, _, c.MatchedTitle = textmatch.Best(names, titles)
		c.Score = round(c.Score)
		if c.Score >= DefaultMatchSettings.CandidateScore {
			cands = append(cands, c)
		}
	}
	slices.SortStableFunc(cands, func(a, b *EntryCandidate) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	var strong []*EntryCandidate
	for _, c := range cands {
		if c.Score >= 1 && c.AuthorsMatch != nil && *c.AuthorsMatch {
			strong = append(strong, c)
		}
	}
	if len(strong) == 1 {
		added, err := s.Add(ctx, v, strong[0].Entry.GetId(), &ref)
		if err != nil {
			return FromSource{}, err
		}
		return FromSource{Added: &added}, nil
	}
	out := FromSource{}
	for i, c := range cands {
		if i >= 5 {
			break
		}
		out.Candidates = append(out.Candidates, *c)
	}
	return out, nil
}

// SourceSearch is a search of one source for a work, with the series the
// work already knows on it, by key.
type SourceSearch struct {
	plugins.Page
	Known map[string]db.SourceSeries
}

// SearchSource searches one source v may use for the work, to fix its
// match by hand. ErrNotFound when v can't see the work or use the source.
func (s *Service) SearchSource(ctx context.Context, v Viewer, workID uuid.UUID, sourceID, query string, page uint32, filters []*sourcev1.FilterValue) (SourceSearch, error) {
	if _, err := s.Get(ctx, v, workID); err != nil {
		return SourceSearch{}, err
	}
	p, err := s.host.Search(ctx, v.plugins(), sourceID, query, page, filters)
	if err != nil {
		return SourceSearch{}, err
	}
	series, err := s.st.ListSourceSeries(ctx, workID)
	if err != nil {
		return SourceSearch{}, err
	}
	out := SourceSearch{Page: p, Known: map[string]db.SourceSeries{}}
	for _, se := range series {
		if se.SourceID == sourceID {
			out.Known[se.Key] = se
		}
	}
	return out, nil
}
