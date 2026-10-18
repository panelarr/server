// SPDX-License-Identifier: GPL-3.0-or-later

package chapters

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/store/db"
)

// The updates feed: chapters that appeared in a user's library, found by
// a later check of a series they weren't in before (not the source's
// upload date, and not what a series had when first listed).

// Update is one chapter in the feed.
type Update = db.UpdatesRow

// UpdateCursor is where a feed page ends.
type UpdateCursor struct {
	AppearedAt time.Time
	ChapterID  uuid.UUID
}

// Updates returns a page of the chapters that appeared in these works of
// a user's library after they added each, newest first.
func (s *Service) Updates(ctx context.Context, userID uuid.UUID, workIDs []uuid.UUID, before *UpdateCursor, limit int) ([]Update, error) {
	p := db.UpdatesParams{UserID: userID, WorkIds: workIDs, Lim: int32(limit)} //nolint:gosec // bounded
	if before != nil {
		p.Before = &before.AppearedAt
		p.BeforeID = uuid.NullUUID{UUID: before.ChapterID, Valid: true}
	}
	return s.st.Updates(ctx, p)
}

// CheckState is how the chapter checks of some works stand.
type CheckState struct {
	// LastCheckedAt is the latest check of any of their series; nil when
	// none was checked yet.
	LastCheckedAt *time.Time
	// Checking counts checks running or due now; Failing, series whose
	// last check failed.
	Checking, Failing int
}

// ChecksOf returns how the chapter checks of these works stand.
func (s *Service) ChecksOf(ctx context.Context, workIDs []uuid.UUID) (CheckState, error) {
	r, err := s.st.UpdateChecksOf(ctx, workIDs)
	if err != nil {
		return CheckState{}, err
	}
	out := CheckState{Checking: int(r.Checking), Failing: int(r.Failing)}
	if at, ok := r.LastCheckedAt.(time.Time); ok {
		out.LastCheckedAt = &at
	}
	return out, nil
}

// RefreshWorks queues chapter checks of every attached series of these
// works, ahead of scheduled ones, and returns how many series it queued.
func (s *Service) RefreshWorks(ctx context.Context, workIDs []uuid.UUID, by uuid.UUID) (int, error) {
	n := 0
	for _, id := range workIDs {
		queued, err := s.Refresh(ctx, id, by)
		if err != nil {
			return n, err
		}
		n += queued
	}
	return n, nil
}
