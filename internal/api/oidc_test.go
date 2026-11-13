// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/sso"
	"github.com/panelarr/server/internal/store/db"
)

// withSSO turns single sign-on on with the mock provider.
func withSSO(m *mockOP, allowedGroups ...string) harnessOpt {
	return withSSOConfig(m, func(c *sso.Config) { c.AllowedGroups = allowedGroups })
}

// withSSOConfig turns single sign-on on with the mock provider and the
// settings change makes.
func withSSOConfig(m *mockOP, change func(*sso.Config)) harnessOpt {
	return func(d *Deps) {
		box := secret.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
		c := sso.Config{
			Issuer: m.issuer, ClientID: mockClientID, ClientSecret: mockClientSecret,
			Scopes: []string{"openid", "profile", "email"}, RedirectURL: mockRedirectURI,
			Label: "Sign in with Example ID", PostLogoutRedirectURL: "https://panelarr.example.com/login",
		}
		if change != nil {
			change(&c)
		}
		d.SSO = sso.New(c, box, m.srv.Client(), nil)
		d.Auth.SetPasswordAdminsOnly(c.PasswordAdminsOnly)
	}
}

// browser is a client that doesn't follow redirects, like a test that
// looks at each hop.
func (h *harness) browser() *client {
	c := h.client()
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// link links the mock provider's subject to a user.
func (h *harness) link(m *mockOP, userID uuid.UUID, subject string) {
	h.t.Helper()
	if _, err := h.st.CreateUserIdentity(context.Background(), db.CreateUserIdentityParams{
		UserID: userID, Issuer: m.issuer, Subject: subject,
	}); err != nil {
		h.t.Fatal(err)
	}
}

// start begins a sign-in and returns the provider URL.
func (c *client) startSSO(returnTo string) string {
	c.h.t.Helper()
	path := "/api/v1/auth/oidc/start"
	if returnTo != "" {
		path += "?return_to=" + url.QueryEscape(returnTo)
	}
	res := c.do(http.MethodGet, path, nil)
	if res.status != http.StatusFound {
		c.h.t.Fatalf("start: status %d: %s", res.status, res.body)
	}
	return res.header.Get("Location")
}

// ssoLogin runs a whole sign-in as claims and returns the callback's
// response.
func (c *client) ssoLogin(m *mockOP, claims mockClaims, returnTo string) response {
	c.h.t.Helper()
	m.next = claims
	return c.do(http.MethodGet, m.authorize(c.startSSO(returnTo)), nil)
}

// expectSSOError checks a callback refused with code and started no
// session.
func (c *client) expectSSOError(res response, code string) {
	c.h.t.Helper()
	if res.status != http.StatusFound || res.header.Get("Location") != "/login?sso_error="+code {
		c.h.t.Fatalf("callback: status %d, location %q, want the %s error page: %s", res.status, res.header.Get("Location"), code, res.body)
	}
	for _, ck := range (&http.Response{Header: res.header}).Cookies() {
		if ck.Name == SessionCookie && ck.MaxAge >= 0 {
			c.h.t.Fatalf("a refused callback set a session cookie")
		}
	}
	c.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")
}

func (c *client) cookie(name string) *http.Cookie {
	u, _ := url.Parse(c.h.srv.URL + oidcCookiePath + "callback")
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// AC1: without configuration nothing changes.
func TestOIDCOffByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin() // setup works as before
	anon := h.browser()
	methods := call[apigen.AuthMethods](anon, http.MethodGet, "/api/v1/auth/methods", nil, http.StatusOK)
	if methods.Password != apigen.AuthMethodsPasswordAll || methods.Oidc != nil {
		t.Fatalf("methods: %+v", methods)
	}
	anon.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start", nil, http.StatusNotFound, CodeNotFound)
	anon.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?return_to=/library", nil, http.StatusNotFound, CodeNotFound)
	anon.expectProblem(http.MethodGet, "/api/v1/auth/oidc/callback?code=x&state=y", nil, http.StatusNotFound, CodeNotFound)
	// Signed in, too.
	admin.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start", nil, http.StatusNotFound, CodeNotFound)
	admin.expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", nil, http.StatusNotFound, CodeNotFound)
	admin.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link&csrf_token="+url.QueryEscape(admin.csrf), nil, http.StatusNotFound, CodeNotFound)
	if anon.cookie(OIDCFlowCookie) != nil {
		t.Fatal("a flow cookie was set with single sign-on off")
	}
	// Password login works as before.
	h.createUser(admin, "reader", nil)
	h.login("reader", "user password 1")
	// The health checks are unchanged.
	health := call[apigen.SystemHealth](admin, http.MethodGet, "/api/v1/system/health", nil, http.StatusOK)
	for _, c := range health.Components {
		if c.Name == "oidc" {
			t.Fatal("oidc health check with single sign-on off")
		}
	}
}

// AC2: a linked user signs in and lands where they started, with a session
// like a password login's.
func TestOIDCLoginLinkedUser(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")

	b := h.browser()
	methods := call[apigen.AuthMethods](b, http.MethodGet, "/api/v1/auth/methods", nil, http.StatusOK)
	if methods.Oidc == nil || methods.Oidc.Label != "Sign in with Example ID" || methods.Password != apigen.AuthMethodsPasswordAll {
		t.Fatalf("methods: %+v", methods)
	}

	authURL := b.startSSO("/library/abc?sort=new#top")
	flow := b.cookie(OIDCFlowCookie)
	if flow == nil {
		t.Fatal("no flow cookie")
	}
	// The flow's secrets are sealed, not readable in the cookie.
	q := mustURL(t, authURL).Query()
	for _, v := range []string{q.Get("state"), q.Get("nonce")} {
		if strings.Contains(flow.Value, v) {
			t.Fatal("the flow cookie shows the state or nonce")
		}
	}
	m.next = mockClaims{Subject: "subject-reader", Email: "reader@example.com"}
	res := b.do(http.MethodGet, m.authorize(authURL), nil)
	if res.status != http.StatusFound || res.header.Get("Location") != "/library/abc?sort=new#top" {
		t.Fatalf("callback: %d %q %s", res.status, res.header.Get("Location"), res.body)
	}
	// The flow cookie is used up.
	if b.cookie(OIDCFlowCookie) != nil {
		t.Fatal("flow cookie kept after the callback")
	}
	csrf := b.cookie(CSRFCookie)
	if csrf == nil || b.cookie(SessionCookie) == nil {
		t.Fatal("no session cookies")
	}
	for _, ck := range (&http.Response{Header: res.header}).Cookies() {
		if ck.Name == SessionCookie && (!ck.HttpOnly || ck.Path != "/" || ck.SameSite != http.SameSiteLaxMode) {
			t.Fatalf("session cookie: %+v", ck)
		}
	}
	me := call[apigen.Me](b, http.MethodGet, "/api/v1/me", nil, http.StatusOK)
	if me.User.Id != reader.Id {
		t.Fatalf("signed in as %s, want %s", me.User.Id, reader.Id)
	}
	// Changing requests need the CSRF token, as after a password login.
	b.expectProblem(http.MethodPost, "/api/v1/auth/logout", nil, http.StatusForbidden, "csrf_failed")

	ctx := context.Background()
	var oidcSessions int
	if err := h.st.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND auth_method = 'oidc'`, reader.Id).Scan(&oidcSessions); err != nil || oidcSessions != 1 {
		t.Fatalf("oidc sessions: %d, %v", oidcSessions, err)
	}
	// Password sessions are marked too.
	var pw int
	if err := h.st.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE auth_method = 'password'`).Scan(&pw); err != nil || pw == 0 {
		t.Fatalf("password sessions: %d, %v", pw, err)
	}
	var email string
	var last *time.Time
	if err := h.st.Pool.QueryRow(ctx, `SELECT email, last_login_at FROM user_identities WHERE user_id = $1`, reader.Id).Scan(&email, &last); err != nil || email != "reader@example.com" || last == nil {
		t.Fatalf("identity: %q %v %v", email, last, err)
	}
	b.csrf = csrf.Value
	b.do(http.MethodPost, "/api/v1/auth/logout", nil)
	b.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")

	// Without return_to, home.
	res = h.browser().ssoLogin(m, mockClaims{Subject: "subject-reader"}, "")
	if res.status != http.StatusFound || res.header.Get("Location") != "/" {
		t.Fatalf("callback: %d %q", res.status, res.header.Get("Location"))
	}
}

