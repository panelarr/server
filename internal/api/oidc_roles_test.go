// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/sso"
)

// stopAtRedirects makes c look at each hop, like browser().
func stopAtRedirects(c *client) *client {
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// signedIn checks a callback signed the browser in and lands at want, and
// lets it send changing requests.
func (c *client) signedIn(res response, want string) apigen.Me {
	c.h.t.Helper()
	if res.status != http.StatusFound || res.header.Get("Location") != want {
		c.h.t.Fatalf("callback: status %d, location %q, want %q: %s", res.status, res.header.Get("Location"), want, res.body)
	}
	ck := c.cookie(CSRFCookie)
	if ck == nil {
		c.h.t.Fatal("no CSRF cookie after the callback")
	}
	c.csrf = ck.Value
	return call[apigen.Me](c, http.MethodGet, "/api/v1/me", nil, http.StatusOK)
}

// linkSSO runs a link flow from a signed-in browser.
func (c *client) linkSSO(m *mockOP, claims mockClaims, returnTo string) response {
	c.h.t.Helper()
	start := c.startLink(returnTo)
	m.next = claims
	return c.do(http.MethodGet, m.authorize(start), nil)
}

// startLink starts a link flow with POST /auth/oidc/link and returns the
// provider's URL.
func (c *client) startLink(returnTo string) string {
	c.h.t.Helper()
	return call[apigen.OidcLinkStart](c, http.MethodPost, "/api/v1/auth/oidc/link", map[string]string{"return_to": returnTo}, http.StatusOK).RedirectTo
}

// noSessionCookie checks a response set no session cookie.
func noSessionCookie(t *testing.T, res response) {
	t.Helper()
	for _, ck := range (&http.Response{Header: res.header}).Cookies() {
		if ck.Name == SessionCookie {
			t.Fatalf("session cookie set: %+v", ck)
		}
	}
}

func (h *harness) userNamed(admin *client, name string) apigen.User {
	h.t.Helper()
	list := call[apigen.UserList](admin, http.MethodGet, "/api/v1/users?page_size=200", nil, http.StatusOK)
	for _, u := range list.Users {
		if u.Username == name {
			return u
		}
	}
	h.t.Fatalf("no user %q", name)
	return apigen.User{}
}

func (h *harness) countUsers() int {
	h.t.Helper()
	var n int
	if err := h.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func isTrue(b *bool) bool { return b != nil && *b }

// AC4: unknown logins are refused unless provisioning is on; then a user
// is created with a clean, unique username, the default grants and the
// role from groups, and only members of the allowed groups get in.
func TestOIDCProvisioning(t *testing.T) {
	t.Parallel()
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		h := newHarness(t, withSSO(m))
		h.setupAdmin()
		before := h.countUsers()
		b := h.browser()
		b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "s-new", Username: "newbie", Groups: []string{"readers"}}, ""), "not_linked")
		if h.countUsers() != before {
			t.Fatal("a user was created with provisioning off")
		}
	})

	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) {
		c.AutoProvision, c.AllowedGroups, c.AdminGroups = true, []string{"readers", "admins"}, []string{"admins"}
	}))
	admin, _ := h.setupAdmin()

	before := h.countUsers()
	b := h.browser()
	b.expectSSOError(b.ssoLogin(m, mockClaims{Subject: "s-out", Username: "outsider", Groups: []string{"others"}}, ""), "not_allowed")
	if h.countUsers() != before {
		t.Fatal("a user outside the allowed groups was created")
	}

	b = h.browser()
	me := b.signedIn(b.ssoLogin(m, mockClaims{Subject: "s-ann", Username: "Ann Smith!", Name: "Ann\x00 Smith", Email: "ann@example.com", Groups: []string{"readers"}}, "/library"), "/library")
	u := me.User
	if u.Username != "AnnSmith" || u.DisplayName != "Ann Smith" || u.Role != apigen.RoleUser || isTrue(u.HasPassword) || !isTrue(u.Sso) {
		t.Fatalf("provisioned user: %+v", u)
	}
	if g := u.Grants; !g.MayAdd || g.MayDownload || g.MayManageMatches || g.ContentRatingCeiling != "suggestive" || !g.AllSources {
		t.Fatalf("not the default grants: %+v", g)
	}
	ids := call[apigen.IdentityList](b, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK)
	if len(ids.Identities) != 1 || ids.Identities[0].Subject != "s-ann" || ids.Identities[0].Email != "ann@example.com" || ids.Identities[0].Issuer != m.issuer {
		t.Fatalf("identities: %+v", ids)
	}
	// No password: password login fails like a wrong one.
	h.client().expectProblem(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "AnnSmith", "password": ""}, http.StatusUnauthorized, "invalid_credentials")
	h.client().expectProblem(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "AnnSmith", "password": "!none"}, http.StatusUnauthorized, "invalid_credentials")

	// Signing in again finds the same user.
	again := h.browser()
	if me2 := again.signedIn(again.ssoLogin(m, mockClaims{Subject: "s-ann", Username: "someone-else", Groups: []string{"readers"}}, "/"), "/"); me2.User.Id != u.Id {
		t.Fatalf("second sign-in created %s", me2.User.Username)
	}

	for _, tc := range []struct {
		claims mockClaims
		want   string
		role   apigen.Role
	}{
		// Taken, regardless of case.
		{mockClaims{Subject: "s-ann2", Username: "annsmith", Groups: []string{"readers"}}, "annsmith-2", apigen.RoleUser},
		{mockClaims{Subject: "s-ann3", Username: "ANNSMITH", Groups: []string{"readers"}}, "ANNSMITH-3", apigen.RoleUser},
		// An unusable claim: the email's local part, then the subject.
		{mockClaims{Subject: "s-bob", Username: "!é", Email: "bob.jones@example.com", Groups: []string{"readers"}}, "bob.jones", apigen.RoleUser},
		{mockClaims{Subject: "x9-sub", Groups: []string{"readers"}}, "x9-sub", apigen.RoleUser},
		{mockClaims{Subject: "-", Groups: []string{"readers"}}, "user", apigen.RoleUser},
		// Long names are cut.
		{mockClaims{Subject: "s-long", Username: strings.Repeat("a", 100), Groups: []string{"readers"}}, strings.Repeat("a", 58), apigen.RoleUser},
		// Admin groups make admins.
		{mockClaims{Subject: "s-boss", Username: "boss", Groups: []string{"admins"}}, "boss", apigen.RoleAdmin},
	} {
		c := h.browser()
		got := c.signedIn(c.ssoLogin(m, tc.claims, "/"), "/").User
		if got.Username != tc.want || got.Role != tc.role {
			t.Fatalf("%+v: got %q %s, want %q %s", tc.claims, got.Username, got.Role, tc.want, tc.role)
		}
	}
	if h.userNamed(admin, "boss").Role != apigen.RoleAdmin || !isTrue(h.userNamed(admin, "boss").Sso) {
		t.Fatal("the admin list doesn't show the provisioned admin")
	}
}

