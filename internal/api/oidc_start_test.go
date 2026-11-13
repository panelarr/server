// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/sso"
)

// Browser headers: a navigation in a browser with Fetch Metadata, one
// without it, and a script's fetch.
var (
	navigate   = http.Header{"Sec-Fetch-Mode": {"navigate"}, "Accept": {"text/html,application/xhtml+xml,*/*;q=0.8"}}
	oldBrowser = http.Header{"Accept": {"text/html,application/xhtml+xml,*/*;q=0.8"}}
	scriptJSON = http.Header{"Sec-Fetch-Mode": {"cors"}, "Accept": {"application/json"}}
)

// expectStartRefused checks a start answered a navigation with a redirect
// to loc, set no flow cookie and can't be cached.
func (c *client) expectStartRefused(path string, header http.Header, loc string) {
	c.h.t.Helper()
	res := c.doWith(http.MethodGet, path, nil, header)
	if res.status != http.StatusFound || res.header.Get("Location") != loc {
		c.h.t.Fatalf("%s: status %d, location %q, want %q: %s", path, res.status, res.header.Get("Location"), loc, res.body)
	}
	if res.header.Get("Cache-Control") != "no-store" {
		c.h.t.Fatalf("%s: Cache-Control %q", path, res.header.Get("Cache-Control"))
	}
	for _, ck := range (&http.Response{Header: res.header}).Cookies() {
		if ck.Name == OIDCFlowCookie || ck.Name == SessionCookie {
			c.h.t.Fatalf("%s: a refused start set %s", path, ck.Name)
		}
	}
}

// A refused start is a page for browsers: the login page with the code in
// sso_error, keeping a valid return_to as its redirect. API clients that
// can be told apart still get the problem.
func TestOIDCStartRefusedNavigation(t *testing.T) {
	t.Parallel()
	const start = "/api/v1/auth/oidc/start"

	t.Run("provider down", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		m.srv.Close()
		h := newHarness(t, withSSO(m))
		h.setupAdmin()
		b := h.browser()
		for _, hd := range []http.Header{navigate, oldBrowser, nil, {"Accept": {"application/json, text/html"}}} {
			b.expectStartRefused(start+"?return_to="+url.QueryEscape("/works/abc?tab=chapters"), hd,
				"/login?redirect=%2Fworks%2Fabc%3Ftab%3Dchapters&sso_error=oidc_unavailable")
		}
		b.expectStartRefused(start, navigate, "/login?sso_error=oidc_unavailable")
		b.expectStartRefused(start+"?return_to=/", navigate, "/login?sso_error=oidc_unavailable")
		// The return_to is cleaned, as for a successful start.
		b.expectStartRefused(start+"?return_to=/library/./x/../", navigate, "/login?redirect=%2Flibrary%2F&sso_error=oidc_unavailable")
		// An invalid return_to is refused first, and never passed on.
		b.expectStartRefused(start+"?return_to="+url.QueryEscape("//evil.example.com"), navigate, "/login?sso_error=invalid_return_to")
		b.expectStartRefused(start+"?return_to="+url.QueryEscape("/api/v1/me"), navigate, "/login?sso_error=invalid_return_to")

		// API clients: a script's fetch, or a client asking for JSON.
		for _, hd := range []http.Header{scriptJSON, {"Accept": {"application/json"}}, {"Accept": {"application/problem+json"}}, {"Sec-Fetch-Mode": {"no-cors"}}} {
			res := b.doWith(http.MethodGet, start+"?return_to=/library", nil, hd)
			if p := res.problem(t); res.status != http.StatusServiceUnavailable || p.Code != "oidc_unavailable" {
				t.Fatalf("%v: %d %s", hd, res.status, res.body)
			}
		}
	})

	t.Run("invite", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		h := newHarness(t, withSSO(m))
		h.setupAdmin()
		h.browser().expectStartRefused(start+"?return_to=/welcome&invite=nope", navigate, "/login?redirect=%2Fwelcome&sso_error=invite_invalid")
		h.browser().expectProblem(http.MethodGet, start+"?invite=nope", nil, http.StatusNotFound, "invite_invalid")
	})

	// A refused link start goes back to the page it came from, as a
	// failed link callback does; the user is signed in, so the login page
	// would only send them on.
	t.Run("link", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		h := newHarness(t, withSSO(m))
		admin, _ := h.setupAdmin()
		readerPw, _ := h.createUser(admin, "reader", nil)
		reader := stopAtRedirects(readerPw)
		link := start + "?intent=link&return_to=" + url.QueryEscape("/settings?tab=security")
		back := func(code string) string { return "/settings?sso_error=" + code + "&tab=security" }

		reader.expectStartRefused(link, navigate, back("csrf_failed"))
		reader.expectStartRefused(link+"&csrf_token=not-the-token", oldBrowser, back("csrf_failed"))
		cross := http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Site": {"cross-site"}}
		reader.expectStartRefused(link+"&csrf_token="+url.QueryEscape(reader.csrf), cross, back("cross_site"))
		// Without a valid return_to, the login page.
		reader.expectStartRefused(start+"?intent=link&return_to="+url.QueryEscape("https://evil.example.com/"), navigate, "/login?sso_error=invalid_return_to")
		reader.expectStartRefused(start+"?intent=link", navigate, "/login?sso_error=csrf_failed")
		// Still signed in, still unlinked; a script gets the problem.
		if me := call[apigen.Me](reader, http.MethodGet, "/api/v1/me", nil, http.StatusOK); isTrue(me.User.Sso) {
			t.Fatalf("linked: %+v", me.User)
		}
		res := reader.doWith(http.MethodGet, link, nil, scriptJSON)
		if p := res.problem(t); res.status != http.StatusForbidden || p.Code != "csrf_failed" {
			t.Fatalf("script: %d %s", res.status, res.body)
		}
		// A good start still goes to the provider.
		res = reader.doWith(http.MethodGet, link+"&csrf_token="+url.QueryEscape(reader.csrf), nil, navigate)
		if loc := res.header.Get("Location"); res.status != http.StatusFound || !strings.HasPrefix(loc, m.srv.URL) {
			t.Fatalf("link start: %d %q", res.status, loc)
		}
	})

	// Off, the endpoint doesn't exist, for browsers too.
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.setupAdmin()
		res := h.browser().doWith(http.MethodGet, start+"?return_to=/library", nil, navigate)
		if p := res.problem(t); res.status != http.StatusNotFound || p.Code != CodeNotFound {
			t.Fatalf("off: %d %s", res.status, res.body)
		}
	})
}

