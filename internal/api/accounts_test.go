// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestSetupGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	anon := h.client()

	// Before the first account, everything but setup answers setup_required.
	anon.expectProblem(http.MethodGet, "/api/v1/users", nil, 409, "setup_required")
	anon.expectProblem(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "a", "password": "b"}, 409, "setup_required")
	st := call[apigen.SetupStatus](anon, http.MethodGet, "/api/v1/setup", nil, 200)
	if st.AdminCreated || st.Completed {
		t.Fatalf("fresh: %+v", st)
	}

	admin, me := h.setupAdmin()
	if me.Role != apigen.RoleAdmin || me.Username != "Admin" {
		t.Fatalf("first account: %+v", me)
	}
	// The setup cookie signs the admin in.
	got := call[apigen.Me](admin, http.MethodGet, "/api/v1/me", nil, 200)
	if got.User.Id != me.Id || got.Auth.Method != apigen.MeAuthMethodSession || len(got.Auth.Scopes) != 4 {
		t.Fatalf("me: %+v", got)
	}

	// Nobody can create another "first" admin.
	anon.expectProblem(http.MethodPost, "/api/v1/setup/admin", map[string]string{"username": "intruder", "password": "intruder pass"}, 409, "already_set_up")
	// Finishing the wizard is for admins.
	anon.expectProblem(http.MethodPost, "/api/v1/setup/complete", nil, 401, "unauthenticated")
	done := call[apigen.SetupStatus](admin, http.MethodPost, "/api/v1/setup/complete", nil, 200)
	if !done.AdminCreated || !done.Completed || done.CompletedAt == nil {
		t.Fatalf("completed: %+v", done)
	}
	again := call[apigen.SetupStatus](admin, http.MethodPost, "/api/v1/setup/complete", nil, 200)
	if !again.CompletedAt.Equal(*done.CompletedAt) {
		t.Fatal("completing twice moved the time")
	}
}

func TestFirstAdminRace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() {
			c := h.client()
			codes[i] = c.do(http.MethodPost, "/api/v1/setup/admin",
				map[string]string{"username": "admin" + strconv.Itoa(i), "password": adminPassword}).status
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 409:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("%d first admins created", created)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	_, u := h.createUser(admin, "Reader", nil)

	anon := h.client()
	login := func(user, pass string) response {
		return anon.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": user, "password": pass})
	}
	// Usernames are case-insensitive; cookies are HTTP-only and SameSite.
	res := login("READER", "user password 1")
	if res.status != 200 {
		t.Fatalf("login: %d %s", res.status, res.body)
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range (&http.Response{Header: res.header}).Cookies() {
		switch c.Name {
		case SessionCookie:
			sessionCookie = c
		case CSRFCookie:
			csrfCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.HttpOnly || csrfCookie.Value == "" {
		t.Fatalf("csrf cookie: %+v", csrfCookie)
	}

	// Unknown users and wrong passwords look the same.
	for _, tc := range [][2]string{{"nobody", "whatever pass"}, {"reader", "wrong password"}} {
		if p := login(tc[0], tc[1]); p.status != 401 || p.problem(t).Code != "invalid_credentials" {
			t.Fatalf("%v: %d %s", tc, p.status, p.body)
		}
	}

	// Disabled accounts can't sign in, and lose their sessions.
	userClient := h.login("reader", "user password 1")
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+u.Id.String(), map[string]any{"disabled": true}, 200)
	userClient.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "unauthenticated")
	if p := login("reader", "user password 1"); p.status != 403 || p.problem(t).Code != "account_disabled" {
		t.Fatalf("disabled: %d %s", p.status, p.body)
	}
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setupAdmin()
	anon := h.client()
	for range 5 {
		anon.expectProblem(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong password"}, 401, "invalid_credentials")
	}
	// Even the right password is refused while blocked.
	res := anon.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "admin", "password": adminPassword})
	if res.status != 429 || res.problem(t).Code != "rate_limited" {
		t.Fatalf("blocked: %d %s", res.status, res.body)
	}
	if ra, err := strconv.Atoi(res.header.Get("Retry-After")); err != nil || ra <= 0 || ra > 15*60 {
		t.Fatalf("Retry-After %q", res.header.Get("Retry-After"))
	}
}

