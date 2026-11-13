// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/store/db"
)

// How an imported series stands on its work, after AttachImported.
const (
	// ImportAttached: the series is now attached, by this call.
	ImportAttached = "attached"
	// ImportAttachedBefore: the same series was attached already.
	ImportAttachedBefore = "attached_before"
	// ImportKeptOther: another series of the source is attached; it
	// stays, and the imported one is recorded as a candidate.
	ImportKeptOther = "kept_other"
	// ImportRejected: the series was rejected for the work before (by
	// someone fixing its match); it isn't attached again.
	ImportRejected = "rejected"
	// ImportUnavailable: the source didn't answer for the series.
	ImportUnavailable = "unavailable"
)

// AttachImported attaches a series that an import of another app's
// library found for a work, as v (who may use the source). Works are
// shared, so it never replaces a series that is attached already: it
// attaches only when the work has none on that source, and never one
// that was rejected for the work. The series' details come from the
// source; when the source doesn't answer, nothing is attached.
func (s *Service) AttachImported(ctx context.Context, v Viewer, workID uuid.UUID, sourceID, key string) (string, error) {
	se, err := s.host.Series(ctx, v.plugins(), sourceID, key)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.log.InfoContext(ctx, "import: series unavailable, not attached", "work", workID, "source", sourceID, "err", err)
		return ImportUnavailable, nil
	}
	if se.GetKey() == "" {
		se.Key = key
	}
	state, out, err := s.attachImported(ctx, workID, sourceID, se)
	if isUniqueViolation(err) {
		// Matching attached a series on the source meanwhile (it doesn't
		// take the work's lock): look again.
		state, out, err = s.attachImported(ctx, workID, sourceID, se)
	}
	if err != nil {
		return "", err
	}
	if state == ImportAttached {
		s.log.InfoContext(ctx, "series attached", "component", "audit", "actor", v.UserID, "work", workID, "source", sourceID, "origin", OriginImport)
		s.hooks.SeriesAttached(ctx, out)
		s.hooks.WorkChanged(ctx, workID)
		s.notify(ctx, workID)
	}
	return state, nil
}

func (s *Service) attachImported(ctx context.Context, workID uuid.UUID, sourceID string, se *sourcev1.Series) (string, db.SourceSeries, error) {
	state := ""
	var out db.SourceSeries
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		// The work's lock first, as every writer of its series and
		// chapters takes it.
		if err := q.LockWork(ctx, workID); err != nil {
			return err
		}
		params := db.UpsertSourceSeriesParams{
			WorkID: workID, SourceID: sourceID, Key: se.GetKey(), Title: se.GetTitle(), Url: se.GetUrl(), CoverRef: se.GetCoverUrl(),
			State: StateCandidate, Origin: OriginImport, Score: 1, Evidence: Evidence{MatchedTitle: se.GetTitle(), TitleScore: 1}.json(),
		}
		cur, err := q.AttachedOnSource(ctx, db.AttachedOnSourceParams{WorkID: workID, SourceID: sourceID})
		switch {
		case err == nil && cur.Key == se.GetKey():
			state = ImportAttachedBefore
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		other := err == nil
		prev, err := q.GetSourceSeriesByKey(ctx, db.GetSourceSeriesByKeyParams{WorkID: workID, SourceID: sourceID, Key: se.GetKey()})
		switch {
		case other && errors.Is(err, pgx.ErrNoRows):
			// Recorded as a candidate, for someone fixing the match.
			state = ImportKeptOther
			_, err := q.UpsertSourceSeries(ctx, params)
			return err
		case other:
			state = ImportKeptOther
			return err
		case err == nil && prev.State == StateRejected:
			state = ImportRejected
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		row, err := q.UpsertSourceSeries(ctx, params)
		if err != nil {
			return err
		}
		if out, err = q.SetSourceSeriesState(ctx, db.SetSourceSeriesStateParams{ID: row.ID, State: StateAttached, Origin: OriginImport}); err != nil {
			return err
		}
		state = ImportAttached
		return q.UpsertWorkSourceMatch(ctx, db.UpsertWorkSourceMatchParams{
			WorkID: workID, SourceID: sourceID, Status: MatchMatched, Message: "attached by an import", Queries: []string{},
		})
	})
	return state, out, err
}
