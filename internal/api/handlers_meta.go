// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta"
)

// metaError maps metadata service errors to problems.
func metaError(err error) error {
	var bad *meta.BadFilterError
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &bad):
		return fieldProblem(bad.Field, "unknown value "+bad.Value)
	case errors.Is(err, meta.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such entry")
	case errors.Is(err, meta.ErrNotConfigured):
		return NewProblem(http.StatusServiceUnavailable, "meta_unavailable", "the metadata service isn't connected yet; an admin connects it in the setup")
	case errors.Is(err, meta.ErrManagedByEnv):
		return NewProblem(http.StatusConflict, "managed_by_env", "the connection is set by PANELARR_META_URL")
	case errors.Is(err, meta.ErrInvalidURL):
		return fieldProblem("url", "must be http(s)://host[:port]")
	case errors.As(err, &ce):
		switch ce.Code() {
		case connect.CodeNotFound:
			return NewProblem(http.StatusNotFound, CodeNotFound, ce.Message())
		case connect.CodeAlreadyExists, connect.CodeFailedPrecondition:
			return NewProblem(http.StatusConflict, "meta_conflict", ce.Message())
		case connect.CodeInvalidArgument:
			return NewProblem(http.StatusBadRequest, CodeInvalidRequest, ce.Message())
		}
		return NewProblem(http.StatusServiceUnavailable, "meta_unavailable", "the metadata service didn't answer: "+ce.Code().String())
	}
	return err
}

func toMetaConnection(st meta.Status) apigen.MetaConnection {
	c := st.Check
	out := apigen.MetaConnection{
		ManagedByEnv: st.ManagedByEnv,
		Check: apigen.MetaCheck{
			Configured: c.Configured, Reachable: c.Reachable, KeyValid: c.KeyValid, AdminScope: c.AdminScope,
			Ready: c.Ready, CheckedAt: c.CheckedAt, Providers: make([]apigen.MetaProvider, len(c.Providers)),
		},
	}
	if st.URL != "" {
		out.Url = ptr(st.URL)
	}
	if c.Problem != "" {
		out.Check.Problem = ptr(c.Problem)
	}
	for i, p := range c.Providers {
		out.Check.Providers[i] = apigen.MetaProvider{Id: p.ID, Health: p.Health, Records: p.Records}
		if p.Message != "" {
			out.Check.Providers[i].Message = ptr(p.Message)
		}
	}
	return out
}

// GetMetaConnection shows the connection and its state.
func (s *Server) GetMetaConnection(ctx context.Context, _ apigen.GetMetaConnectionRequestObject) (apigen.GetMetaConnectionResponseObject, error) {
	return apigen.GetMetaConnection200JSONResponse(toMetaConnection(s.deps.Meta.Status(ctx))), nil
}

// SetMetaConnection tests and saves the connection.
func (s *Server) SetMetaConnection(ctx context.Context, req apigen.SetMetaConnectionRequestObject) (apigen.SetMetaConnectionResponseObject, error) {
	check, err := s.deps.Meta.Save(ctx, meta.Conn{URL: req.Body.Url, APIKey: deref(req.Body.ApiKey)})
	if errors.Is(err, meta.ErrCheckFailed) {
		return nil, NewProblem(http.StatusUnprocessableEntity, "meta_check_failed", check.Problem)
	}
	if err != nil {
		return nil, metaError(err)
	}
	st := s.deps.Meta.Status(ctx)
	st.Check = check
	return apigen.SetMetaConnection200JSONResponse(toMetaConnection(st)), nil
}

// CheckMetaConnection checks the connection again now.
func (s *Server) CheckMetaConnection(ctx context.Context, _ apigen.CheckMetaConnectionRequestObject) (apigen.CheckMetaConnectionResponseObject, error) {
	s.deps.Meta.Check(ctx, true)
	return apigen.CheckMetaConnection200JSONResponse(toMetaConnection(s.deps.Meta.Status(ctx))), nil
}

// RunMetaProviderSync starts a provider sync in the metadata service.
func (s *Server) RunMetaProviderSync(ctx context.Context, req apigen.RunMetaProviderSyncRequestObject) (apigen.RunMetaProviderSyncResponseObject, error) {
	kind := "dump"
	if req.Body != nil && req.Body.Kind != nil {
		kind = string(*req.Body.Kind)
	}
	id, err := s.deps.Meta.RunSync(ctx, req.Provider, kind)
	if err != nil {
		return nil, metaError(err)
	}
	return apigen.RunMetaProviderSync202JSONResponse{RunId: id}, nil
}
