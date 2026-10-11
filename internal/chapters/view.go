// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store/db"
)

// VariantView is a variant with its source's name.
type VariantView struct {
	Variant
	SourceName string
}

// Chapter is a canonical chapter as a user sees it.
type Chapter struct {
	db.Chapter
	Chosen   *VariantView
	Variants int
	// New: found after the user last looked at the work.
	New bool
}

// List returns a work's chapters in reading order (numbered ones by
// number, extras after), with the chosen variant and whether each is new
// for userID.
func (s *Service) List(ctx context.Context, workID, userID uuid.UUID) ([]Chapter, error) {
	chs, err := s.st.ListChapters(ctx, workID)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.WorkVariants(ctx, workID)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]VariantView{}
	counts := map[uuid.UUID]int{}
	for _, r := range rows {
		byID[r.SourceChapter.ID] = VariantView{Variant: variantOf(r), SourceName: r.SourceName}
		if r.SourceChapter.ChapterID.Valid {
			counts[r.SourceChapter.ChapterID.UUID]++
		}
	}
	var seen *time.Time
	if le, err := s.st.GetLibraryEntry(ctx, db.GetLibraryEntryParams{UserID: userID, WorkID: workID}); err == nil {
		seen = &le.SeenAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	out := make([]Chapter, 0, len(chs))
	for _, c := range chs {
		ch := Chapter{Chapter: c, Variants: counts[c.ID], New: seen != nil && c.Available && c.CreatedAt.After(*seen)}
		if c.PreferredVariantID.Valid {
			if v, ok := byID[c.PreferredVariantID.UUID]; ok {
				ch.Chosen = &v
			}
		}
		out = append(out, ch)
	}
	slices.SortStableFunc(out, func(a, b Chapter) int {
		switch {
		case a.Number == nil && b.Number == nil:
			return a.CreatedAt.Compare(b.CreatedAt)
		case a.Number == nil:
			return 1
		case b.Number == nil:
			return -1
		case *a.Number < *b.Number:
			return -1
		case *a.Number > *b.Number:
			return 1
		}
		return 0
	})
	return out, nil
}

// Detail is one chapter with all its variants.
type Detail struct {
	Chapter
	All []VariantView
}

// Get returns a chapter of a work with all its variants.
func (s *Service) Get(ctx context.Context, workID, chapterID, userID uuid.UUID) (Detail, error) {
	c, err := s.st.GetChapter(ctx, chapterID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && c.WorkID != workID) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	rows, err := s.st.ChapterVariants(ctx, chapterID)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Chapter: Chapter{Chapter: c, Variants: len(rows)}}
	for _, r := range rows {
		v := VariantView{Variant: variantOf(db.WorkVariantsRow(r)), SourceName: r.SourceName}
		d.All = append(d.All, v)
		if c.PreferredVariantID.Valid && v.ID == c.PreferredVariantID.UUID {
			chosen := v
			d.Chosen = &chosen
		}
	}
	if le, err := s.st.GetLibraryEntry(ctx, db.GetLibraryEntryParams{UserID: userID, WorkID: workID}); err == nil {
		d.New = c.Available && c.CreatedAt.After(le.SeenAt)
	}
	return d, nil
}

// SeriesChapters returns a series' chapters exactly as its source lists
// them (the per-source view), including ones it no longer lists.
func (s *Service) SeriesChapters(ctx context.Context, workID, seriesID uuid.UUID) (db.SourceSeries, []db.SourceChapter, error) {
	se, err := s.st.GetSourceSeries(ctx, seriesID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && se.WorkID != workID) {
		return db.SourceSeries{}, nil, ErrNotFound
	}
	if err != nil {
		return db.SourceSeries{}, nil, err
	}
	chs, err := s.st.ListSourceChaptersBySeries(ctx, seriesID)
	return se, chs, err
}

// MarkSeen clears a user's "new" badges of a work.
func (s *Service) MarkSeen(ctx context.Context, userID, workID uuid.UUID) (bool, error) {
	n, err := s.st.MarkLibrarySeen(ctx, db.MarkLibrarySeenParams{UserID: userID, WorkID: workID})
	return n > 0, err
}

// NewCounts returns how many new chapters each of a user's works has.
func (s *Service) NewCounts(ctx context.Context, userID uuid.UUID, workIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := s.st.NewChaptersForUser(ctx, db.NewChaptersForUserParams{UserID: userID, WorkIds: workIDs})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int64, len(rows))
	for _, r := range rows {
		out[r.WorkID] = r.New
	}
	return out, nil
}
