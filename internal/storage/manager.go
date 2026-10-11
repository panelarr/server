// SPDX-License-Identifier: GPL-3.0-or-later

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Kinds of storage.
const (
	KindLocal = "local"
	KindS3    = "s3"
)

// Errors callers map to responses.
var (
	ErrUnknown    = errors.New("storage: no such storage")
	ErrIDTaken    = errors.New("storage: a storage with this ID exists")
	ErrInvalidID  = errors.New("storage: the ID must be lowercase letters, digits, '-' or '_' (up to 40)")
	ErrInUse      = errors.New("storage: files are stored here; move them first")
	ErrIsDefault  = errors.New("storage: the default storage can't be removed; make another the default first")
	ErrBadConfig  = errors.New("storage: invalid settings")
	ErrNoDefault  = errors.New("storage: no default storage")
	ErrRootNeeded = errors.New("storage: a local storage needs an absolute root directory")
)

// LocalConfig is a local storage's settings.
type LocalConfig struct {
	Root string `json:"root"`
}

// Config is a storage's settings: Local for local, S3 for S3.
type Config struct {
	Local *LocalConfig
	S3    *S3Config
}

// Manager keeps the configured storages.
type Manager struct {
	st  *store.Store
	box *secret.Box
	log *slog.Logger

	mu       sync.Mutex
	backends map[string]Storage
}

// NewManager returns the storage manager.
func NewManager(st *store.Store, box *secret.Box, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{st: st, box: box, log: log, backends: map[string]Storage{}}
}

// EnsureDefault creates a local storage at root as the default when there
// is no storage yet.
func (m *Manager) EnsureDefault(ctx context.Context, root string) error {
	rows, err := m.st.ListStorages(ctx)
	if err != nil || len(rows) > 0 {
		return err
	}
	cfg, _ := json.Marshal(LocalConfig{Root: root})
	_, err = m.st.CreateStorage(ctx, db.CreateStorageParams{ID: "local", Name: "Local library", Kind: KindLocal, Config: cfg, IsDefault: true})
	if err == nil {
		m.log.InfoContext(ctx, "default storage created", "root", root)
	}
	return err
}

// List returns every storage.
func (m *Manager) List(ctx context.Context) ([]db.Storage, error) { return m.st.ListStorages(ctx) }

// Get returns a storage.
func (m *Manager) Get(ctx context.Context, id string) (db.Storage, error) {
	s, err := m.st.GetStorage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Storage{}, ErrUnknown
	}
	return s, err
}

// ConfigOf decodes a storage's settings (without the secret).
func ConfigOf(s db.Storage) (Config, error) {
	switch s.Kind {
	case KindLocal:
		var c LocalConfig
		err := json.Unmarshal(s.Config, &c)
		return Config{Local: &c}, err
	case KindS3:
		var c S3Config
		err := json.Unmarshal(s.Config, &c)
		return Config{S3: &c}, err
	}
	return Config{}, fmt.Errorf("storage: unknown kind %q", s.Kind)
}

func build(kind string, cfg Config, secret string) (Storage, error) {
	switch kind {
	case KindLocal:
		if cfg.Local == nil || !filepath.IsAbs(cfg.Local.Root) {
			return nil, ErrRootNeeded
		}
		return &Local{Root: cfg.Local.Root}, nil
	case KindS3:
		if cfg.S3 == nil {
			return nil, ErrBadConfig
		}
		s, err := NewS3(*cfg.S3, secret)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadConfig, err)
		}
		return s, nil
	}
	return nil, fmt.Errorf("%w: unknown kind %q", ErrBadConfig, kind)
}

// Backend returns the live backend of a storage.
func (m *Manager) Backend(ctx context.Context, id string) (Storage, error) {
	m.mu.Lock()
	b, ok := m.backends[id]
	m.mu.Unlock()
	if ok {
		return b, nil
	}
	row, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg, err := ConfigOf(row)
	if err != nil {
		return nil, err
	}
	sec := ""
	if row.Secret != "" {
		if sec, err = m.box.Open(row.Secret); err != nil {
			return nil, err
		}
	}
	b, err = build(row.Kind, cfg, sec)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.backends[id] = b
	m.mu.Unlock()
	return b, nil
}

