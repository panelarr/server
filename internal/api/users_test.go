// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestUsersAdmin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, me := h.setupAdmin()

	// Defaults: add and read, no downloads, no match fixing, suggestive.
	_, u := h.createUser(admin, "Reader", nil)
	g := u.Grants
	if u.Role != apigen.RoleUser || !g.MayAdd || g.MayDownload || g.MayManageMatches || g.ContentRatingCeiling != apigen.ContentRatingSuggestive || !g.AllSources || len(g.AllowedSources) != 0 {
		t.Fatalf("defaults %+v", u)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/users", map[string]any{"username": "READER", "password": "another pass"}, 409, "username_taken")

	// Validation comes from the spec.
	p := admin.expectProblem(http.MethodPost, "/api/v1/users",
		map[string]any{"username": "-bad name", "password": "x", "role": "god", "grants": map[string]any{"content_rating_ceiling": "extreme"}}, 400, CodeInvalidRequest)
	fields := map[string]bool{}
	for _, f := range *p.Errors {
		fields[f.Field] = true
	}
	for _, want := range []string{"username", "password", "role", "grants.content_rating_ceiling"} {
		if !fields[want] {
			t.Errorf("no error for %s: %+v", want, *p.Errors)
		}
	}

	// Grants change field by field; allowed sources are sorted and unique.
	upd := call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+u.Id.String(), map[string]any{
		"display_name": "Rita",
		"grants": map[string]any{
			"may_download": true, "content_rating_ceiling": "erotic",
			"all_sources": false, "allowed_sources": []string{"p:2", "p:1", "p:2"},
		},
	}, 200)
	g = upd.Grants
	if upd.DisplayName != "Rita" || !g.MayAdd || !g.MayDownload || g.ContentRatingCeiling != apigen.ContentRatingErotic || g.AllSources ||
		strings.Join(g.AllowedSources, ",") != "p:1,p:2" {
		t.Fatalf("updated %+v", upd)
	}
	got := call[apigen.User](admin, http.MethodGet, "/api/v1/users/"+u.Id.String(), nil, 200)
	if got.DisplayName != "Rita" || got.LastLoginAt == nil {
		t.Fatalf("get %+v", got)
	}

	// Admins can't lock themselves out, and the last admin stays.
	admin.expectProblem(http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"role": "user"}, 409, "cannot_change_self")
	admin.expectProblem(http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"disabled": true}, 409, "cannot_change_self")
	admin.expectProblem(http.MethodDelete, "/api/v1/users/"+me.Id.String(), nil, 409, "cannot_change_self")
	// ...but may change their own name.
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"display_name": "Root"}, 200)

	// A second admin can't remove the first if it's the last enabled one.
	call[apigen.User](admin, http.MethodPatch, "/api/v1/users/"+u.Id.String(), map[string]any{"role": "admin"}, 200)
	second := h.login("reader", "user password 1")
	call[apigen.User](second, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"role": "user"}, 200)
	admin.expectProblem(http.MethodGet, "/api/v1/users", nil, 403, "forbidden")
	second.expectProblem(http.MethodPatch, "/api/v1/users/"+u.Id.String(), map[string]any{"disabled": true}, 409, "cannot_change_self")

	// Password reset by an admin ends the user's sessions.
	call[apigen.User](second, http.MethodPatch, "/api/v1/users/"+me.Id.String(), map[string]any{"password": "reset password"}, 200)
	admin.expectProblem(http.MethodGet, "/api/v1/me", nil, 401, "unauthenticated")
	h.login("admin", "reset password")

	// Deleting a user.
	call[struct{}](second, http.MethodDelete, "/api/v1/users/"+me.Id.String(), nil, 204)
	second.expectProblem(http.MethodGet, "/api/v1/users/"+me.Id.String(), nil, 404, "not_found")
	second.expectProblem(http.MethodDelete, "/api/v1/users/"+me.Id.String(), nil, 404, "not_found")
}