// With admin groups configured, the groups decide the role of a user an
// invite creates, as they do on every later sign-in: an admin invite
// accepted from outside the groups makes a user, and one accepted from
// inside them an admin (a user invite too).
func TestOIDCInviteAdminGroups(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.AdminGroups = []string{"admins"} }))
	admin, _ := h.setupAdmin()
	accept := func(role, subject string, groups []string) apigen.User {
		t.Helper()
		token := call[apigen.InviteCreated](admin, http.MethodPost, "/api/v1/invites", map[string]any{"role": role}, http.StatusCreated).Token
		b := h.browser()
		res := b.do(http.MethodGet, "/api/v1/auth/oidc/start?return_to=/welcome&invite="+url.QueryEscape(token), nil)
		if res.status != http.StatusFound {
			t.Fatalf("start: %d %s", res.status, res.body)
		}
		m.next = mockClaims{Subject: subject, Username: subject, Groups: groups}
		u := b.signedIn(b.do(http.MethodGet, m.authorize(res.header.Get("Location")), nil), "/welcome").User
		// The role sticks: the user list says the same.
		if got := call[apigen.User](admin, http.MethodGet, "/api/v1/users/"+u.Id.String(), nil, http.StatusOK); got.Role != u.Role {
			t.Fatalf("%s: signed in as %s, stored as %s", subject, u.Role, got.Role)
		}
		return u
	}

	if u := accept("admin", "outsider", []string{"readers"}); u.Role != apigen.RoleUser {
		t.Fatalf("admin invite outside the admin groups: %s", u.Role)
	}
	if u := accept("admin", "member", []string{"readers", "admins"}); u.Role != apigen.RoleAdmin {
		t.Fatalf("admin invite inside the admin groups: %s", u.Role)
	}
	if u := accept("user", "promoted", []string{"admins"}); u.Role != apigen.RoleAdmin {
		t.Fatalf("user invite inside the admin groups: %s", u.Role)
	}
}