// AC5: linking from the account page, signing in either way, unlinking
// only with another way in, and admins seeing and removing links.
func TestOIDCLinking(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	readerPw, reader := h.createUser(admin, "reader", nil)
	reader1 := stopAtRedirects(readerPw)
	otherPw, _ := h.createUser(admin, "other", nil)
	other := stopAtRedirects(otherPw)

	if me := call[apigen.Me](reader1, http.MethodGet, "/api/v1/me", nil, http.StatusOK); !isTrue(me.User.HasPassword) || isTrue(me.User.Sso) {
		t.Fatalf("before linking: %+v", me.User)
	}
	res := reader1.linkSSO(m, mockClaims{Subject: "s-reader", Email: "reader@example.com"}, "/account?tab=logins")
	if res.status != http.StatusFound || res.header.Get("Location") != "/account?tab=logins" {
		t.Fatalf("link: %d %q", res.status, res.header.Get("Location"))
	}
	noSessionCookie(t, res) // linking starts no new session
	if me := call[apigen.Me](reader1, http.MethodGet, "/api/v1/me", nil, http.StatusOK); me.User.Id != reader.Id || !isTrue(me.User.Sso) {
		t.Fatalf("after linking: %+v", me.User)
	}
	ids := call[apigen.IdentityList](reader1, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK)
	if len(ids.Identities) != 1 || ids.Identities[0].Email != "reader@example.com" || ids.Identities[0].LastLoginAt == nil {
		t.Fatalf("identities: %+v", ids)
	}
	identity := ids.Identities[0].Id

	// Either way in.
	b := h.browser()
	if me := b.signedIn(b.ssoLogin(m, mockClaims{Subject: "s-reader"}, "/"), "/"); me.User.Id != reader.Id {
		t.Fatalf("SSO signed in as %s", me.User.Username)
	}
	h.login("reader", "user password 1")

	// Linking it again is a no-op.
	if res := reader1.linkSSO(m, mockClaims{Subject: "s-reader"}, "/account"); res.header.Get("Location") != "/account" {
		t.Fatalf("relink: %d %q", res.status, res.header.Get("Location"))
	}
	// Someone else can't take it: no account switch, the error goes back
	// to the page that started.
	res = other.linkSSO(m, mockClaims{Subject: "s-reader"}, "/account#logins")
	if res.header.Get("Location") != "/account?sso_error=already_linked#logins" {
		t.Fatalf("other user linking: %d %q", res.status, res.header.Get("Location"))
	}
	noSessionCookie(t, res)
	if me := call[apigen.Me](other, http.MethodGet, "/api/v1/me", nil, http.StatusOK); me.User.Username != "other" {
		t.Fatalf("linking switched the account to %s", me.User.Username)
	}
	// One login per provider.
	if res := reader1.linkSSO(m, mockClaims{Subject: "s-reader-2"}, "/account"); res.header.Get("Location") != "/account?sso_error=link_exists" {
		t.Fatalf("second link: %q", res.header.Get("Location"))
	}

	// Linking needs this browser's own session and a same-site start.
	anon := h.browser()
	anon.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link", nil, http.StatusUnauthorized, "unauthenticated")
	h.keyClient(readerPw.newAPIKey("read", "write")).expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link", nil, http.StatusForbidden, "session_required")
	for _, site := range []string{"cross-site", "same-site"} {
		reader1.header.Set("Sec-Fetch-Site", site)
		reader1.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link", nil, http.StatusForbidden, "cross_site")
	}
	reader1.header.Set("Sec-Fetch-Site", "same-origin")
	reader1.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link&invite=x", nil, http.StatusBadRequest, CodeInvalidRequest)
	reader1.header.Del("Sec-Fetch-Site")

	// A link flow whose session ended meanwhile links nothing.
	gone := stopAtRedirects(h.login("other", "user password 1"))
	start := gone.startLink("/account")
	gone.do(http.MethodPost, "/api/v1/auth/logout", nil)
	m.next = mockClaims{Subject: "s-other"}
	if res := gone.do(http.MethodGet, m.authorize(start), nil); res.header.Get("Location") != "/account?sso_error=session_changed" {
		t.Fatalf("link after logout: %d %q", res.status, res.header.Get("Location"))
	}
	h.browser().expectSSOError(h.browser().ssoLogin(m, mockClaims{Subject: "s-other"}, ""), "not_linked")
	// Nor one whose session was replaced, even by the same user.
	again := stopAtRedirects(h.login("other", "user password 1"))
	start = again.startLink("/account")
	in := call[apigen.LoginResponse](again, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "other", "password": "user password 1"}, http.StatusOK)
	again.csrf = in.CsrfToken
	m.next = mockClaims{Subject: "s-other"}
	if res := again.do(http.MethodGet, m.authorize(start), nil); res.header.Get("Location") != "/account?sso_error=session_changed" {
		t.Fatalf("link after a new session: %d %q", res.status, res.header.Get("Location"))
	}
	// Nor one whose browser signed in as someone else meanwhile: the
	// login must not land on that account.
	switched := stopAtRedirects(h.login("other", "user password 1"))
	start = switched.startLink("/account")
	in = call[apigen.LoginResponse](switched, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "reader", "password": "user password 1"}, http.StatusOK)
	switched.csrf = in.CsrfToken
	m.next = mockClaims{Subject: "s-other"}
	if res := switched.do(http.MethodGet, m.authorize(start), nil); res.header.Get("Location") != "/account?sso_error=session_changed" {
		t.Fatalf("link after switching users: %d %q", res.status, res.header.Get("Location"))
	}
	h.browser().expectSSOError(h.browser().ssoLogin(m, mockClaims{Subject: "s-other"}, ""), "not_linked")

	// Admins see the mark and the identities.
	if u := h.userNamed(admin, "reader"); !isTrue(u.Sso) || !isTrue(u.HasPassword) {
		t.Fatalf("admin list: %+v", u)
	}
	if u := h.userNamed(admin, "Admin"); isTrue(u.Sso) {
		t.Fatalf("admin list: %+v", u)
	}
	if u := call[apigen.User](admin, http.MethodGet, "/api/v1/users/"+reader.Id.String(), nil, http.StatusOK); !isTrue(u.Sso) {
		t.Fatalf("admin get: %+v", u)
	}
	if got := call[apigen.IdentityList](admin, http.MethodGet, "/api/v1/users/"+reader.Id.String()+"/identities", nil, http.StatusOK); len(got.Identities) != 1 || got.Identities[0].Id != identity {
		t.Fatalf("admin identities: %+v", got)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/users/"+uuid.NewString()+"/identities", nil, http.StatusNotFound, CodeNotFound)

	// Nobody unlinks someone else's login through /me.
	other = stopAtRedirects(h.login("other", "user password 1"))
	other.expectProblem(http.MethodDelete, "/api/v1/me/identities/"+identity.String(), nil, http.StatusNotFound, CodeNotFound)
	// Nor through another user's path.
	_, third := h.createUser(admin, "third", nil)
	admin.expectProblem(http.MethodDelete, "/api/v1/users/"+third.Id.String()+"/identities/"+identity.String(), nil, http.StatusNotFound, CodeNotFound)

	// With a password, the user may unlink.
	reader1.do(http.MethodDelete, "/api/v1/me/identities/"+identity.String(), nil)
	if got := call[apigen.IdentityList](reader1, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK); len(got.Identities) != 0 {
		t.Fatalf("after unlinking: %+v", got)
	}
	h.browser().expectSSOError(h.browser().ssoLogin(m, mockClaims{Subject: "s-reader"}, ""), "not_linked")
	h.login("reader", "user password 1")
}

