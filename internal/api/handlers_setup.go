// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/users"
)

func toSetup(st users.SetupStatus) apigen.SetupStatus {
	return apigen.SetupStatus{AdminCreated: st.AdminCreated, Completed: st.CompletedAt != nil, CompletedAt: st.CompletedAt}
}

// GetSetup reports the first-run wizard's state.
func (s *Server) GetSetup(ctx context.Context, _ apigen.GetSetupRequestObject) (apigen.GetSetupResponseObject, error) {
	st, err := s.deps.Users.SetupStatus(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.GetSetup200JSONResponse(toSetup(st)), nil
}

// CreateSetupAdmin creates the first account and signs it in.
func (s *Server) CreateSetupAdmin(ctx context.Context, req apigen.CreateSetupAdminRequestObject) (apigen.CreateSetupAdminResponseObject, error) {
	b := req.Body
	u, sess, err := s.deps.Users.CreateFirstAdmin(ctx, users.NewUser{
		Username: b.Username, Password: b.Password, DisplayName: deref(b.DisplayName),
	}, s.clientInfo(ctx))
	if err != nil {
		return nil, domainError(err)
	}
	s.setSessionCookies(ctx, sess)
	return apigen.CreateSetupAdmin201JSONResponse{User: toUser(u), CsrfToken: sess.CSRFToken}, nil
}

// CompleteSetup finishes the wizard.
func (s *Server) CompleteSetup(ctx context.Context, _ apigen.CompleteSetupRequestObject) (apigen.CompleteSetupResponseObject, error) {
	st, err := s.deps.Users.CompleteSetup(ctx, auth.FromContext(ctx).User.ID)
	if err != nil {
		return nil, err
	}
	return apigen.CompleteSetup200JSONResponse(toSetup(st)), nil
}

func (s *Server) clientInfo(ctx context.Context) auth.ClientInfo {
	ua, _ := ctx.Value(userAgentKey).(string)
	return auth.ClientInfo{IP: ClientIP(ctx), UserAgent: ua}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
