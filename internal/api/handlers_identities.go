// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/config"
	"github.com/panelarr/server/internal/store/db"
)

func toIdentity(i db.UserIdentity) apigen.Identity {
	return apigen.Identity{
		Id: i.ID, Issuer: i.Issuer, Subject: i.Subject, Email: i.Email, CreatedAt: i.CreatedAt, LastLoginAt: i.LastLoginAt,
	}
}

func (s *Server) identities(ctx context.Context, userID uuid.UUID) (apigen.IdentityList, error) {
	rows, err := s.deps.Users.ListIdentities(ctx, userID)
	if err != nil {
		return apigen.IdentityList{}, err
	}
	out := apigen.IdentityList{Identities: make([]apigen.Identity, len(rows))}
	for i, r := range rows {
		out.Identities[i] = toIdentity(r)
	}
	return out, nil
}

// ListMyIdentities lists the caller's linked logins.
func (s *Server) ListMyIdentities(ctx context.Context, _ apigen.ListMyIdentitiesRequestObject) (apigen.ListMyIdentitiesResponseObject, error) {
	out, err := s.identities(ctx, auth.FromContext(ctx).User.ID)
	if err != nil {
		return nil, err
	}
	return apigen.ListMyIdentities200JSONResponse(out), nil
}

// DeleteMyIdentity unlinks one of the caller's logins, unless it is their
// last way to sign in.
func (s *Server) DeleteMyIdentity(ctx context.Context, req apigen.DeleteMyIdentityRequestObject) (apigen.DeleteMyIdentityResponseObject, error) {
	me := auth.FromContext(ctx).User.ID
	if err := s.deps.Users.Unlink(ctx, me, me, req.Id, false); err != nil {
		return nil, domainError(err)
	}
	return apigen.DeleteMyIdentity204Response{}, nil
}

// ListUserIdentities lists a user's linked logins (admins).
func (s *Server) ListUserIdentities(ctx context.Context, req apigen.ListUserIdentitiesRequestObject) (apigen.ListUserIdentitiesResponseObject, error) {
	if _, err := s.deps.Users.GetUser(ctx, req.Id); err != nil {
		return nil, domainError(err)
	}
	out, err := s.identities(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return apigen.ListUserIdentities200JSONResponse(out), nil
}

// DeleteUserIdentity unlinks a user's login and ends their single sign-on
// sessions (admins).
func (s *Server) DeleteUserIdentity(ctx context.Context, req apigen.DeleteUserIdentityRequestObject) (apigen.DeleteUserIdentityResponseObject, error) {
	if err := s.deps.Users.Unlink(ctx, auth.FromContext(ctx).User.ID, req.Id, req.IdentityId, true); err != nil {
		return nil, domainError(err)
	}
	return apigen.DeleteUserIdentity204Response{}, nil
}

// systemAuthCheckWait bounds the reachability check on the status page.
const systemAuthCheckWait = 5 * time.Second

// GetSystemAuth shows how sign-in is configured. Never the client secret.
func (s *Server) GetSystemAuth(ctx context.Context, _ apigen.GetSystemAuthRequestObject) (apigen.GetSystemAuthResponseObject, error) {
	out := apigen.GetSystemAuth200JSONResponse{Password: apigen.SystemAuthPasswordAll}
	if s.deps.SSO == nil {
		return out, nil
	}
	if s.deps.Auth.PasswordAdminsOnly() {
		out.Password = apigen.SystemAuthPasswordAdmins
	}
	c := s.deps.SSO.Settings()
	cctx, cancel := context.WithTimeout(ctx, systemAuthCheckWait)
	defer cancel()
	checkErr := s.deps.SSO.Check(cctx)
	logout := apigen.OidcStatusLogout(config.LogoutLocal)
	if c.ProviderLogout {
		logout = apigen.OidcStatusLogout(config.LogoutProvider)
	}
	out.Oidc = apigen.OidcStatus{
		Enabled: true, Issuer: &c.Issuer, ClientId: &c.ClientID, PublicClient: ptr(s.deps.SSO.PublicClient()),
		Reachable: ptr(checkErr == nil), Scopes: &c.Scopes, ButtonLabel: &c.Label,
		UsernameClaim: &c.UsernameClaim, GroupsClaim: &c.GroupsClaim, AutoProvision: &c.AutoProvision,
		AllowedGroups: ptr(nonNil(c.AllowedGroups)), AdminGroups: ptr(nonNil(c.AdminGroups)), Logout: &logout,
	}
	if checkErr != nil {
		msg := checkErr.Error()
		out.Oidc.Error = &msg
	}
	return out, nil
}

// withSSOMarks sets each user's sso flag.
func (s *Server) withSSOMarks(ctx context.Context, us []apigen.User) error {
	ids := make([]uuid.UUID, len(us))
	for i, u := range us {
		ids[i] = u.Id
	}
	linked, err := s.deps.Users.LinkedUsers(ctx, ids)
	if err != nil {
		return err
	}
	for i := range us {
		us[i].Sso = ptr(linked[us[i].Id])
	}
	return nil
}

// userWithSSO is toUser with the sso flag.
func (s *Server) userWithSSO(ctx context.Context, u db.User) (apigen.User, error) {
	out := []apigen.User{toUser(u)}
	err := s.withSSOMarks(ctx, out)
	return out[0], err
}
