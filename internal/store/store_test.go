// SPDX-License-Identifier: GPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
)

func TestMigrateUpDownStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	res, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no migrations applied on an empty database")
	}
	again, err := s.Migrate(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second migrate: %v, %d applied", err, len(again))
	}
	st, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st {
		if !m.Applied {
			t.Fatalf("%s not applied", m.Name)
		}
	}
	// Every migration rolls back cleanly.
	for range st {
		if _, err := s.MigrateDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate after full rollback: %v", err)
	}
}

func TestSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.New(t)

	if _, err := s.GetSetting(ctx, "ui.theme"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing setting: %v", err)
	}
	for _, v := range []string{`"dark"`, `{"name":"light"}`} {
		if err := s.PutSetting(ctx, db.PutSettingParams{Key: "ui.theme", Value: []byte(v)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetSetting(ctx, "ui.theme")
	if err != nil || string(got) != `{"name": "light"}` {
		t.Fatalf("got %s, %v", got, err)
	}
	if err := s.PutSetting(ctx, db.PutSettingParams{Key: "Bad Key", Value: []byte(`1`)}); err == nil {
		t.Fatal("invalid key accepted")
	}
	err = s.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.DeleteSetting(ctx, "ui.theme"); err != nil {
			return err
		}
		return errors.New("roll back")
	})
	if err == nil {
		t.Fatal("InTx swallowed the error")
	}
	if _, err := s.GetSetting(ctx, "ui.theme"); err != nil {
		t.Fatalf("delete was not rolled back: %v", err)
	}
}