// Starting a link changes the account, so it needs the session's CSRF
// token: in the header with POST /auth/oidc/link, or in csrf_token with the
// older GET. Fetch Metadata alone isn't enough: older browsers don't send
// it, and a same-origin navigation can come from a link someone posted.
func TestOIDCLinkStart(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	readerPw, _ := h.createUser(admin, "reader", nil)
	reader := stopAtRedirects(readerPw)
	const get = "/api/v1/auth/oidc/start?intent=link&return_to=/account"

	// The GET without the token, with any Fetch Metadata or none.
	for _, site := range []string{"", "same-origin", "none"} {
		if site == "" {
			reader.header.Del("Sec-Fetch-Site")
		} else {
			reader.header.Set("Sec-Fetch-Site", site)
		}
		reader.expectProblem(http.MethodGet, get, nil, http.StatusForbidden, "csrf_failed")
		reader.expectProblem(http.MethodGet, get+"&csrf_token=", nil, http.StatusForbidden, "csrf_failed")
		reader.expectProblem(http.MethodGet, get+"&csrf_token=not-the-token", nil, http.StatusForbidden, "csrf_failed")
		// Another session's token doesn't do either.
		other := h.login("reader", "user password 1")
		reader.expectProblem(http.MethodGet, get+"&csrf_token="+url.QueryEscape(other.csrf), nil, http.StatusForbidden, "csrf_failed")
	}
	// A cross-site start is refused even with the token.
	reader.header.Set("Sec-Fetch-Site", "cross-site")
	reader.expectProblem(http.MethodGet, get+"&csrf_token="+url.QueryEscape(reader.csrf), nil, http.StatusForbidden, "cross_site")
	reader.header.Del("Sec-Fetch-Site")
	if reader.cookie(OIDCFlowCookie) != nil {
		t.Fatal("a refused start set a flow cookie")
	}
	// With the token, the GET still works, and the flow links.
	res := reader.do(http.MethodGet, get+"&csrf_token="+url.QueryEscape(reader.csrf), nil)
	if res.status != http.StatusFound || !strings.HasPrefix(res.header.Get("Location"), m.srv.URL) {
		t.Fatalf("GET start with the token: %d %q", res.status, res.header.Get("Location"))
	}
	m.next = mockClaims{Subject: "s-reader"}
	if res := reader.do(http.MethodGet, m.authorize(res.header.Get("Location")), nil); res.header.Get("Location") != "/account" {
		t.Fatalf("link via GET: %d %q", res.status, res.header.Get("Location"))
	}

	// The POST needs the token in its header, a session and SSO.
	body := map[string]string{"return_to": "/account"}
	token := reader.csrf
	reader.csrf = ""
	reader.expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", body, http.StatusForbidden, "csrf_failed")
	reader.csrf = "not the token"
	reader.expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", body, http.StatusForbidden, "csrf_failed")
	reader.csrf = token
	h.browser().expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", body, http.StatusUnauthorized, "unauthenticated")
	h.keyClient(readerPw.newAPIKey("read", "write")).expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", body, http.StatusForbidden, "session_required")
	// Without a body it returns to /.
	start := call[apigen.OidcLinkStart](reader, http.MethodPost, "/api/v1/auth/oidc/link", nil, http.StatusOK)
	if !strings.HasPrefix(start.RedirectTo, m.srv.URL) || reader.cookie(OIDCFlowCookie) == nil {
		t.Fatalf("POST start: %+v", start)
	}
	m.next = mockClaims{Subject: "s-reader"}
	if res := reader.do(http.MethodGet, m.authorize(start.RedirectTo), nil); res.header.Get("Location") != "/" {
		t.Fatalf("link via POST: %d %q", res.status, res.header.Get("Location"))
	}
}

