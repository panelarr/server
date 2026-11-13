// SPDX-License-Identifier: GPL-3.0-or-later

package sso

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panelarr/server/internal/secret"
)

// discoveryServer serves a discovery document, failing the first
// failFirst requests.
func discoveryServer(t *testing.T, failFirst int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= failFirst || r.URL.Path != "/.well-known/openid-configuration" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testBox() *secret.Box {
	return secret.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
}

func newTestProvider(srv *httptest.Server, now *time.Time) *Provider {
	p := New(Config{Issuer: srv.URL, ClientID: "c", Scopes: []string{"openid"}, RedirectURL: "https://panelarr.example.com/cb"}, testBox(), srv.Client(), nil)
	p.now = func() time.Time { return *now }
	return p
}

func TestDiscoveryBacksOffThenCaches(t *testing.T) {
	srv, hits := discoveryServer(t, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newTestProvider(srv, &now)
	ctx := context.Background()
	if err := p.Check(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first check: %v", err)
	}
	// Within the backoff, nothing is fetched.
	if err := p.Check(ctx); !errors.Is(err, ErrUnavailable) || hits.Load() != 1 {
		t.Fatalf("second check: %v, %d fetches", err, hits.Load())
	}
	now = now.Add(retryMax)
	if err := p.Check(ctx); err != nil {
		t.Fatalf("after the backoff: %v", err)
	}
	// Kept once fetched.
	for range 3 {
		if err := p.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("%d fetches, want 2", hits.Load())
	}
}

func TestRunRetriesUntilDiscovered(t *testing.T) {
	srv, hits := discoveryServer(t, 0)
	now := time.Now()
	p := newTestProvider(srv, &now)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.Run(ctx) // returns once discovered
	if err := p.Check(ctx); err != nil || hits.Load() != 1 {
		t.Fatalf("%v, %d fetches", err, hits.Load())
	}
}

func TestFlowCookie(t *testing.T) {
	srv, _ := discoveryServer(t, 0)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newTestProvider(srv, &now)
	ctx := context.Background()
	authURL, cookie, err := p.Begin(ctx, "/library")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("auth URL: %s", authURL)
	}
	f, err := p.openFlow(cookie)
	if err != nil || f.ReturnTo != "/library" || f.State != q.Get("state") || f.Nonce != q.Get("nonce") {
		t.Fatalf("flow: %+v %v", f, err)
	}
	if f.Verifier == q.Get("code_challenge") {
		t.Fatal("the verifier went to the provider")
	}

	// A stale flow is refused, before anything else.
	now = now.Add(FlowTTL)
	if _, err := p.Finish(ctx, cookie, q.Get("state"), "code"); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired flow: %v", err)
	}

	// A value sealed with the same key for another purpose isn't a flow.
	other, err := testBox().Seal(`{"s":"a","n":"b","v":"c","r":"/","e":"2999-01-01T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.openFlow(other); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("stored secret as a flow: %v", err)
	}
}

func TestNonceSet(t *testing.T) {
	n := newNonceSet()
	t0 := time.Now()
	if !n.use("a", t0.Add(time.Minute), t0) || n.use("a", t0.Add(time.Minute), t0) {
		t.Fatal("a nonce was accepted twice")
	}
	// Forgotten once its flow expired (the flow is refused by then).
	n.use("b", t0.Add(time.Hour), t0.Add(2*time.Minute))
	if _, kept := n.seen["a"]; kept {
		t.Fatal("expired nonce kept")
	}
}

func TestListClaim(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want int
		ok   bool
	}{
		{[]any{"a", "b", 3}, 2, true},
		{"a", 1, true},
		{nil, 0, false},
	} {
		got, ok := listClaim(map[string]any{"groups": tc.v}, "groups")
		if len(got) != tc.want || ok != tc.ok {
			t.Fatalf("%v: %v %v", tc.v, got, ok)
		}
	}
}

// A provider that hangs holds up no one: callers waiting on discovery
// share one fetch and can give up on their own deadline, and the provider
// is never fetched twice at once.
func TestDiscoveryDoesNotBlockOnAHangingProvider(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	now := time.Now()
	p := newTestProvider(srv, &now)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		err := p.Check(ctx)
		cancel()
		if !errors.Is(err, ErrUnavailable) || time.Since(start) > 2*time.Second {
			t.Fatalf("check: %v after %s", err, time.Since(start))
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d fetches in flight, want 1", n)
	}
}