func TestOIDCFlowCookie(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	h.setupAdmin()
	b := h.browser()
	res := b.do(http.MethodGet, "/api/v1/auth/oidc/start", nil)
	if res.status != http.StatusFound || res.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("start: %d %v", res.status, res.header)
	}
	var flow *http.Cookie
	for _, ck := range (&http.Response{Header: res.header}).Cookies() {
		if ck.Name == OIDCFlowCookie {
			flow = ck
		}
	}
	if flow == nil || !flow.HttpOnly || flow.Path != "/api/v1/auth/oidc/" || flow.MaxAge != 600 || flow.SameSite != http.SameSiteLaxMode {
		t.Fatalf("flow cookie: %+v", flow)
	}
}

// AC3: a callback that isn't exactly right starts no session.
func TestOIDCCallbackRefusals(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")
	ok := mockClaims{Subject: "subject-reader"}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		claims func(mockClaims) mockClaims
		code   string
	}{
		{"expired token", func(c mockClaims) mockClaims { c.Expiry = -5 * time.Minute; return c }, "invalid_token"},
		{"another audience", func(c mockClaims) mockClaims { c.Audience = "someone-else"; return c }, "invalid_token"},
		{"another issuer", func(c mockClaims) mockClaims { c.Issuer = "https://evil.example.com"; return c }, "invalid_token"},
		{"another nonce", func(c mockClaims) mockClaims { c.Nonce = "not-the-flows-nonce"; return c }, "invalid_token"},
		{"another key", func(c mockClaims) mockClaims { c.SignWith = otherKey; return c }, "invalid_token"},
		// Several audiences need azp naming us (OpenID Connect Core
		// 3.1.3.7); an azp naming someone else is refused.
		{"several audiences, azp another client", func(c mockClaims) mockClaims {
			c.Audiences, c.AZP = []string{"other-client", mockClientID}, "other-client"
			return c
		}, "invalid_token"},
		{"several audiences, no azp", func(c mockClaims) mockClaims { c.Audiences = []string{"other-client", mockClientID}; return c }, "invalid_token"},
		{"azp another client", func(c mockClaims) mockClaims { c.AZP = "other-client"; return c }, "invalid_token"},
		{"not linked", func(c mockClaims) mockClaims { c.Subject = "subject-stranger"; return c }, "not_linked"},
		// The same email as a linked user doesn't make a stranger that
		// user: only issuer and subject count.
		{"same email", func(c mockClaims) mockClaims { c.Subject, c.Email = "subject-stranger", "reader@example.com"; return c }, "not_linked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := h.browser()
			b.expectSSOError(b.ssoLogin(m, tc.claims(ok), "/library"), tc.code)
		})
	}

	t.Run("expiry within the clock skew", func(t *testing.T) {
		b := h.browser()
		c := ok
		c.Expiry = -time.Minute
		if res := b.ssoLogin(m, c, "/x"); res.header.Get("Location") != "/x" {
			t.Fatalf("callback: %d %q", res.status, res.header.Get("Location"))
		}
	})

	for name, c := range map[string]mockClaims{
		"several audiences, azp us": {Subject: "subject-reader", Audiences: []string{"other-client", mockClientID}, AZP: mockClientID},
		"azp us":                    {Subject: "subject-reader", AZP: mockClientID},
	} {
		t.Run(name, func(t *testing.T) {
			if res := h.browser().ssoLogin(m, c, "/ok"); res.header.Get("Location") != "/ok" {
				t.Fatalf("callback: %d %q", res.status, res.header.Get("Location"))
			}
		})
	}

	t.Run("state mismatch", func(t *testing.T) {
		b := h.browser()
		m.next = ok
		cb := mustURL(t, m.authorize(b.startSSO("")))
		q := cb.Query()
		q.Set("state", "attacker-state")
		b.expectSSOError(b.do(http.MethodGet, cb.Path+"?"+q.Encode(), nil), "state_mismatch")
	})

	t.Run("no state", func(t *testing.T) {
		b := h.browser()
		m.next = ok
		cb := mustURL(t, m.authorize(b.startSSO("")))
		q := cb.Query()
		q.Del("state")
		b.expectSSOError(b.do(http.MethodGet, cb.Path+"?"+q.Encode(), nil), "state_mismatch")
	})

	t.Run("no flow cookie", func(t *testing.T) {
		// Login CSRF: an attacker's own callback in a victim's browser.
		attacker := h.browser()
		m.next = ok
		cb := m.authorize(attacker.startSSO(""))
		victim := h.browser()
		victim.expectSSOError(victim.do(http.MethodGet, cb, nil), "flow_expired")
	})

	t.Run("forged flow cookie", func(t *testing.T) {
		b := h.browser()
		m.next = ok
		cb := m.authorize(b.startSSO(""))
		u := mustURL(t, h.srv.URL)
		b.http.Jar.SetCookies(u, []*http.Cookie{{Name: OIDCFlowCookie, Value: "enc:v1:" + base64.StdEncoding.EncodeToString(make([]byte, 64)), Path: oidcCookiePath}})
		b.expectSSOError(b.do(http.MethodGet, cb, nil), "flow_expired")
	})

	t.Run("nonce used before", func(t *testing.T) {
		b := h.browser()
		authURL := b.startSSO("/first")
		flow := *b.cookie(OIDCFlowCookie)
		m.next = ok
		if res := b.do(http.MethodGet, m.authorize(authURL), nil); res.header.Get("Location") != "/first" {
			t.Fatalf("first callback: %d %q", res.status, res.header.Get("Location"))
		}
		// Someone who copied the flow cookie and gets a new code for
		// the same request replays it.
		replay := h.browser()
		replay.http.Jar.SetCookies(mustURL(t, h.srv.URL+oidcCookiePath), []*http.Cookie{{Name: flow.Name, Value: flow.Value, Path: oidcCookiePath}})
		calls := m.calls()
		replay.expectSSOError(replay.do(http.MethodGet, m.authorize(authURL), nil), "nonce_reused")
		if m.calls() != calls {
			t.Fatal("a reused flow reached the token endpoint")
		}
	})

	t.Run("provider error", func(t *testing.T) {
		b := h.browser()
		state := mustURL(t, b.startSSO("")).Query().Get("state")
		b.expectSSOError(b.do(http.MethodGet, "/api/v1/auth/oidc/callback?error=access_denied&error_description=no&state="+url.QueryEscape(state), nil), "provider_error")
	})

	t.Run("provider error without its flow", func(t *testing.T) {
		b := h.browser()
		b.expectSSOError(b.do(http.MethodGet, "/api/v1/auth/oidc/callback?error=access_denied", nil), "flow_expired")
		b.startSSO("")
		b.expectSSOError(b.do(http.MethodGet, "/api/v1/auth/oidc/callback?error=access_denied&state=attacker-state", nil), "state_mismatch")
	})

	t.Run("code replayed", func(t *testing.T) {
		b := h.browser()
		m.next = ok
		authURL := b.startSSO("")
		cb := mustURL(t, m.authorize(authURL))
		if res := b.do(http.MethodGet, cb.String(), nil); res.status != http.StatusFound || res.header.Get("Location") != "/" {
			t.Fatalf("first: %d %q", res.status, res.header.Get("Location"))
		}
		// A new flow with an old code fails at the provider (PKCE and
		// one-time codes).
		other := h.browser()
		authURL2 := other.startSSO("")
		q := cb.Query()
		q.Set("state", mustURL(t, authURL2).Query().Get("state"))
		other.expectSSOError(other.do(http.MethodGet, cb.Path+"?"+q.Encode(), nil), "provider_error")
	})
}