// return_to never points into the API: the browser lands there signed in,
// so it could, say, start a link flow the user didn't ask for.
func TestOIDCReturnToNotAPI(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	readerPw, _ := h.createUser(admin, "reader", nil)
	reader := stopAtRedirects(readerPw)
	anon := h.browser()
	for _, bad := range []string{
		"/api", "/api/", "/api/v1/auth/oidc/start?intent=link", "/API/v1/me", "/Api/v1",
		"/api?x=1", "/api#x", "/./api/v1", "/x/../api/v1", "/library/../../api/v1",
		"/%61pi/v1", "/%41PI/v1", "/%2561pi/v1", "/%252561pi/v1", "/api%2Fv1", "/%2Fapi/v1",
	} {
		q := url.QueryEscape(bad)
		anon.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?return_to="+q, nil, http.StatusBadRequest, "invalid_return_to")
		reader.expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?intent=link&csrf_token="+url.QueryEscape(reader.csrf)+"&return_to="+q, nil, http.StatusBadRequest, "invalid_return_to")
		reader.expectProblem(http.MethodPost, "/api/v1/auth/oidc/link", map[string]string{"return_to": bad}, http.StatusBadRequest, "invalid_return_to")
	}
	// Pages that only start like the API are fine.
	for _, good := range []string{"/apis", "/api-docs", "/library/api", "/search?q=/api/v1"} {
		if loc := anon.startSSO(good); !strings.HasPrefix(loc, m.srv.URL) {
			t.Fatalf("return_to %q: %s", good, loc)
		}
		call[apigen.OidcLinkStart](reader, http.MethodPost, "/api/v1/auth/oidc/link", map[string]string{"return_to": good}, http.StatusOK)
	}
}

