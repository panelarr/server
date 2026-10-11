// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/users"
)

func toMe(p *auth.Principal) apigen.Me {
	var me apigen.Me
	me.User = toUser(p.User)
	me.Auth.Method = apigen.MeAuthMethod(p.Method)
	me.Auth.Scopes = toScopes(p.Scopes)
	return me
}

// GetMe returns the caller.
func (s *Server) GetMe(ctx context.Context, _ apigen.GetMeRequestObject) (apigen.GetMeResponseObject, error) {
	return apigen.GetMe200JSONResponse(toMe(auth.FromContext(ctx))), nil
}

// UpdateMe changes the caller's profile.
func (s *Server) UpdateMe(ctx context.Context, req apigen.UpdateMeRequestObject) (apigen.UpdateMeResponseObject, error) {
	p := auth.FromContext(ctx)
	u, err := s.deps.Users.UpdateUser(ctx, p.User.ID, p.User.ID, users.Patch{DisplayName: req.Body.DisplayName})
	if err != nil {
		return nil, domainError(err)
	}
	updated := *p
	updated.User = u
	return apigen.UpdateMe200JSONResponse(toMe(&updated)), nil
}

// ChangeMyPassword changes the caller's password.
func (s *Server) ChangeMyPassword(ctx context.Context, req apigen.ChangeMyPasswordRequestObject) (apigen.ChangeMyPasswordResponseObject, error) {
	err := s.deps.Auth.ChangePassword(ctx, auth.FromContext(ctx), req.Body.CurrentPassword, req.Body.NewPassword)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		// 403, not 401: the caller is signed in; only the check failed.
		return nil, NewProblem(http.StatusForbidden, "invalid_credentials", "the current password is wrong")
	}
	if err != nil {
		return nil, domainError(err)
	}
	return apigen.ChangeMyPassword204Response{}, nil
}

// ListMySessions lists the caller's sessions.
func (s *Server) ListMySessions(ctx context.Context, _ apigen.ListMySessionsRequestObject) (apigen.ListMySessionsResponseObject, error) {
	p := auth.FromContext(ctx)
	rows, err := s.deps.Auth.ListSessions(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	out := apigen.ListMySessions200JSONResponse{Sessions: make([]apigen.Session, len(rows))}
	for i, r := range rows {
		out.Sessions[i] = toSession(r, p)
	}
	return out, nil
}

// DeleteMySession ends one of the caller's sessions.
func (s *Server) DeleteMySession(ctx context.Context, req apigen.DeleteMySessionRequestObject) (apigen.DeleteMySessionResponseObject, error) {
	p := auth.FromContext(ctx)
	if err := s.deps.Auth.DeleteSession(ctx, p.User.ID, req.Id); err != nil {
		return nil, domainError(err)
	}
	if req.Id == p.SessionID {
		s.clearSessionCookies(ctx)
	}
	return apigen.DeleteMySession204Response{}, nil
}

// ListMyApiKeys lists the caller's API keys.
func (s *Server) ListMyApiKeys(ctx context.Context, _ apigen.ListMyApiKeysRequestObject) (apigen.ListMyApiKeysResponseObject, error) {
	rows, err := s.deps.Auth.ListAPIKeys(ctx, auth.FromContext(ctx).User.ID)
	if err != nil {
		return nil, err
	}
	out := apigen.ListMyApiKeys200JSONResponse{ApiKeys: make([]apigen.ApiKey, len(rows))}
	for i, r := range rows {
		out.ApiKeys[i] = toAPIKey(r)
	}
	return out, nil
}

// CreateMyApiKey creates an API key for the caller.
func (s *Server) CreateMyApiKey(ctx context.Context, req apigen.CreateMyApiKeyRequestObject) (apigen.CreateMyApiKeyResponseObject, error) {
	b := req.Body
	k, key, err := s.deps.Auth.CreateAPIKey(ctx, auth.FromContext(ctx).User, b.Name, fromScopes(b.Scopes), b.ExpiresAt)
	if err != nil {
		return nil, domainError(err)
	}
	return apigen.CreateMyApiKey201JSONResponse{ApiKey: toAPIKey(k), Key: key}, nil
}

// DeleteMyApiKey revokes one of the caller's API keys.
func (s *Server) DeleteMyApiKey(ctx context.Context, req apigen.DeleteMyApiKeyRequestObject) (apigen.DeleteMyApiKeyResponseObject, error) {
	if err := s.deps.Auth.DeleteAPIKey(ctx, auth.FromContext(ctx).User.ID, req.Id); err != nil {
		return nil, domainError(err)
	}
	return apigen.DeleteMyApiKey204Response{}, nil
}
