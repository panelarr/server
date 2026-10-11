// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors callers map to responses.
var (
	ErrNotConfigured = errors.New("meta: not connected")
	ErrManagedByEnv  = errors.New("meta: the connection is set by PANELARR_META_URL and can't be changed here")
	ErrCheckFailed   = errors.New("meta: the metadata service can't be used with these settings")
	ErrInvalidURL    = errors.New("meta: the URL must be http(s)://host[:port]")
)

// settingConnection stores the connection, with the key encrypted.
const settingConnection = "meta.connection"

type storedConn struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"` // sealed
}

// scopeAdmin is the metadata service's admin scope. The server uses it for
// the setup checks and the admin pages it shows built in; it implies every
// other scope.
const scopeAdmin = "admin"

// Provider is one metadata provider plugin of the service.
type Provider struct {
	ID      string
	Health  string
	Message string
	Records int64
}

// Check is the result of checking the metadata service.
type Check struct {
	Configured bool
	Reachable  bool
	KeyValid   bool
	AdminScope bool
	Providers  []Provider
	// Ready: reachable, an admin key, and a provider with entries. Until
	// then the server is degraded and nothing can be added.
	Ready bool
	// Problem says what to do when not ready.
	Problem   string
	CheckedAt time.Time
}

// Status is the connection and its last check.
type Status struct {
	URL          string
	ManagedByEnv bool
	Check        Check
}

// checkTTL is how long a check result is reused (readiness probes run
// often).
const checkTTL = 30 * time.Second

// Service holds the connection to the metadata service.
type Service struct {
	st  *store.Store
	box *secret.Box
	log *slog.Logger
	hc  *http.Client
	env Conn
	now func() time.Time

	mu     sync.RWMutex
	conn   Conn
	client *Client
	last   Check

	covers coverIndex
}

// NewService returns the service. env, when its URL is set, is the
// connection and can't be changed in the app.
func NewService(st *store.Store, box *secret.Box, env Conn, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{st: st, box: box, env: env, log: log, hc: newHTTPClient(), now: time.Now}
}

// SetHTTPClient replaces the HTTP client (tests).
func (s *Service) SetHTTPClient(hc *http.Client) { s.hc = hc }

// Load reads the connection from the environment or the database.
func (s *Service) Load(ctx context.Context) error {
	if s.env.URL != "" {
		if err := validURL(s.env.URL); err != nil {
			return fmt.Errorf("PANELARR_META_URL: %w", err)
		}
		s.use(s.env)
		return nil
	}
	raw, err := s.st.GetSetting(ctx, settingConnection)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var sc storedConn
	if err := json.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("meta: setting %s: %w", settingConnection, err)
	}
	key, err := s.box.Open(sc.APIKey)
	if err != nil {
		return fmt.Errorf("meta: stored API key: %w", err)
	}
	s.use(Conn{URL: sc.URL, APIKey: key})
	return nil
}

func (s *Service) use(c Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = c
	s.client = NewClient(c, s.hc)
	s.last = Check{}
}

// Client returns the current client, or ErrNotConfigured.
func (s *Service) Client() (*Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return nil, ErrNotConfigured
	}
	return s.client, nil
}

// Status returns the connection and a check no older than checkTTL.
func (s *Service) Status(ctx context.Context) Status {
	s.mu.RLock()
	u := s.conn.URL
	s.mu.RUnlock()
	return Status{URL: u, ManagedByEnv: s.env.URL != "", Check: s.Check(ctx, false)}
}

// Check checks the current connection. Without fresh, a recent result is
// reused.
func (s *Service) Check(ctx context.Context, fresh bool) Check {
	s.mu.RLock()
	client, last := s.client, s.last
	s.mu.RUnlock()
	if client == nil {
		return Check{Problem: "Connect the metadata service: enter its URL and an API key with the admin scope.", CheckedAt: s.now()}
	}
	if !fresh && !last.CheckedAt.IsZero() && s.now().Sub(last.CheckedAt) < checkTTL {
		return last
	}
	c := s.check(ctx, client)
	s.mu.Lock()
	if s.client == client {
		s.last = c
	}
	s.mu.Unlock()
	return c
}

// ReadyCheck is the readiness check: it fails while the service isn't
// ready, which marks the server degraded.
func (s *Service) ReadyCheck(ctx context.Context) error {
	c := s.Check(ctx, false)
	if !c.Ready {
		return errors.New(c.Problem)
	}
	return nil
}