// Unlinking never leaves a user without a way in; an admin's unlink also
// ends the user's single sign-on sessions.
func TestOIDCUnlinkLastLoginMethod(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.AutoProvision = true }))
	admin, _ := h.setupAdmin()

	b := h.browser()
	me := b.signedIn(b.ssoLogin(m, mockClaims{Subject: "s-sso", Username: "ssoonly"}, "/"), "/")
	ids := call[apigen.IdentityList](b, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK)
	identity := ids.Identities[0].Id
	b.expectProblem(http.MethodDelete, "/api/v1/me/identities/"+identity.String(), nil, http.StatusConflict, "last_login_method")
	admin.expectProblem(http.MethodDelete, "/api/v1/users/"+me.User.Id.String()+"/identities/"+identity.String(), nil, http.StatusConflict, "last_login_method")

	// The first password needs no current one, but like every other
	// /me write it needs a signed-in browser session and its CSRF token:
	// not anonymous, not an API key, not without the token.
	first := map[string]string{"current_password": "", "new_password": "a new password 1"}
	h.client().expectProblem(http.MethodPost, "/api/v1/me/password", first, http.StatusUnauthorized, "unauthenticated")
	h.keyClient(b.newAPIKey("read", "write")).expectProblem(http.MethodPost, "/api/v1/me/password", first, http.StatusForbidden, "session_required")
	token := b.csrf
	b.csrf = ""
	b.expectProblem(http.MethodPost, "/api/v1/me/password", first, http.StatusForbidden, "csrf_failed")
	b.csrf = "not the token"
	b.expectProblem(http.MethodPost, "/api/v1/me/password", first, http.StatusForbidden, "csrf_failed")
	b.csrf = token
	if res := b.do(http.MethodPost, "/api/v1/me/password", map[string]string{"current_password": "", "new_password": "a new password 1"}); res.status != http.StatusNoContent {
		t.Fatalf("set the first password: %d %s", res.status, res.body)
	}
	// From now on it does.
	b.expectProblem(http.MethodPost, "/api/v1/me/password", map[string]string{"current_password": "", "new_password": "another password 1"}, http.StatusForbidden, "invalid_credentials")
	pw := h.login("ssoonly", "a new password 1")
	if u := call[apigen.Me](pw, http.MethodGet, "/api/v1/me", nil, http.StatusOK).User; !isTrue(u.HasPassword) {
		t.Fatalf("has_password: %+v", u)
	}

	// An admin's unlink ends the SSO sessions, not the password ones.
	sso2 := h.browser()
	sso2.signedIn(sso2.ssoLogin(m, mockClaims{Subject: "s-sso"}, "/"), "/")
	if res := admin.do(http.MethodDelete, "/api/v1/users/"+me.User.Id.String()+"/identities/"+identity.String(), nil); res.status != http.StatusNoContent {
		t.Fatalf("admin unlink: %d %s", res.status, res.body)
	}
	b.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")
	sso2.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")
	call[apigen.Me](pw, http.MethodGet, "/api/v1/me", nil, http.StatusOK)
	admin.expectProblem(http.MethodDelete, "/api/v1/users/"+me.User.Id.String()+"/identities/"+identity.String(), nil, http.StatusNotFound, CodeNotFound)
}

// AC6: an invite accepted with SSO creates its user with the invite's
// role and grants, links the login and is used up.
func TestOIDCInvite(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	invite := func(role string) string {
		return call[apigen.InviteCreated](admin, http.MethodPost, "/api/v1/invites", map[string]any{
			"role": role, "grants": map[string]any{"may_download": true, "content_rating_ceiling": "safe"},
		}, http.StatusCreated).Token
	}
	startInvite := func(c *client, token string) string {
		res := c.do(http.MethodGet, "/api/v1/auth/oidc/start?return_to=/welcome&invite="+url.QueryEscape(token), nil)
		if res.status != http.StatusFound {
			t.Fatalf("start: %d %s", res.status, res.body)
		}
		return res.header.Get("Location")
	}

	token := invite("user")
	b := h.browser()
	authURL := startInvite(b, token)
	// The invite token goes nowhere near the provider.
	if strings.Contains(authURL, token) || strings.Contains(b.cookie(OIDCFlowCookie).Value, token) {
		t.Fatal("the invite token is readable outside the server")
	}
	m.next = mockClaims{Subject: "s-invited", Username: "invited", Email: "invited@example.com"}
	me := b.signedIn(b.do(http.MethodGet, m.authorize(authURL), nil), "/welcome")
	if u := me.User; u.Username != "invited" || u.Role != apigen.RoleUser || !u.Grants.MayDownload || u.Grants.ContentRatingCeiling != "safe" || !isTrue(u.Sso) || isTrue(u.HasPassword) {
		t.Fatalf("invited user: %+v", u)
	}
	h.client().expectProblem(http.MethodPost, "/api/v1/auth/invites/check", map[string]string{"token": token}, http.StatusNotFound, "invite_invalid")
	// Signs in with the link from now on.
	again := h.browser()
	if got := again.signedIn(again.ssoLogin(m, mockClaims{Subject: "s-invited"}, "/"), "/"); got.User.Id != me.User.Id {
		t.Fatal("not the invited user")
	}

	// An unknown or used invite.
	h.browser().expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?invite=nope", nil, http.StatusNotFound, "invite_invalid")
	h.browser().expectProblem(http.MethodGet, "/api/v1/auth/oidc/start?invite="+url.QueryEscape(token), nil, http.StatusNotFound, "invite_invalid")

	// Two browsers race for one invite: one user.
	token = invite("admin")
	b1, b2 := h.browser(), h.browser()
	u1, u2 := startInvite(b1, token), startInvite(b2, token)
	m.next = mockClaims{Subject: "s-first", Username: "first"}
	if got := b1.signedIn(b1.do(http.MethodGet, m.authorize(u1), nil), "/welcome"); got.User.Role != apigen.RoleAdmin {
		t.Fatalf("admin invite: %+v", got.User)
	}
	m.next = mockClaims{Subject: "s-second", Username: "second"}
	b2.expectSSOError(b2.do(http.MethodGet, m.authorize(u2), nil), "invite_invalid")
	h.browser().expectSSOError(h.browser().ssoLogin(m, mockClaims{Subject: "s-second"}, ""), "not_linked")

	// A login that is linked already just signs in; the invite stays.
	token = invite("user")
	b3 := h.browser()
	m.next = mockClaims{Subject: "s-invited"}
	if got := b3.signedIn(b3.do(http.MethodGet, m.authorize(startInvite(b3, token)), nil), "/welcome"); got.User.Id != me.User.Id {
		t.Fatal("not the linked user")
	}
	call[apigen.InviteCheck](h.client(), http.MethodPost, "/api/v1/auth/invites/check", map[string]string{"token": token}, http.StatusOK)
}