func TestUsersPagination(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	for i := range 5 {
		call[apigen.User](admin, http.MethodPost, "/api/v1/users",
			map[string]any{"username": fmt.Sprintf("user%d", i), "password": "a long password"}, 201)
	}
	var names []string
	tok := ""
	for pages := 0; ; pages++ {
		q := url.Values{"page_size": {"2"}}
		if tok != "" {
			q.Set("page_token", tok)
		}
		page := call[apigen.UserList](admin, http.MethodGet, "/api/v1/users?"+q.Encode(), nil, 200)
		for _, u := range page.Users {
			names = append(names, u.Username)
		}
		tok = page.NextPageToken
		if tok == "" || pages > 5 {
			break
		}
	}
	if strings.Join(names, ",") != "Admin,user0,user1,user2,user3,user4" {
		t.Fatalf("pages: %v", names)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/users?page_size=0", nil, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodGet, "/api/v1/users?page_token=%25%25", nil, 400, CodeInvalidRequest)
}

func TestInvites(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	anon := h.client()

	created := call[apigen.InviteCreated](admin, http.MethodPost, "/api/v1/invites", map[string]any{
		"note": "for my friend", "expires_in_hours": 24,
		"grants": map[string]any{"may_download": true, "content_rating_ceiling": "safe"},
	}, 201)
	if created.Token == "" || created.Invite.Role != apigen.RoleUser || !created.Invite.Grants.MayDownload || created.Invite.Grants.ContentRatingCeiling != apigen.ContentRatingSafe {
		t.Fatalf("created %+v", created)
	}
	list := call[apigen.InviteList](admin, http.MethodGet, "/api/v1/invites", nil, 200)
	if len(list.Invites) != 1 || list.Invites[0].Note != "for my friend" || strings.Contains(fmt.Sprint(list), created.Token) {
		t.Fatalf("list %+v", list)
	}

	check := call[apigen.InviteCheck](anon, http.MethodPost, "/api/v1/auth/invites/check", map[string]string{"token": created.Token}, 200)
	if check.Role != apigen.RoleUser {
		t.Fatalf("check %+v", check)
	}
	anon.expectProblem(http.MethodPost, "/api/v1/auth/invites/check", map[string]string{"token": "nope"}, 404, "invite_invalid")

	// Taking a used name keeps the invite.
	anon.expectProblem(http.MethodPost, "/api/v1/auth/invites/accept",
		map[string]string{"token": created.Token, "username": "admin", "password": "friend password"}, 409, "username_taken")
	friend := h.client()
	res := call[apigen.LoginResponse](friend, http.MethodPost, "/api/v1/auth/invites/accept",
		map[string]string{"token": created.Token, "username": "friend", "password": "friend password"}, 201)
	friend.csrf = res.CsrfToken
	if !res.User.Grants.MayDownload || res.User.Grants.ContentRatingCeiling != apigen.ContentRatingSafe {
		t.Fatalf("friend %+v", res.User)
	}
	call[apigen.Me](friend, http.MethodGet, "/api/v1/me", nil, 200)

	// Used up.
	anon.expectProblem(http.MethodPost, "/api/v1/auth/invites/accept",
		map[string]string{"token": created.Token, "username": "friend2", "password": "friend password"}, 404, "invite_invalid")
	if l := call[apigen.InviteList](admin, http.MethodGet, "/api/v1/invites", nil, 200); len(l.Invites) != 0 {
		t.Fatalf("used invite still listed: %+v", l)
	}

	// Revoked.
	revoked := call[apigen.InviteCreated](admin, http.MethodPost, "/api/v1/invites", map[string]any{"role": "admin"}, 201)
	call[struct{}](admin, http.MethodDelete, "/api/v1/invites/"+revoked.Invite.Id.String(), nil, 204)
	anon.expectProblem(http.MethodPost, "/api/v1/auth/invites/check", map[string]string{"token": revoked.Token}, 404, "invite_invalid")
	admin.expectProblem(http.MethodDelete, "/api/v1/invites/"+revoked.Invite.Id.String(), nil, 404, "not_found")
}