// AC8: a disabled user can't sign in with SSO.
func TestOIDCDisabledUser(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+reader.Id.String(), map[string]any{"disabled": true}, http.StatusOK)
	b := h.browser()
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "subject-reader"}, ""), "account_disabled")
	var n int
	if err := h.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1`, reader.Id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions of the disabled user: %d %v", n, err)
	}
}

// AC11: return_to only takes paths on Panelarr itself.
func TestOIDCReturnTo(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	h.setupAdmin()
	b := h.browser()
	for _, bad := range []string{
		"https://evil.example.com/", "http://evil.example.com", "//evil.example.com", "///evil.example.com",
		"/\\evil.example.com", "\\\\evil.example.com", "/x\\y", "evil.example.com", "library",
		"javascript:alert(1)", "data:text/html,x", " /library", "\t//evil.example.com",
		"/library\r\nSet-Cookie: x=y", "/library\x00", "https:/evil.example.com",
	} {
		start := "/api/v1/auth/oidc/start?return_to=" + url.QueryEscape(bad)
		res := b.doWith(http.MethodGet, start, nil, http.Header{"Accept": {"application/json"}})
		if res.status == http.StatusFound {
			t.Fatalf("return_to %q accepted: %s", bad, res.header.Get("Location"))
		}
		if res.status != http.StatusBadRequest {
			t.Fatalf("return_to %q: status %d: %s", bad, res.status, res.body)
		}
		// A navigation goes to the login page, without the return_to.
		// (A NUL is refused for every endpoint before its handler.)
		res = b.do(http.MethodGet, start, nil)
		if strings.ContainsRune(bad, 0) {
			if res.status != http.StatusBadRequest || res.problem(t).Code != CodeInvalidRequest {
				t.Fatalf("return_to %q as a navigation: %d %s", bad, res.status, res.body)
			}
			continue
		}
		if loc := res.header.Get("Location"); res.status != http.StatusFound || loc != "/login?sso_error=invalid_return_to" {
			t.Fatalf("return_to %q as a navigation: %d %q", bad, res.status, loc)
		}
	}
	b.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?return_to="+url.QueryEscape("//evil.example.com"), nil, http.StatusBadRequest, "invalid_return_to")
	for _, good := range []string{"/", "/library", "/works/abc?tab=chapters#c12", "/a/%2F%2Fb", "/search?q=//x"} {
		if loc := b.startSSO(good); !strings.HasPrefix(loc, m.srv.URL) {
			t.Fatalf("return_to %q: %s", good, loc)
		}
	}
	// The browser is sent back to the cleaned path.
	for in, want := range map[string]string{
		"/library/./series/../": "/library/", "/a/../../b?x=/../y": "/b?x=/../y", "/works/abc#../..": "/works/abc#../..",
		// Dot segments and doubled slashes can't make it scheme-relative.
		"/.//evil.example.com": "/evil.example.com", "/a/..//evil.example.com": "/evil.example.com",
	} {
		got, ok := localPath(in)
		if !ok || got != want {
			t.Fatalf("localPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// Rule 2: with allowed groups, only members sign in; groups come from
// the ID token, or from userinfo when the token has none.
func TestOIDCAllowedGroups(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m, "readers"))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")

	b := h.browser()
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "subject-reader", Groups: []string{"others"}}, ""), "not_allowed")
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "subject-reader"}, ""), "not_allowed")
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "subject-reader", UserinfoGroups: []string{"others"}}, ""), "not_allowed")
	for _, c := range []mockClaims{
		{Subject: "subject-reader", Groups: []string{"others", "readers"}},
		{Subject: "subject-reader", UserinfoGroups: []string{"readers"}},
	} {
		if res := h.browser().ssoLogin(m, c, "/ok"); res.header.Get("Location") != "/ok" {
			t.Fatalf("%+v: %d %q", c, res.status, res.header.Get("Location"))
		}
	}
}

// Failed sign-ins are rate limited per address, on their own: they never
// lock anyone out of password login.
func TestOIDCRateLimited(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")
	b := h.browser()
	for range 30 {
		// Without a flow cookie (another browser at the same address).
		nb := h.browser()
		nb.expectSSOError(nb.do(http.MethodGet, "/api/v1/auth/oidc/callback?code=x&state=y", nil), "flow_expired")
		nb.expectSSOError(nb.do(http.MethodGet, "/api/v1/auth/oidc/callback?error=access_denied", nil), "flow_expired")
		b.startSSO("")
		b.expectSSOError(b.do(http.MethodGet, "/api/v1/auth/oidc/callback?code=x&state=y", nil), "state_mismatch")
	}
	// None of those were sign-in attempts.
	if res := b.ssoLogin(m, mockClaims{Subject: "subject-reader"}, "/ok"); res.header.Get("Location") != "/ok" {
		t.Fatalf("sign-in after refused callbacks: %d %q", res.status, res.header.Get("Location"))
	}
	h.login("Admin", adminPassword)

	// Real failed sign-ins count, and block single sign-on only.
	for range 30 {
		h.browser().expectSSOError(h.browser().ssoLogin(m, mockClaims{Subject: "subject-stranger"}, ""), "not_linked")
	}
	calls := m.calls()
	b = h.browser()
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "subject-reader"}, ""), "rate_limited")
	if m.calls() != calls {
		t.Fatal("a rate-limited callback reached the token endpoint")
	}
	h.login("Admin", adminPassword)
}

// Some providers' issuers end in a slash. It is kept exactly, because
// discovery must name the same issuer, and stored with the link as is.
func TestOIDCIssuerWithTrailingSlash(t *testing.T) {
	t.Parallel()
	m := newMockOPAt(t, "/application/o/panelarr/")
	h := newHarness(t, withSSO(m), func(d *Deps) { d.Checks = append(d.Checks, Check{Name: "oidc", Run: d.SSO.Check}) })
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "subject-reader")
	if !strings.HasSuffix(m.issuer, "/") {
		t.Fatalf("issuer %q", m.issuer)
	}
	if res := h.browser().ssoLogin(m, mockClaims{Subject: "subject-reader"}, "/ok"); res.header.Get("Location") != "/ok" {
		t.Fatalf("callback: %d %q", res.status, res.header.Get("Location"))
	}
	health := call[apigen.SystemHealth](admin, http.MethodGet, "/api/v1/system/health", nil, http.StatusOK)
	for _, c := range health.Components {
		if c.Name == "oidc" && c.Status != apigen.ComponentHealthStatusOk {
			t.Fatalf("oidc health: %+v", c)
		}
	}
}

// A provider that can't be reached doesn't stop the server: health shows
// single sign-on degraded, starting a sign-in says so, and password login
// works.
func TestOIDCProviderDown(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	m.srv.Close()
	h := newHarness(t, withSSO(m), func(d *Deps) { d.Checks = append(d.Checks, Check{Name: "oidc", Run: d.SSO.Check}) })
	admin, _ := h.setupAdmin()
	h.browser().expectProblem(http.MethodGet, "/api/v1/auth/oidc/start", nil, http.StatusServiceUnavailable, "oidc_unavailable")
	health := call[apigen.SystemHealth](admin, http.MethodGet, "/api/v1/system/health", nil, http.StatusOK)
	found := false
	for _, c := range health.Components {
		if c.Name == "oidc" {
			found = true
			if c.Status == apigen.ComponentHealthStatusOk {
				t.Fatalf("oidc health: %+v", c)
			}
		}
	}
	if !found || health.Status == apigen.HealthStatusOk {
		t.Fatalf("health: %+v", health)
	}
	h.login("Admin", adminPassword)
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