// AC7: with admin groups, linked users' roles follow them on each
// sign-in; the last enabled admin is never demoted; users without a
// linked login and a missing groups claim change nothing.
func TestOIDCRoleSync(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.AdminGroups = []string{"admins", "ops"} }))
	admin, adminUser := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	_, plain := h.createUser(admin, "plain", nil)
	h.link(m, reader.Id, "s-reader")
	h.link(m, adminUser.Id, "s-admin")
	signIn := func(subject string, c mockClaims) apigen.Role {
		t.Helper()
		c.Subject = subject
		b := h.browser()
		return b.signedIn(b.ssoLogin(m, c, "/"), "/").User.Role
	}

	// The only admin, signing in outside the admin groups, stays admin.
	if got := signIn("s-admin", mockClaims{Groups: []string{"readers"}}); got != apigen.RoleAdmin {
		t.Fatalf("the last admin was demoted: %s", got)
	}
	if got := signIn("s-reader", mockClaims{Groups: []string{"readers", "ops"}}); got != apigen.RoleAdmin {
		t.Fatalf("joined an admin group: %s", got)
	}
	// Groups only in userinfo count too.
	if got := signIn("s-reader", mockClaims{UserinfoGroups: []string{"readers"}}); got != apigen.RoleUser {
		t.Fatalf("left the admin groups: %s", got)
	}
	if got := signIn("s-reader", mockClaims{UserinfoGroups: []string{"admins"}}); got != apigen.RoleAdmin {
		t.Fatalf("joined via userinfo: %s", got)
	}
	// No groups claim at all: nothing changes.
	if got := signIn("s-reader", mockClaims{}); got != apigen.RoleAdmin {
		t.Fatalf("missing groups claim changed the role: %s", got)
	}
	// An empty list is "in no group".
	if got := signIn("s-reader", mockClaims{Groups: []string{}}); got != apigen.RoleUser {
		t.Fatalf("empty groups: %s", got)
	}
	// With another admin around, the first admin is demoted like anyone.
	signIn("s-reader", mockClaims{Groups: []string{"admins"}})
	if got := signIn("s-admin", mockClaims{Groups: []string{}}); got != apigen.RoleUser {
		t.Fatalf("admin outside the groups with another admin: %s", got)
	}
	// Now reader is the last admin: never demoted.
	if got := signIn("s-reader", mockClaims{Groups: []string{}}); got != apigen.RoleAdmin {
		t.Fatalf("the last admin was demoted: %s", got)
	}
	// A disabled admin doesn't count as another admin.
	if got := signIn("s-admin", mockClaims{Groups: []string{"admins"}}); got != apigen.RoleAdmin {
		t.Fatal(got)
	}
	readerAdmin := h.login("reader", "user password 1")
	call[apigen.User](readerAdmin, http.MethodPatch, "/api/v1/users/"+adminUser.Id.String(), map[string]any{"disabled": true}, http.StatusOK)
	if got := signIn("s-reader", mockClaims{Groups: []string{}}); got != apigen.RoleAdmin {
		t.Fatalf("demoted while the other admin is disabled: %s", got)
	}
	// A user without a linked login never changes.
	if r := call[apigen.User](readerAdmin, http.MethodGet, "/api/v1/users/"+plain.Id.String(), nil, http.StatusOK).Role; r != apigen.RoleUser {
		t.Fatalf("plain user: %s", r)
	}
}