func (s *Service) check(ctx context.Context, client *Client) Check {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := Check{Configured: true, CheckedAt: s.now()}
	who, err := client.Clients.WhoAmI(ctx, connect.NewRequest(&metav1.WhoAmIRequest{}))
	switch code := connect.CodeOf(err); {
	case err == nil:
		c.Reachable, c.KeyValid = true, true
	case code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied:
		c.Reachable = true
		c.Problem = "The metadata service refused the API key. Create a client with the admin scope in its admin and use that key."
		return c
	default:
		c.Problem = "Can't reach the metadata service: " + errorMessage(err) + ". Check the URL and that the service is running."
		return c
	}
	if !slices.Contains(who.Msg.GetScopes(), scopeAdmin) {
		c.Problem = "The API key works but lacks the admin scope. Give its client the admin scope in the metadata service's admin."
		return c
	}
	c.AdminScope = true
	provs, err := client.Admin.ListProviders(ctx, connect.NewRequest(&metav1.ListProvidersRequest{}))
	if err != nil {
		c.Problem = "Can't list the metadata providers: " + errorMessage(err) + "."
		return c
	}
	var records int64
	for _, p := range provs.Msg.GetProviders() {
		c.Providers = append(c.Providers, Provider{
			ID: p.GetId(), Health: healthName(p.GetHealth().String()), Message: p.GetHealthMessage(), Records: p.GetRecordCount(),
		})
		records += p.GetRecordCount()
	}
	switch {
	case len(c.Providers) == 0:
		c.Problem = "The metadata service has no metadata provider yet. Add a provider plugin to it, then run its first sync."
	case records == 0:
		c.Problem = "No metadata provider has entries yet. Run a sync of a provider; a first full import can take a while."
	default:
		c.Ready = true
	}
	return c
}

// Save tests conn and, when the service is reachable and the key has the
// admin scope, stores it (the key encrypted) and switches to it. A service
// without provider data is saved too: the check says what's missing.
func (s *Service) Save(ctx context.Context, conn Conn) (Check, error) {
	if s.env.URL != "" {
		return Check{}, ErrManagedByEnv
	}
	conn.URL = strings.TrimRight(strings.TrimSpace(conn.URL), "/")
	if err := validURL(conn.URL); err != nil {
		return Check{}, err
	}
	if conn.APIKey == "" {
		// Keep the stored key when only the URL changes.
		s.mu.RLock()
		conn.APIKey = s.conn.APIKey
		s.mu.RUnlock()
	}
	c := s.check(ctx, NewClient(conn, s.hc))
	if !c.AdminScope {
		return c, ErrCheckFailed
	}
	sealed, err := s.box.Seal(conn.APIKey)
	if err != nil {
		return c, err
	}
	raw, err := json.Marshal(storedConn{URL: conn.URL, APIKey: sealed}) //nolint:gosec // the key is sealed (encrypted) here
	if err != nil {
		return c, err
	}
	if err := s.st.PutSetting(ctx, db.PutSettingParams{Key: settingConnection, Value: raw}); err != nil {
		return c, err
	}
	s.use(conn)
	s.mu.Lock()
	s.last = c
	s.mu.Unlock()
	s.log.InfoContext(ctx, "metadata service connected", "component", "audit", "url", conn.URL, "ready", c.Ready)
	return c, nil
}

// RunSync starts a sync of a provider in the metadata service and returns
// the run's ID.
func (s *Service) RunSync(ctx context.Context, provider, kind string) (string, error) {
	client, err := s.Client()
	if err != nil {
		return "", err
	}
	res, err := client.Admin.RunSync(ctx, connect.NewRequest(&metav1.RunSyncRequest{Provider: provider, Kind: kind}))
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.last = Check{} // the next check sees the new state
	s.mu.Unlock()
	return res.Msg.GetRun().GetId(), nil
}

func validURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidURL
	}
	return nil
}

// errorMessage shortens a connect error for people.
func errorMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		if ce.Message() != "" {
			return ce.Message()
		}
		return ce.Code().String()
	}
	return err.Error()
}

// healthName turns "HEALTH_STATUS_OK" into "ok".
func healthName(s string) string {
	s = strings.TrimPrefix(s, "HEALTH_STATUS_")
	if s == "UNSPECIFIED" {
		return "unknown"
	}
	return strings.ToLower(s)
}