func TestCSRF(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	body := map[string]any{"display_name": "Boss"}

	token := admin.csrf
	admin.csrf = ""
	admin.expectProblem(http.MethodPatch, "/api/v1/me", body, 403, "csrf_failed")
	admin.csrf = "not the token"
	admin.expectProblem(http.MethodPatch, "/api/v1/me", body, 403, "csrf_failed")
	admin.csrf = token
	admin.header.Set("Sec-Fetch-Site", "cross-site")
	admin.expectProblem(http.MethodPatch, "/api/v1/me", body, 403, "csrf_failed")
	admin.header.Set("Sec-Fetch-Site", "same-origin")
	me := call[apigen.Me](admin, http.MethodPatch, "/api/v1/me", body, 200)
	if me.User.DisplayName != "Boss" {
		t.Fatalf("display name %q", me.User.DisplayName)
	}
	// Reads need no token.
	admin.csrf = ""
	call[apigen.Me](admin, http.MethodGet, "/api/v1/me", nil, 200)
}

func TestSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setupAdmin()
	a := h.login("admin", adminPassword)
	a.header.Set("User-Agent", "browser A")
	b := h.login("admin", adminPassword)

	list := call[apigen.SessionList](a, http.MethodGet, "/api/v1/me/sessions", nil, 200)
	if len(list.Sessions) != 3 { // setup, a, b
		t.Fatalf("%d sessions", len(list.Sessions))
	}
	var current int
	var other apigen.Session
	bMe := call[apigen.SessionList](b, http.MethodGet, "/api/v1/me/sessions", nil, 200)
	for _, s := range bMe.Sessions {
		if s.Current {
			current++
			other = s
		}
		if s.Ip == nil || *s.Ip != "127.0.0.1" {
			t.Fatalf("session ip %v", s.Ip)
		}
	}
	if current != 1 {
		t.Fatalf("%d current sessions", current)
	}
	// a signs b out.
	call[struct{}](a, http.MethodDelete, "/api/v1/me/sessions/"+other.Id.String(), nil, 204)
	b.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "unauthenticated")
	a.expectProblem(http.MethodDelete, "/api/v1/me/sessions/"+other.Id.String(), nil, 404, "not_found")

	// Logout ends the session and clears the cookies.
	call[struct{}](a, http.MethodPost, "/api/v1/auth/logout", nil, 204)
	a.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "unauthenticated")
}

func TestChangePassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	other := h.login("admin", adminPassword)

	admin.expectProblem(http.MethodPost, "/api/v1/me/password",
		map[string]string{"current_password": "wrong one", "new_password": "a new password"}, 403, "invalid_credentials")
	p := admin.expectProblem(http.MethodPost, "/api/v1/me/password",
		map[string]string{"current_password": adminPassword, "new_password": "short"}, 400, CodeInvalidRequest)
	if len(*p.Errors) != 1 || !strings.Contains((*p.Errors)[0].Field, "new_password") {
		t.Fatalf("errors %+v", *p.Errors)
	}
	call[struct{}](admin, http.MethodPost, "/api/v1/me/password",
		map[string]string{"current_password": adminPassword, "new_password": "a new password"}, 204)

	// This session stays; the other one ends; the new password works.
	call[apigen.Me](admin, http.MethodGet, "/api/v1/me", nil, 200)
	other.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "unauthenticated")
	h.login("admin", "a new password")
}

