// SPDX-License-Identifier: GPL-3.0-or-later

// Package storetest gives tests a fresh, migrated PostgreSQL database.
//
// It uses the server at PANELARR_TEST_DATABASE_URL when set, otherwise it
// starts a postgres container with testcontainers (once per test binary).
// Each call creates its own database and drops it when the test ends, so
// tests can run in parallel. The role only needs CREATEDB.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/panelarr/server/internal/store"
)

// Image is the PostgreSQL image used when no server is configured.
const Image = "postgres:18-alpine"

var (
	once     sync.Once
	adminURL string
	onceErr  error
)

// serverURL returns a URL for a role that may create databases.
func serverURL(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("needs PostgreSQL")
	}
	once.Do(func() {
		if u := os.Getenv("PANELARR_TEST_DATABASE_URL"); u != "" {
			adminURL = u
			return
		}
		adminURL, onceErr = startContainer()
	})
	if onceErr != nil {
		t.Fatalf("storetest: %v (set PANELARR_TEST_DATABASE_URL to use a running server)", onceErr)
	}
	return adminURL
}

func startContainer() (string, error) {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithLogger(nopLogger{}),
	)
	if err != nil {
		return "", err
	}
	super, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", err
	}
	// Tests run as an ordinary role, like the server in production.
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `CREATE ROLE panelarr LOGIN CREATEDB PASSWORD 'panelarr'`); err != nil {
		return "", err
	}
	u, err := url.Parse(super)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("panelarr", "panelarr")
	return u.String(), nil
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// URL creates an empty database and returns its URL. The database is
// dropped when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	base := serverURL(t)
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "panelarr_test_" + hex.EncodeToString(b)

	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("storetest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Logf("storetest: drop %s: %v", name, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("storetest: drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// New returns a store on a fresh, migrated database.
func New(t testing.TB) *store.Store {
	t.Helper()
	s := Open(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("storetest: %v", err)
	}
	return s
}

// Open returns a store on a fresh database without running migrations.
func Open(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), URL(t))
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
