// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/network"
)

// networkError maps the network service's errors.
func networkError(err error) error {
	var bad *network.InvalidError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, network.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, network.ErrIDTaken):
		return NewProblem(http.StatusConflict, "id_taken", "this ID is taken; choose another")
	case errors.Is(err, network.ErrInvalidID):
		return fieldProblem("id", err.Error())
	case errors.Is(err, network.ErrInUse):
		return NewProblem(http.StatusConflict, "in_use", err.Error())
	case errors.As(err, &bad):
		return fieldProblem(bad.Field, bad.Reason)
	}
	return err
}

func secondsPtr(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	return ptr(int(d / time.Second))
}

func durationOf(seconds *int) time.Duration {
	if seconds == nil {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}

func toSolver(s network.Solver) apigen.Solver {
	return apigen.Solver{
		Id: s.ID, Name: s.Name, Type: apigen.SolverType(s.Type), Url: s.URL,
		SessionTtlSeconds: secondsPtr(s.SessionTTL), MaxTimeoutSeconds: secondsPtr(s.MaxTimeout),
		AsResponseFallback: s.AsResponseFallback, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

func solverInput(b apigen.NewSolver) network.Solver {
	return network.Solver{
		ID: b.Id, Name: b.Name, Type: string(b.Type), URL: b.Url,
		SessionTTL: durationOf(b.SessionTtlSeconds), MaxTimeout: durationOf(b.MaxTimeoutSeconds), AsResponseFallback: deref(b.AsResponseFallback),
	}
}

func toNetworkTest(r network.TestResult) apigen.NetworkTest {
	return apigen.NetworkTest{Ok: r.OK, Message: r.Message, Version: nonEmpty(r.Version), LatencyMs: int(r.Latency.Milliseconds())}
}

// ListSolvers lists the solvers.
func (s *Server) ListSolvers(ctx context.Context, _ apigen.ListSolversRequestObject) (apigen.ListSolversResponseObject, error) {
	list, err := s.deps.Network.Solvers(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListSolvers200JSONResponse{Solvers: make([]apigen.Solver, len(list))}
	for i, v := range list {
		out.Solvers[i] = toSolver(v)
	}
	return out, nil
}

// CreateSolver adds a solver.
func (s *Server) CreateSolver(ctx context.Context, req apigen.CreateSolverRequestObject) (apigen.CreateSolverResponseObject, error) {
	v, err := s.deps.Network.CreateSolver(ctx, solverInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.CreateSolver201JSONResponse(toSolver(v)), nil
}

// TestSolverSettings checks an unsaved solver.
func (s *Server) TestSolverSettings(ctx context.Context, req apigen.TestSolverSettingsRequestObject) (apigen.TestSolverSettingsResponseObject, error) {
	r, err := s.deps.Network.TestSolver(ctx, solverInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestSolverSettings200JSONResponse(toNetworkTest(r)), nil
}

// GetSolver returns a solver.
func (s *Server) GetSolver(ctx context.Context, req apigen.GetSolverRequestObject) (apigen.GetSolverResponseObject, error) {
	v, err := s.deps.Network.Solver(ctx, req.SolverId)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.GetSolver200JSONResponse(toSolver(v)), nil
}

// UpdateSolver changes a solver.
func (s *Server) UpdateSolver(ctx context.Context, req apigen.UpdateSolverRequestObject) (apigen.UpdateSolverResponseObject, error) {
	b := req.Body
	p := network.SolverPatch{Name: b.Name, URL: b.Url, AsResponseFallback: b.AsResponseFallback}
	if b.Type != nil {
		p.Type = ptr(string(*b.Type))
	}
	if b.SessionTtlSeconds != nil {
		p.SessionTTL = ptr(durationOf(b.SessionTtlSeconds))
	}
	if b.MaxTimeoutSeconds != nil {
		p.MaxTimeout = ptr(durationOf(b.MaxTimeoutSeconds))
	}
	v, err := s.deps.Network.UpdateSolver(ctx, req.SolverId, p)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.UpdateSolver200JSONResponse(toSolver(v)), nil
}

// DeleteSolver removes a solver.
func (s *Server) DeleteSolver(ctx context.Context, req apigen.DeleteSolverRequestObject) (apigen.DeleteSolverResponseObject, error) {
	if err := s.deps.Network.DeleteSolver(ctx, req.SolverId); err != nil {
		return nil, networkError(err)
	}
	return apigen.DeleteSolver204Response{}, nil
}

// TestSolver checks a saved solver.
func (s *Server) TestSolver(ctx context.Context, req apigen.TestSolverRequestObject) (apigen.TestSolverResponseObject, error) {
	v, err := s.deps.Network.Solver(ctx, req.SolverId)
	if err != nil {
		return nil, networkError(err)
	}
	r, err := s.deps.Network.TestSolver(ctx, v)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestSolver200JSONResponse(toNetworkTest(r)), nil
}

func toProxy(p network.Proxy) apigen.Proxy {
	return apigen.Proxy{
		Id: p.ID, Name: p.Name, Url: p.URL, Username: nonEmpty(p.Username), HasPassword: p.HasPassword,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func proxyInput(b apigen.NewProxy) network.ProxyInput {
	return network.ProxyInput{ID: b.Id, Name: b.Name, URL: b.Url, Username: deref(b.Username), Password: deref(b.Password)}
}

// ListProxies lists the proxies.
func (s *Server) ListProxies(ctx context.Context, _ apigen.ListProxiesRequestObject) (apigen.ListProxiesResponseObject, error) {
	list, err := s.deps.Network.Proxies(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListProxies200JSONResponse{Proxies: make([]apigen.Proxy, len(list))}
	for i, v := range list {
		out.Proxies[i] = toProxy(v)
	}
	return out, nil
}

// CreateProxy adds a proxy.
func (s *Server) CreateProxy(ctx context.Context, req apigen.CreateProxyRequestObject) (apigen.CreateProxyResponseObject, error) {
	v, err := s.deps.Network.CreateProxy(ctx, proxyInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.CreateProxy201JSONResponse(toProxy(v)), nil
}

// TestProxySettings checks an unsaved proxy (or a saved one with changes).
func (s *Server) TestProxySettings(ctx context.Context, req apigen.TestProxySettingsRequestObject) (apigen.TestProxySettingsResponseObject, error) {
	r, err := s.deps.Network.TestProxy(ctx, proxyInput(req.Body.Proxy), deref(req.Body.Url))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestProxySettings200JSONResponse(toNetworkTest(r)), nil
}

// GetProxy returns a proxy.
func (s *Server) GetProxy(ctx context.Context, req apigen.GetProxyRequestObject) (apigen.GetProxyResponseObject, error) {
	v, err := s.deps.Network.Proxy(ctx, req.ProxyId)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.GetProxy200JSONResponse(toProxy(v)), nil
}

// UpdateProxy changes a proxy.
func (s *Server) UpdateProxy(ctx context.Context, req apigen.UpdateProxyRequestObject) (apigen.UpdateProxyResponseObject, error) {
	b := req.Body
	v, err := s.deps.Network.UpdateProxy(ctx, req.ProxyId, network.ProxyPatch{Name: b.Name, URL: b.Url, Username: b.Username, Password: b.Password})
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.UpdateProxy200JSONResponse(toProxy(v)), nil
}

// DeleteProxy removes a proxy.
func (s *Server) DeleteProxy(ctx context.Context, req apigen.DeleteProxyRequestObject) (apigen.DeleteProxyResponseObject, error) {
	if err := s.deps.Network.DeleteProxy(ctx, req.ProxyId); err != nil {
		return nil, networkError(err)
	}
	return apigen.DeleteProxy204Response{}, nil
}

// TestProxy checks a saved proxy.
func (s *Server) TestProxy(ctx context.Context, req apigen.TestProxyRequestObject) (apigen.TestProxyResponseObject, error) {
	p, err := s.deps.Network.Proxy(ctx, req.ProxyId)
	if err != nil {
		return nil, networkError(err)
	}
	target := ""
	if req.Body != nil {
		target = deref(req.Body.Url)
	}
	r, err := s.deps.Network.TestProxy(ctx, network.ProxyInput{ID: p.ID, Name: p.Name, URL: p.URL, Username: p.Username}, target)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestProxy200JSONResponse(toNetworkTest(r)), nil
}
