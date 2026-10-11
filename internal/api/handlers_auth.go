// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/users"
)

// Login signs a user in with a password.
func (s *Server) Login(ctx context.Context, req apigen.LoginRequestObject) (apigen.LoginResponseObject, error) {
	u, sess, err := s.deps.Auth.Login(ctx, req.Body.Username, req.Body.Password, s.clientInfo(ctx))
	var rl *auth.RateLimitedError
	if errors.As(err, &rl) {
		if w := responseWriter(ctx); w != nil {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(rl.RetryAfter.Seconds()))))
		}
	}
	if err != nil {
		return nil, domainError(err)
	}
	s.setSessionCookies(ctx, sess)
	return apigen.Login200JSONResponse{User: toUser(u), CsrfToken: sess.CSRFToken}, nil
}

// Logout ends the current session.
func (s *Server) Logout(ctx context.Context, _ apigen.LogoutRequestObject) (apigen.LogoutResponseObject, error) {
	if err := s.deps.Auth.Logout(ctx, auth.FromContext(ctx)); err != nil {
		return nil, err
	}
	s.clearSessionCookies(ctx)
	return apigen.Logout204Response{}, nil
}

// CheckInvite says whether an invite token is valid.
func (s *Server) CheckInvite(ctx context.Context, req apigen.CheckInviteRequestObject) (apigen.CheckInviteResponseObject, error) {
	inv, err := s.deps.Users.CheckInvite(ctx, req.Body.Token)
	if err != nil {
		return nil, domainError(err)
	}
	return apigen.CheckInvite200JSONResponse{Role: apigen.Role(inv.Role), ExpiresAt: inv.ExpiresAt}, nil
}

// AcceptInvite creates an account from an invite and signs it in.
func (s *Server) AcceptInvite(ctx context.Context, req apigen.AcceptInviteRequestObject) (apigen.AcceptInviteResponseObject, error) {
	b := req.Body
	u, sess, err := s.deps.Users.AcceptInvite(ctx, b.Token, users.NewUser{
		Username: b.Username, Password: b.Password, DisplayName: deref(b.DisplayName),
	}, s.clientInfo(ctx))
	if err != nil {
		return nil, domainError(err)
	}
	s.setSessionCookies(ctx, sess)
	return apigen.AcceptInvite201JSONResponse{User: toUser(u), CsrfToken: sess.CSRFToken}, nil
}