// AC9: password login keeps working with single sign-on on and the
// provider down; limited to admins, only admins sign in with a password,
// and that is the break-glass way in.
func TestOIDCPasswordAdminsOnly(t *testing.T) {
	t.Parallel()
	t.Run("all by default, provider down", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		m.srv.Close()
		h := newHarness(t, withSSO(m))
		admin, _ := h.setupAdmin()
		h.createUser(admin, "reader", nil) // signs in with a password
	})
	// Break-glass: limited to admins and the provider down from the
	// start, the admin still signs in with a password.
	t.Run("admins, provider down", func(t *testing.T) {
		t.Parallel()
		m := newMockOP(t)
		m.srv.Close()
		h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.PasswordAdminsOnly = true }))
		admin, _ := h.setupAdmin()
		h.browser().expectProblem(http.MethodGet, "/api/v1/auth/oidc/start", nil, http.StatusServiceUnavailable, "oidc_unavailable")
		h.login("Admin", adminPassword)
		reader := call[apigen.User](admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "reader", "password": "user password 1"}, http.StatusCreated)
		h.client().expectProblem(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "reader", "password": "user password 1"}, http.StatusUnauthorized, "invalid_credentials")
		// Disabled, a non-admin's right password still answers like a
		// wrong one (not account_disabled).
		call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+reader.Id.String(), map[string]any{"disabled": true}, http.StatusOK)
		expectSameLoginAnswer(t, h,
			map[string]string{"username": "reader", "password": "user password 1"},
			map[string]string{"username": "reader", "password": "wrong password"},
		)
	})

	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.PasswordAdminsOnly = true }))
	admin, adminUser := h.setupAdmin()
	methods := call[apigen.AuthMethods](h.client(), http.MethodGet, "/api/v1/auth/methods", nil, http.StatusOK)
	if methods.Password != apigen.AuthMethodsPasswordAdmins || methods.Oidc == nil {
		t.Fatalf("methods: %+v", methods)
	}
	reader := call[apigen.User](admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "reader", "password": "user password 1"}, http.StatusCreated)
	// A non-admin's password login says nothing about the password: right,
	// wrong and an unknown user get byte-identical answers.
	expectSameLoginAnswer(t, h,
		map[string]string{"username": "reader", "password": "user password 1"},
		map[string]string{"username": "reader", "password": "wrong password"},
		map[string]string{"username": "nobody", "password": "user password 1"},
	)
	h.link(m, reader.Id, "s-reader")
	b := h.browser()
	b.signedIn(b.ssoLogin(m, mockClaims{Subject: "s-reader"}, "/"), "/")
	// Their password doesn't count as a way in: no unlinking the login.
	ids := call[apigen.IdentityList](b, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK)
	b.expectProblem(http.MethodDelete, "/api/v1/me/identities/"+ids.Identities[0].Id.String(), nil, http.StatusConflict, "last_login_method")
	// An admin's password does.
	h.link(m, adminUser.Id, "s-admin")
	adminIDs := call[apigen.IdentityList](admin, http.MethodGet, "/api/v1/users/"+adminUser.Id.String()+"/identities", nil, http.StatusOK)
	if res := admin.do(http.MethodDelete, "/api/v1/me/identities/"+adminIDs.Identities[0].Id.String(), nil); res.status != http.StatusNoContent {
		t.Fatalf("admin unlinking own login: %d %s", res.status, res.body)
	}

	// Break-glass: the provider is down, admins still get in.
	// (Discovery is cached by now: single sign-on fails at the provider.)
	m.srv.Close()
	down := h.browser()
	down.expectSSOError(down.ssoLogin(m, mockClaims{Subject: "s-reader"}, ""), "provider_error")
	h.login("Admin", adminPassword)
}

// AC10: logout ends the session; with provider logout, a single sign-on
// session also gets the provider's end-session URL.
func TestOIDCLogout(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) { c.ProviderLogout = true }))
	admin, adminUser := h.setupAdmin()
	h.link(m, adminUser.Id, "s-admin")

	b := h.browser()
	b.signedIn(b.ssoLogin(m, mockClaims{Subject: "s-admin"}, "/"), "/")
	out := call[apigen.LogoutResult](b, http.MethodPost, "/api/v1/auth/logout", nil, http.StatusOK)
	u := mustURL(t, out.RedirectTo)
	q := u.Query()
	if !strings.HasPrefix(out.RedirectTo, m.srv.URL+"/logout?") || q.Get("realm") != "test" || q.Get("client_id") != mockClientID ||
		q.Get("post_logout_redirect_uri") != "https://panelarr.example.com/login" {
		t.Fatalf("redirect_to: %s", out.RedirectTo)
	}
	if strings.Contains(out.RedirectTo, mockClientSecret) {
		t.Fatal("the client secret is in the logout URL")
	}
	b.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")

	// A password session has nothing to end at the provider.
	if res := admin.do(http.MethodPost, "/api/v1/auth/logout", nil); res.status != http.StatusNoContent {
		t.Fatalf("password logout: %d %s", res.status, res.body)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")

	// Local logout (the default) ends Panelarr's session only.
	m2 := newMockOP(t)
	h2 := newHarness(t, withSSO(m2))
	_, a2 := h2.setupAdmin()
	h2.link(m2, a2.Id, "s-admin")
	b2 := h2.browser()
	b2.signedIn(b2.ssoLogin(m2, mockClaims{Subject: "s-admin"}, "/"), "/")
	if res := b2.do(http.MethodPost, "/api/v1/auth/logout", nil); res.status != http.StatusNoContent {
		t.Fatalf("local logout: %d %s", res.status, res.body)
	}
	b2.expectProblem(http.MethodGet, "/api/v1/me", nil, http.StatusUnauthorized, "unauthenticated")
}

