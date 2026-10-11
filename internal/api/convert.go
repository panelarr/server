// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"slices"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

func toGrants(g users.Grants) apigen.Grants {
	return apigen.Grants{
		MayAdd: g.MayAdd, MayDownload: g.MayDownload, MayManageMatches: g.MayManageMatches,
		ContentRatingCeiling: apigen.ContentRating(g.ContentRatingCeiling),
		AllSources:           g.AllSources, AllowedSources: slices.Clone(g.AllowedSources),
	}
}

func fromGrantsPatch(p *apigen.GrantsPatch) *users.GrantsPatch {
	if p == nil {
		return nil
	}
	out := &users.GrantsPatch{
		MayAdd: p.MayAdd, MayDownload: p.MayDownload, MayManageMatches: p.MayManageMatches,
		AllSources: p.AllSources, AllowedSources: p.AllowedSources,
	}
	if p.ContentRatingCeiling != nil {
		c := string(*p.ContentRatingCeiling)
		out.ContentRatingCeiling = &c
	}
	return out
}

// toUser shows a user. Grants are the stored ones: admins may do
// everything anyway.
func toUser(u db.User) apigen.User {
	return apigen.User{
		Id: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: apigen.Role(u.Role),
		Disabled: u.Disabled, Grants: toGrants(users.GrantsOf(u)), CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
}

func toScopes(s []string) []apigen.ApiKeyScope {
	out := make([]apigen.ApiKeyScope, len(s))
	for i, v := range s {
		out[i] = apigen.ApiKeyScope(v)
	}
	return out
}

func fromScopes(s []apigen.ApiKeyScope) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}

func toAPIKey(k db.ApiKey) apigen.ApiKey {
	return apigen.ApiKey{
		Id: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: toScopes(k.Scopes),
		CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, ExpiresAt: k.ExpiresAt,
	}
}

func toSession(s db.Session, current *auth.Principal) apigen.Session {
	out := apigen.Session{
		Id: s.ID, Current: current != nil && current.SessionID == s.ID,
		CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, ExpiresAt: s.ExpiresAt,
	}
	if s.UserAgent != "" {
		out.UserAgent = ptr(s.UserAgent)
	}
	if s.Ip != "" {
		out.Ip = ptr(s.Ip)
	}
	return out
}

func toInvite(inv db.Invite) (apigen.Invite, error) {
	g, err := users.InviteGrants(inv)
	if err != nil {
		return apigen.Invite{}, err
	}
	return apigen.Invite{
		Id: inv.ID, Role: apigen.Role(inv.Role), Grants: toGrants(g), Note: inv.Note,
		CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
	}, nil
}
