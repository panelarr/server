// SPDX-License-Identifier: GPL-3.0-or-later

// Package store is Panelarr's PostgreSQL data layer: a pgx pool, the
// embedded goose migrations and the sqlc queries in package db.
//
// Panelarr keeps everything inside its own database. It never changes
// settings of the PostgreSQL server and needs no superuser: the role only
// has to own the database (or have CREATE on it).
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/panelarr/server/internal/store/db"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the connection pool and the generated queries.
type Store struct {
	Pool *pgxpool.Pool
	*db.Queries
}

// Open connects to the database at dsn (a PostgreSQL URL or key=value
// string) and checks the connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse database URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "panelarr"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	return &Store{Pool: pool, Queries: db.New(pool)}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

// InTx runs f in a transaction and commits it if f returns nil.
func (s *Store) InTx(ctx context.Context, f func(q *db.Queries, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return f(db.New(tx), tx)
	})
}

func migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

func (s *Store) provider() (*goose.Provider, func() error, error) {
	sqlDB := stdlib.OpenDBFromPool(s.Pool)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations(),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return p, sqlDB.Close, nil
}

// MigrationResult describes one applied migration.
type MigrationResult struct {
	Version   int64
	Name      string
	Direction string
}

// Migrate applies all pending migrations. A Postgres advisory lock keeps
// several server instances from migrating at once.
func (s *Store) Migrate(ctx context.Context) ([]MigrationResult, error) {
	p, closeDB, err := s.provider()
	if err != nil {
		return nil, fmt.Errorf("store: migrations: %w", err)
	}
	defer func() { _ = closeDB() }()
	res, err := p.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return results(res), nil
}

// MigrateDown rolls back the latest migration.
func (s *Store) MigrateDown(ctx context.Context) ([]MigrationResult, error) {
	p, closeDB, err := s.provider()
	if err != nil {
		return nil, fmt.Errorf("store: migrations: %w", err)
	}
	defer func() { _ = closeDB() }()
	r, err := p.Down(ctx)
	if err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: migrate down: %w", err)
	}
	return results([]*goose.MigrationResult{r}), nil
}

// MigrationStatus lists every migration and whether it is applied.
type MigrationStatus struct {
	Version int64
	Name    string
	Applied bool
}

// Status reports which migrations are applied.
func (s *Store) Status(ctx context.Context) ([]MigrationStatus, error) {
	p, closeDB, err := s.provider()
	if err != nil {
		return nil, fmt.Errorf("store: migrations: %w", err)
	}
	defer func() { _ = closeDB() }()
	st, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migration status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, m := range st {
		out = append(out, MigrationStatus{
			Version: m.Source.Version,
			Name:    m.Source.Path,
			Applied: m.State == goose.StateApplied,
		})
	}
	return out, nil
}

func results(rs []*goose.MigrationResult) []MigrationResult {
	out := make([]MigrationResult, 0, len(rs))
	for _, r := range rs {
		if r == nil || r.Source == nil {
			continue
		}
		out = append(out, MigrationResult{Version: r.Source.Version, Name: r.Source.Path, Direction: r.Direction})
	}
	return out
}