// AC12: admins see whether single sign-on is on, the issuer and whether
// it's reachable, and the settings; never the client secret.
func TestOIDCSystemAuth(t *testing.T) {
	t.Parallel()
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		admin, _ := h.setupAdmin()
		got := call[apigen.SystemAuth](admin, http.MethodGet, "/api/v1/system/auth", nil, http.StatusOK)
		if got.Password != apigen.SystemAuthPasswordAll || got.Oidc.Enabled || got.Oidc.Issuer != nil {
			t.Fatalf("off: %+v", got)
		}
	})
	m := newMockOP(t)
	h := newHarness(t, withSSOConfig(m, func(c *sso.Config) {
		c.AutoProvision, c.AllowedGroups, c.AdminGroups, c.ProviderLogout, c.PasswordAdminsOnly = true, []string{"readers"}, []string{"admins"}, true, true
	}))
	admin, _ := h.setupAdmin()
	res := admin.do(http.MethodGet, "/api/v1/system/auth", nil)
	if strings.Contains(string(res.body), mockClientSecret) {
		t.Fatalf("the client secret is returned: %s", res.body)
	}
	got := call[apigen.SystemAuth](admin, http.MethodGet, "/api/v1/system/auth", nil, http.StatusOK)
	o := got.Oidc
	if got.Password != apigen.SystemAuthPasswordAdmins || !o.Enabled || deref(o.Issuer) != m.issuer || !isTrue(o.Reachable) || o.Error != nil ||
		isTrue(o.PublicClient) || deref(o.ClientId) != mockClientID || !isTrue(o.AutoProvision) || o.Logout == nil || *o.Logout != apigen.OidcStatusLogoutProvider ||
		o.AdminGroups == nil || len(*o.AdminGroups) != 1 || (*o.AdminGroups)[0] != "admins" || o.AllowedGroups == nil || (*o.AllowedGroups)[0] != "readers" || deref(o.UsernameClaim) != "preferred_username" {
		t.Fatalf("on: %+v", got)
	}
	for _, path := range []string{"/api/v1/auth/methods", "/api/v1/system/health", "/api/v1/me"} {
		if res := admin.do(http.MethodGet, path, nil); strings.Contains(string(res.body), mockClientSecret) {
			t.Fatalf("%s returns the client secret", path)
		}
	}

	// Without admin groups the list is empty: roles are Panelarr's own.
	plain := newHarness(t, withSSO(newMockOP(t)))
	adminPlain, _ := plain.setupAdmin()
	if got := call[apigen.SystemAuth](adminPlain, http.MethodGet, "/api/v1/system/auth", nil, http.StatusOK); got.Oidc.AdminGroups == nil || len(*got.Oidc.AdminGroups) != 0 {
		t.Fatalf("no admin groups: %+v", got.Oidc)
	}

	down := newMockOP(t)
	down.srv.Close()
	hd := newHarness(t, withSSO(down))
	adminDown, _ := hd.setupAdmin()
	got = call[apigen.SystemAuth](adminDown, http.MethodGet, "/api/v1/system/auth", nil, http.StatusOK)
	if !got.Oidc.Enabled || isTrue(got.Oidc.Reachable) || got.Oidc.Error == nil {
		t.Fatalf("down: %+v", got)
	}
}

// A forged callback (a wrong state) doesn't end a sign-in in progress:
// the flow cookie is cleared only by a callback that matches it.
func TestOIDCForgedCallbackKeepsTheFlow(t *testing.T) {
	t.Parallel()
	m := newMockOP(t)
	h := newHarness(t, withSSO(m))
	admin, _ := h.setupAdmin()
	_, reader := h.createUser(admin, "reader", nil)
	h.link(m, reader.Id, "s-reader")

	b := h.browser()
	m.next = mockClaims{Subject: "s-reader"}
	cb := m.authorize(b.startSSO("/library"))
	for _, forged := range []string{
		"/api/v1/auth/oidc/callback?code=x&state=attacker-state",
		"/api/v1/auth/oidc/callback?error=access_denied&state=attacker-state",
		"/api/v1/auth/oidc/callback?code=x",
	} {
		res := b.do(http.MethodGet, forged, nil)
		b.expectSSOError(res, "state_mismatch")
		for _, ck := range (&http.Response{Header: res.header}).Cookies() {
			if ck.Name == OIDCFlowCookie {
				t.Fatalf("%s touched the flow cookie: %+v", forged, ck)
			}
		}
		if b.cookie(OIDCFlowCookie) == nil {
			t.Fatalf("%s ended the flow", forged)
		}
	}
	// The real callback still finishes, and uses the flow up.
	b.signedIn(b.do(http.MethodGet, cb, nil), "/library")
	if b.cookie(OIDCFlowCookie) != nil {
		t.Fatal("flow cookie kept after the callback")
	}
}

var requestIDField = regexp.MustCompile(`"request_id":"[^"]*"`)

// expectSameLoginAnswer checks each password login fails with 401
// invalid_credentials, sets no cookie and gets the same body, byte for
// byte apart from the request ID.
func expectSameLoginAnswer(t *testing.T, h *harness, logins ...map[string]string) {
	t.Helper()
	var first response
	for i, l := range logins {
		res := h.client().do(http.MethodPost, "/api/v1/auth/login", l)
		if res.status != http.StatusUnauthorized || res.problem(t).Code != "invalid_credentials" {
			t.Fatalf("login %d: %d %s", i, res.status, res.body)
		}
		if c := res.header.Values("Set-Cookie"); len(c) > 0 {
			t.Fatalf("login %d sets cookies: %v", i, c)
		}
		// Every request has its own request_id; everything else must
		// be byte-identical.
		res.body = requestIDField.ReplaceAll(res.body, []byte(`"request_id":""`))
		if i == 0 {
			first = res
			continue
		}
		if !bytes.Equal(res.body, first.body) || res.header.Get("Content-Type") != first.header.Get("Content-Type") {
			t.Fatalf("login %d answers differently:\n%s\n%s", i, first.body, res.body)
		}
	}
}
