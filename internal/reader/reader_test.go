// SPDX-License-Identifier: GPL-3.0-or-later

package reader

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/store/db"
)

func TestCheckImage(t *testing.T) {
	for in, want := range map[string]string{"image/png": "image/png", "image/jpg": "image/jpeg", "image/webp; q=1": "image/webp"} {
		if got, err := checkImage(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"image/svg+xml", "text/html", ""} {
		if _, err := checkImage(bad); !errors.Is(err, ErrNotImage) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A finished last chapter opened again (its row saved later at page 0, still
// completed) leaves nothing to continue; once marked unread it is where to
// continue.
func TestContinueAtReReadLastChapter(t *testing.T) {
	order := []readingChapter{{ID: uuid.New(), Available: true}, {ID: uuid.New(), Available: true}}
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	rows := []db.Progress{
		{ChapterID: order[0].ID, Page: 3, Completed: true, UpdatedAt: t0},
		{ChapterID: order[1].ID, Page: 0, Completed: true, UpdatedAt: t0.Add(time.Hour)},
	}
	if got := continueAt(order, rows); got != nil {
		t.Fatalf("continue at %s with everything read", got)
	}
	// The earlier chapter re-read later: continue after it, at nothing unread.
	rows[0].UpdatedAt = t0.Add(2 * time.Hour)
	rows[0].Page = 0
	if got := continueAt(order, rows); got != nil {
		t.Fatalf("continue at %s after re-reading the first chapter", got)
	}
	rows[1].Completed = false
	rows[1].UpdatedAt = t0.Add(3 * time.Hour)
	if got := continueAt(order, rows); got == nil || *got != order[1].ID {
		t.Fatalf("continue at %v, want the unread last chapter", got)
	}
}
