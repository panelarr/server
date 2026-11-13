// SPDX-License-Identifier: GPL-3.0-or-later

package store_test

import (
	"context"
	"testing"

	"github.com/panelarr/server/internal/store/storetest"
)

// TestCompatIDsMigrationKeepsRows applies the compat IDs migration
// (00020) over a database that already holds sources and series, as an
// existing install does.
func TestCompatIDsMigrationKeepsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Back to just before 00020 (and any later migration).
	downTo := func(below int64) {
		t.Helper()
		for {
			st, err := s.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var top int64
			for _, m := range st {
				if m.Applied {
					top = max(top, m.Version)
				}
			}
			if top < below {
				return
			}
			if _, err := s.MigrateDown(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	downTo(20)
	for _, q := range []string{
		`INSERT INTO plugins (id, url) VALUES ('p', 'http://plugin.example')`,
		`INSERT INTO sources (id, plugin_id, source_id, name) VALUES ('p:1', 'p', '1', 'One')`,
		`INSERT INTO works (meta_entry_id, title, sort_title) VALUES ('e1', 'W', 'w')`,
		`INSERT INTO source_series (work_id, source_id, key, title, state, origin)
		 SELECT id, 'p:1', '/s/1', 'W', 'attached', 'manual' FROM works`,
	} {
		if _, err := s.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE source_series SET origin = 'import'`); err == nil {
		t.Fatal("origin import accepted before the migration")
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	var origin string
	if err := s.Pool.QueryRow(ctx, `SELECT compat_ids FROM sources WHERE id = 'p:1'`).Scan(&ids); err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("existing source's compat IDs: %v %#v", err, ids)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT origin FROM source_series`).Scan(&origin); err != nil || origin != "manual" {
		t.Fatalf("existing series: %v %q", err, origin)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE source_series SET origin = 'import'`); err != nil {
		t.Fatalf("origin import: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE source_series SET origin = 'other'`); err == nil {
		t.Fatal("unknown origin accepted")
	}
	// Rolling back keeps the series, as added by hand.
	downTo(20)
	if err := s.Pool.QueryRow(ctx, `SELECT origin FROM source_series`).Scan(&origin); err != nil || origin != "manual" {
		t.Fatalf("after rollback: %v %q", err, origin)
	}
}
