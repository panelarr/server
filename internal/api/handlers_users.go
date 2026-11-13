// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/users"
)

// Page sizes.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

func pageSize(p *int) int {
	switch {
	case p == nil:
		return defaultPageSize
	case *p > maxPageSize:
		return maxPageSize
	case *p < 1:
		return 1
	}
	return *p
}

// Page tokens are opaque to clients: base64url of the last key seen.
func encodePageToken(cursor string) string {
	if cursor == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

func decodePageToken(tok *string) (string, error) {
	if tok == nil || *tok == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(*tok)
	if err != nil {
		return "", fieldProblem("page_token", "not a token from this API")
	}
	return string(b), nil
}

// ListUsers lists users by username.
func (s *Server) ListUsers(ctx context.Context, req apigen.ListUsersRequestObject) (apigen.ListUsersResponseObject, error) {
	after, err := decodePageToken(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	rows, next, err := s.deps.Users.ListUsers(ctx, after, pageSize(req.Params.PageSize))
	if err != nil {
		return nil, err
	}
	out := apigen.ListUsers200JSONResponse{Users: make([]apigen.User, len(rows)), NextPageToken: encodePageToken(next)}
	for i, u := range rows {
		out.Users[i] = toUser(u)
	}
	if err := s.withSSOMarks(ctx, out.Users); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateUser creates a user with a password.
func (s *Server) CreateUser(ctx context.Context, req apigen.CreateUserRequestObject) (apigen.CreateUserResponseObject, error) {
	b := req.Body
	role := auth.RoleUser
	if b.Role != nil {
		role = string(*b.Role)
	}
	u, err := s.deps.Users.CreateUser(ctx, auth.FromContext(ctx).User.ID, users.NewUser{
		Username: b.Username, Password: b.Password, DisplayName: deref(b.DisplayName), Role: role,
		Grants: fromGrantsPatch(b.Grants).Apply(users.DefaultGrants()),
	})
	if err != nil {
		return nil, domainError(err)
	}
	out, err := s.userWithSSO(ctx, u)
	if err != nil {
		return nil, err
	}
	return apigen.CreateUser201JSONResponse(out), nil
}

// GetUser returns a user.
func (s *Server) GetUser(ctx context.Context, req apigen.GetUserRequestObject) (apigen.GetUserResponseObject, error) {
	u, err := s.deps.Users.GetUser(ctx, req.Id)
	if err != nil {
		return nil, domainError(err)
	}
	out, err := s.userWithSSO(ctx, u)
	if err != nil {
		return nil, err
	}
	return apigen.GetUser200JSONResponse(out), nil
}

// UpdateUser changes a user.
func (s *Server) UpdateUser(ctx context.Context, req apigen.UpdateUserRequestObject) (apigen.UpdateUserResponseObject, error) {
	b := req.Body
	patch := users.Patch{DisplayName: b.DisplayName, Disabled: b.Disabled, Password: b.Password, Grants: fromGrantsPatch(b.Grants)}
	if b.Role != nil {
		r := string(*b.Role)
		patch.Role = &r
	}
	u, err := s.deps.Users.UpdateUser(ctx, auth.FromContext(ctx).User.ID, req.Id, patch)
	if err != nil {
		return nil, domainError(err)
	}
	out, err := s.userWithSSO(ctx, u)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateUser200JSONResponse(out), nil
}

// DeleteUser deletes a user.
func (s *Server) DeleteUser(ctx context.Context, req apigen.DeleteUserRequestObject) (apigen.DeleteUserResponseObject, error) {
	if err := s.deps.Users.DeleteUser(ctx, auth.FromContext(ctx).User.ID, req.Id); err != nil {
		return nil, domainError(err)
	}
	return apigen.DeleteUser204Response{}, nil
}

// ListInvites lists open invites.
func (s *Server) ListInvites(ctx context.Context, _ apigen.ListInvitesRequestObject) (apigen.ListInvitesResponseObject, error) {
	rows, err := s.deps.Users.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListInvites200JSONResponse{Invites: make([]apigen.Invite, len(rows))}
	for i, r := range rows {
		if out.Invites[i], err = toInvite(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateInvite creates an invite.
func (s *Server) CreateInvite(ctx context.Context, req apigen.CreateInviteRequestObject) (apigen.CreateInviteResponseObject, error) {
	b := req.Body
	n := users.NewInvite{Role: auth.RoleUser, Grants: fromGrantsPatch(b.Grants).Apply(users.DefaultGrants()), Note: deref(b.Note)}
	if b.Role != nil {
		n.Role = string(*b.Role)
	}
	if b.ExpiresInHours != nil {
		n.TTL = time.Duration(*b.ExpiresInHours) * time.Hour
	}
	inv, token, err := s.deps.Users.CreateInvite(ctx, auth.FromContext(ctx).User.ID, n)
	if err != nil {
		return nil, domainError(err)
	}
	out, err := toInvite(inv)
	if err != nil {
		return nil, err
	}
	return apigen.CreateInvite201JSONResponse{Invite: out, Token: token}, nil
}

// DeleteInvite revokes an invite.
func (s *Server) DeleteInvite(ctx context.Context, req apigen.DeleteInviteRequestObject) (apigen.DeleteInviteResponseObject, error) {
	if err := s.deps.Users.DeleteInvite(ctx, auth.FromContext(ctx).User.ID, req.Id); err != nil {
		return nil, domainError(err)
	}
	return apigen.DeleteInvite204Response{}, nil
}