// Default returns the default storage and its backend.
func (m *Manager) Default(ctx context.Context) (db.Storage, Storage, error) {
	row, err := m.st.GetDefaultStorage(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Storage{}, nil, ErrNoDefault
	}
	if err != nil {
		return db.Storage{}, nil, err
	}
	b, err := m.Backend(ctx, row.ID)
	return row, b, err
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// NewStorage is a storage to create.
type NewStorage struct {
	ID, Name, Kind string
	Config         Config
	// Secret is the S3 secret access key.
	Secret string
}

// Create checks a storage works (write, read, delete) and saves it.
func (m *Manager) Create(ctx context.Context, n NewStorage) (db.Storage, error) {
	if !validID.MatchString(n.ID) {
		return db.Storage{}, ErrInvalidID
	}
	b, err := build(n.Kind, n.Config, n.Secret)
	if err != nil {
		return db.Storage{}, err
	}
	if err := b.Test(ctx); err != nil {
		return db.Storage{}, err
	}
	cfg, err := marshalConfig(n.Kind, n.Config)
	if err != nil {
		return db.Storage{}, err
	}
	sealed := ""
	if n.Secret != "" {
		if sealed, err = m.box.Seal(n.Secret); err != nil {
			return db.Storage{}, err
		}
	}
	row, err := m.st.CreateStorage(ctx, db.CreateStorageParams{ID: n.ID, Name: n.Name, Kind: n.Kind, Config: cfg, Secret: sealed})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return db.Storage{}, ErrIDTaken
	}
	if err != nil {
		return db.Storage{}, err
	}
	m.log.InfoContext(ctx, "storage created", "component", "audit", "storage", row.ID, "kind", row.Kind)
	return row, nil
}

func marshalConfig(kind string, c Config) ([]byte, error) {
	switch kind {
	case KindLocal:
		return json.Marshal(c.Local)
	case KindS3:
		return json.Marshal(c.S3) //nolint:gosec // the access key ID isn't secret; the secret key is stored apart, encrypted
	}
	return nil, ErrBadConfig
}

// Update changes a storage's name and settings (checked first). An empty
// secret keeps the stored one.
func (m *Manager) Update(ctx context.Context, id, name string, cfg Config, secret string) (db.Storage, error) {
	row, err := m.Get(ctx, id)
	if err != nil {
		return db.Storage{}, err
	}
	sec := ""
	if row.Secret != "" {
		if sec, err = m.box.Open(row.Secret); err != nil {
			return db.Storage{}, err
		}
	}
	if secret != "" {
		sec = secret
	}
	b, err := build(row.Kind, cfg, sec)
	if err != nil {
		return db.Storage{}, err
	}
	if err := b.Test(ctx); err != nil {
		return db.Storage{}, err
	}
	raw, err := marshalConfig(row.Kind, cfg)
	if err != nil {
		return db.Storage{}, err
	}
	sealed := ""
	if sec != "" {
		if sealed, err = m.box.Seal(sec); err != nil {
			return db.Storage{}, err
		}
	}
	if name == "" {
		name = row.Name
	}
	out, err := m.st.UpdateStorage(ctx, db.UpdateStorageParams{ID: id, Name: name, Config: raw, Secret: sealed})
	if err != nil {
		return db.Storage{}, err
	}
	m.mu.Lock()
	m.backends[id] = b
	m.mu.Unlock()
	m.log.InfoContext(ctx, "storage changed", "component", "audit", "storage", id)
	return out, nil
}

// SetDefault makes a storage the default for new downloads.
func (m *Manager) SetDefault(ctx context.Context, id string) error {
	err := m.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.ClearDefaultStorage(ctx); err != nil {
			return err
		}
		n, err := q.SetDefaultStorage(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrUnknown
		}
		return nil
	})
	if err == nil {
		m.log.InfoContext(ctx, "default storage changed", "component", "audit", "storage", id)
	}
	return err
}

// Delete removes an empty storage that isn't the default.
func (m *Manager) Delete(ctx context.Context, id string) error {
	row, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.IsDefault {
		return ErrIsDefault
	}
	n, err := m.st.CountFilesByStorage(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	if _, err := m.st.DeleteStorage(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.backends, id)
	m.mu.Unlock()
	m.log.InfoContext(ctx, "storage removed", "component", "audit", "storage", id)
	return nil
}

// Test checks a configured storage now.
func (m *Manager) Test(ctx context.Context, id string) error {
	b, err := m.Backend(ctx, id)
	if err != nil {
		return err
	}
	return b.Test(ctx)
}
