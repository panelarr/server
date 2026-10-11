// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/storetest"
	"github.com/panelarr/server/internal/users"
)

// fastArgon2 keeps password hashing cheap in tests.
var fastArgon2 = auth.Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

type harness struct {
	t       *testing.T
	st      *store.Store
	auth    *auth.Service
	users   *users.Service
	meta    *meta.Service
	metrics *metrics.Metrics
	srv     *httptest.Server
}

type harnessOpt func(*Deps)

func withChecks(c ...Check) harnessOpt { return func(d *Deps) { d.Checks = c } }

func withMetricsOnAPI() harnessOpt {
	return func(d *Deps) { d.Metrics = metrics.New(); d.MetricsOnAPI = true }
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	st := storetest.New(t)
	a := auth.NewService(st, auth.NewHasher(fastArgon2, 4), nil)
	u := users.NewService(st, a, nil)
	m := meta.NewService(st, secret.NewBox("", filepath.Join(t.TempDir(), "secret.key")), meta.Conn{}, nil)
	deps := Deps{
		Log:    slog.New(slog.DiscardHandler),
		Auth:   a,
		Users:  u,
		Meta:   m,
		Images: imagecache.New(t.TempDir()),
		Checks: []Check{
			{Name: "database", Required: true, Run: st.Pool.Ping},
			{Name: "meta", Run: m.ReadyCheck},
		},
		// The test client connects from loopback; trust nothing, so the
		// peer address is the client address.
		TrustedProxies: nil,
	}
	for _, o := range opts {
		o(&deps)
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &harness{t: t, st: st, auth: a, users: u, meta: m, metrics: deps.Metrics, srv: srv}
}

// client is an API client that keeps cookies, sends the CSRF token it was
// given, and optionally an API key.
type client struct {
	h      *harness
	http   *http.Client
	csrf   string
	apiKey string
	header http.Header
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar}, header: http.Header{}}
}

func (h *harness) keyClient(key string) *client {
	c := h.client()
	c.apiKey = key
	return c
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) problem(t *testing.T) apigen.Problem {
	t.Helper()
	var p apigen.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("not a problem (%d): %s", r.status, r.body)
	}
	return p
}

func (c *client) do(method, path string, body any) response {
	c.h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.h.srv.URL+path, rd)
	if err != nil {
		c.h.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(CSRFHeader, c.csrf)
	}
	if c.apiKey != "" {
		req.Header.Set(APIKeyHeader, c.apiKey)
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: b}
}

// json sends a request, checks the status and decodes the body into T.
func call[T any](c *client, method, path string, body any, want int) T {
	c.h.t.Helper()
	res := c.do(method, path, body)
	if res.status != want {
		c.h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.status, want, res.body)
	}
	var v T
	if len(res.body) > 0 {
		if err := json.Unmarshal(res.body, &v); err != nil {
			c.h.t.Fatalf("%s %s: decode: %v: %s", method, path, err, res.body)
		}
	}
	return v
}

// expectProblem checks a request fails with status and code.
func (c *client) expectProblem(method, path string, body any, status int, code string) apigen.Problem {
	c.h.t.Helper()
	res := c.do(method, path, body)
	if res.status != status {
		c.h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.status, status, res.body)
	}
	p := res.problem(c.h.t)
	if p.Code != code {
		c.h.t.Fatalf("%s %s: code %q, want %q: %s", method, path, p.Code, code, res.body)
	}
	return p
}

const adminPassword = "correct horse battery"

// setupAdmin creates the first admin and returns a signed-in client.
func (h *harness) setupAdmin() (*client, apigen.User) {
	h.t.Helper()
	c := h.client()
	res := call[apigen.LoginResponse](c, http.MethodPost, "/api/v1/setup/admin",
		map[string]string{"username": "Admin", "password": adminPassword}, http.StatusCreated)
	c.csrf = res.CsrfToken
	return c, res.User
}

// createUser creates a user through the API and signs it in.
func (h *harness) createUser(admin *client, username string, grants map[string]any) (*client, apigen.User) {
	h.t.Helper()
	body := map[string]any{"username": username, "password": "user password 1"}
	if grants != nil {
		body["grants"] = grants
	}
	u := call[apigen.User](admin, http.MethodPost, "/api/v1/users", body, http.StatusCreated)
	return h.login(username, "user password 1"), u
}

func (h *harness) login(username, password string) *client {
	h.t.Helper()
	c := h.client()
	res := call[apigen.LoginResponse](c, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": username, "password": password}, http.StatusOK)
	c.csrf = res.CsrfToken
	return c
}

// apiKey creates a key with scopes for the signed-in client.
func (c *client) newAPIKey(scopes ...string) string {
	c.h.t.Helper()
	res := call[apigen.ApiKeyCreated](c, http.MethodPost, "/api/v1/me/api-keys",
		map[string]any{"name": "test", "scopes": scopes}, http.StatusCreated)
	return res.Key
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