func TestAPIKeys(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)

	// Users can't hold admin scopes.
	user.expectProblem(http.MethodPost, "/api/v1/me/api-keys", map[string]any{"name": "x", "scopes": []string{"admin"}}, 400, CodeInvalidRequest)
	user.expectProblem(http.MethodPost, "/api/v1/me/api-keys", map[string]any{"name": "x", "scopes": []string{"bogus"}}, 400, CodeInvalidRequest)
	user.expectProblem(http.MethodPost, "/api/v1/me/api-keys",
		map[string]any{"name": "x", "scopes": []string{"read"}, "expires_at": time.Now().Add(-time.Hour)}, 400, CodeInvalidRequest)

	created := call[apigen.ApiKeyCreated](user, http.MethodPost, "/api/v1/me/api-keys",
		map[string]any{"name": "reader app", "scopes": []string{"read", "read"}}, 201)
	if !strings.HasPrefix(created.Key, "pnl_") || !strings.HasPrefix(created.Key, created.ApiKey.Prefix) || len(created.ApiKey.Scopes) != 1 {
		t.Fatalf("created %+v", created)
	}
	// The key is never shown again.
	list := call[apigen.ApiKeyList](user, http.MethodGet, "/api/v1/me/api-keys", nil, 200)
	if len(list.ApiKeys) != 1 || strings.Contains(string(user.do(http.MethodGet, "/api/v1/me/api-keys", nil).body), created.Key) {
		t.Fatalf("list %+v", list)
	}

	ro := h.keyClient(created.Key)
	me := call[apigen.Me](ro, http.MethodGet, "/api/v1/me", nil, 200)
	if me.Auth.Method != apigen.MeAuthMethodApiKey || me.User.Username != "reader" {
		t.Fatalf("me %+v", me)
	}
	// Bearer works too.
	bearer := h.client()
	bearer.header.Set("Authorization", "Bearer "+created.Key)
	call[apigen.Me](bearer, http.MethodGet, "/api/v1/me", nil, 200)

	// A read key can't write, and keys can't manage keys or passwords.
	ro.expectProblem(http.MethodPatch, "/api/v1/me", map[string]any{"display_name": "x"}, 403, "insufficient_scope")
	rw := h.keyClient(user.newAPIKey("read", "write"))
	call[apigen.Me](rw, http.MethodPatch, "/api/v1/me", map[string]any{"display_name": "via key"}, 200)
	rw.expectProblem(http.MethodGet, "/api/v1/me/api-keys", nil, 403, "session_required")
	rw.expectProblem(http.MethodPost, "/api/v1/me/password", map[string]string{"current_password": "a", "new_password": "bbbbbbbbbb"}, 403, "session_required")

	// Admin endpoints need the admin scope on top of the admin role.
	adminRead := h.keyClient(admin.newAPIKey("read"))
	adminRead.expectProblem(http.MethodGet, "/api/v1/users", nil, 403, "insufficient_scope")
	adminFull := h.keyClient(admin.newAPIKey("read", "admin"))
	call[apigen.UserList](adminFull, http.MethodGet, "/api/v1/users", nil, 200)

	// Unknown and revoked keys are refused, not treated as anonymous.
	h.keyClient("pnl_nope").expectProblem(http.MethodGet, "/api/v1/system/info", nil, 401, "invalid_api_key")
	h.keyClient("not-a-key").expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "invalid_api_key")
	call[struct{}](user, http.MethodDelete, "/api/v1/me/api-keys/"+created.ApiKey.Id.String(), nil, 204)
	ro.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "invalid_api_key")
	// Users can't revoke other users' keys.
	adminKeys := call[apigen.ApiKeyList](admin, http.MethodGet, "/api/v1/me/api-keys", nil, 200)
	user.expectProblem(http.MethodDelete, "/api/v1/me/api-keys/"+adminKeys.ApiKeys[0].Id.String(), nil, 404, "not_found")
}

func TestAPIKeyExpiryAndDemotion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	_, secondUser := h.createUser(admin, "second", nil)
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+secondUser.Id.String(), map[string]any{"role": "admin"}, 200)
	second := h.login("second", "user password 1")
	key := h.keyClient(second.newAPIKey("read", "admin"))
	call[apigen.UserList](key, http.MethodGet, "/api/v1/users", nil, 200)

	// Demoted: the key keeps working but loses what the role no longer allows.
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+secondUser.Id.String(), map[string]any{"role": "user"}, 200)
	key.expectProblem(http.MethodGet, "/api/v1/users", nil, 403, "forbidden")
	me := call[apigen.Me](key, http.MethodGet, "/api/v1/me", nil, 200)
	if len(me.Auth.Scopes) != 1 || me.Auth.Scopes[0] != apigen.ApiKeyScopeRead {
		t.Fatalf("scopes after demotion %v", me.Auth.Scopes)
	}

	// Expired keys stop working.
	created := call[apigen.ApiKeyCreated](admin, http.MethodPost, "/api/v1/me/api-keys",
		map[string]any{"name": "short", "scopes": []string{"read"}, "expires_at": time.Now().Add(1500 * time.Millisecond)}, 201)
	short := h.keyClient(created.Key)
	call[apigen.Me](short, http.MethodGet, "/api/v1/me", nil, 200)
	time.Sleep(1600 * time.Millisecond)
	short.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "invalid_api_key")
}
