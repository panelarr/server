// SPDX-License-Identifier: GPL-3.0-or-later

package chapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/storetest"
)

// Something that starts pointing at a ghost chapter while Unify deletes
// ghosts must not be lost to the delete's cascade. Writers don't take the
// work's lock, so Unify has to see what they committed meanwhile instead of
// deciding on an older snapshot.
//
// A progress (or bookmark) row also refers to the work, so its writer and
// Unify's work lock already serialize. A row that refers only to the
// chapter (here a downloaded file's record) doesn't: Unify waits on the
// chapter row alone, which is the case locking the candidates first guards.
func TestUnifyKeepsWhatIsWrittenMeanwhile(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, insert, count string
	}{
		{"progress",
			`INSERT INTO progress (user_id, chapter_id, work_id, page) VALUES ($1, $2, $3, 1)`,
			`SELECT count(*) FROM progress WHERE chapter_id = $1`},
		{"file",
			`INSERT INTO chapter_files (chapter_id, storage_id, key, pages, bytes, sha256)
			SELECT $2, 'local', 'c.cbz', 4, 1, 'x' WHERE $1::uuid IS NOT NULL AND $3::uuid IS NOT NULL`,
			`SELECT count(*) FROM chapter_files WHERE chapter_id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := storetest.New(t)
			hub := events.NewHub()
			svc := chapters.NewService(st, nil, queue.New(st, hub, nil, nil, nil), nil, hub, nil)

			var workID, userID, chapterID uuid.UUID
			if err := st.Pool.QueryRow(ctx, `INSERT INTO works (meta_entry_id, title, sort_title) VALUES ('e1', 'W', 'w') RETURNING id`).Scan(&workID); err != nil {
				t.Fatal(err)
			}
			if err := st.Pool.QueryRow(ctx, `INSERT INTO users (username, password_hash, role) VALUES ('reader', 'x', 'user') RETURNING id`).Scan(&userID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool.Exec(ctx, `INSERT INTO storages (id, name, kind) VALUES ('local', 'Local', 'local')`); err != nil {
				t.Fatal(err)
			}
			// A ghost: no series has a variant for it.
			if err := st.Pool.QueryRow(ctx, `INSERT INTO chapters (work_id, number) VALUES ($1, 1) RETURNING id`, workID).Scan(&chapterID); err != nil {
				t.Fatal(err)
			}

			// The write is in flight (not committed yet) when Unify runs.
			tx, err := st.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, c.insert, userID, chapterID, workID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := svc.Unify(ctx, workID)
				done <- err
			}()
			// Wait until Unify is blocked on a row the write holds.
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting int
				if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("unify didn't wait for the write: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("unify never waited")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			var chs, refs int
			if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM chapters WHERE id = $1`, chapterID).Scan(&chs); err != nil {
				t.Fatal(err)
			}
			if err := st.Pool.QueryRow(ctx, c.count, chapterID).Scan(&refs); err != nil {
				t.Fatal(err)
			}
			if chs != 1 || refs != 1 {
				t.Fatalf("chapter %d, %s %d: the committed write was lost", chs, c.name, refs)
			}

			// With nothing pointing at it any more, the ghost goes.
			if _, err := st.Pool.Exec(ctx, `DELETE FROM progress; DELETE FROM chapter_files`); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Unify(ctx, workID); err != nil {
				t.Fatal(err)
			}
			if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM chapters WHERE id = $1`, chapterID).Scan(&chs); err != nil {
				t.Fatal(err)
			}
			if chs != 0 {
				t.Fatal("unreferenced ghost kept")
			}
		})
	}
}
