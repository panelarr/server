// SPDX-License-Identifier: GPL-3.0-or-later

package library_test

import (
	"context"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/library"
)

// TestMatchingAndAttachByHandQueue: matching replaces a work's candidates
// while someone attaches one of them by hand. Both take the work's lock
// first, so they run one after the other. Before matching took it, the
// attach held the work and waited on the dropped candidate while matching's
// insert waited on the work: a deadlock (SQLSTATE 40P01).
func TestMatchingAndAttachByHandQueue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	added, err := f.lib.Add(ctx, f.user, "e2", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := waitMatched(t, f, added.Work.ID)
	en := sourceOf(t, w, "fake:1001")
	if len(en.Candidates) != 1 {
		t.Fatalf("candidates: %+v", en)
	}
	k := en.Candidates[0]

	attached := make(chan error, 1)
	pause := func() {
		// Matching has dropped the candidate and holds it: attach it by
		// hand now, and go on once the attach waits on a lock.
		go func() {
			_, err := f.lib.AttachRef(ctx, f.user, w.ID, library.SourceRef{SourceID: k.SourceID, Key: k.Key}, library.OriginManual)
			attached <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				t.Error(err)
				return
			}
			if waiting > 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("the attach never waited")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	saved := library.SaveMatchesPaused(ctx, f.lib, w.ID, k.SourceID, pause,
		&sourcev1.Series{Key: "/series/9", Title: "Salt and Iron?"}, &sourcev1.Series{Key: k.Key, Title: k.Title})
	if saved != nil {
		t.Fatalf("matching: %v", saved)
	}
	if err := <-attached; err != nil {
		t.Fatalf("attach by hand: %v", err)
	}
	got := sourceOf(t, waitMatched(t, f, w.ID), k.SourceID)
	if got.Attached == nil || got.Attached.Key != k.Key || got.Attached.Origin != library.OriginManual {
		t.Fatalf("after both: %+v", got)
	}
}
