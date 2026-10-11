// SPDX-License-Identifier: GPL-3.0-or-later

// Package api serves the server's HTTP surface: the REST API under /api/v1
// (generated from api/openapi.yaml), the health probes, the metrics and,
// later, the event stream and the web app.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/users"
)

// BasePath is where the REST API is served.
const BasePath = "/api/v1"

// Deps are what the HTTP layer needs from the rest of the server.
type Deps struct {
	Log     *slog.Logger
	Metrics *metrics.Metrics // nil when metrics are off
	// MetricsOnAPI serves /metrics on this handler, to callers holding the
	// metrics:read scope.
	MetricsOnAPI bool
	// Checks are the readiness dependencies, e.g. the database.
	Checks []Check

	Auth    *auth.Service
	Users   *users.Service
	Meta    *meta.Service
	Images  *imagecache.Cache
	Plugins *plugins.Host
	// Signer signs image links the server hands out.
	Signer *imagecache.Signer

	// SecureCookies marks session cookies Secure (https deployments).
	SecureCookies bool
	// TrustedProxies may set X-Forwarded-For.
	TrustedProxies []netip.Prefix
}

// Server implements the generated strict API interface.
type Server struct {
	deps     Deps
	policies policies
}

var _ apigen.StrictServerInterface = (*Server)(nil)

// NewHandler builds the root HTTP handler.
func NewHandler(deps Deps) (http.Handler, error) {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	pols, err := loadPolicies()
	if err != nil {
		return nil, err
	}
	if deps.Auth == nil || deps.Users == nil || deps.Meta == nil || deps.Images == nil || deps.Plugins == nil || deps.Signer == nil {
		return nil, fmt.Errorf("api: every service dependency is required")
	}
	s := &Server{deps: deps, policies: pols}

	r := chi.NewRouter()
	r.Use(withRequestID, observe(deps.Log, deps.Metrics), recoverer(deps.Log))
	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(deps.Checks))

	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		if deps.Metrics != nil && deps.MetricsOnAPI {
			r.Get("/metrics", s.serveMetrics)
		}
		r.Route(BasePath, func(r chi.Router) {
			r.Use(s.authorize(r))
			r.NotFound(func(w http.ResponseWriter, r *http.Request) {
				writeProblem(w, r, NewProblem(http.StatusNotFound, CodeNotFound, "no such endpoint"))
			})
			r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
				writeProblem(w, r, NewProblem(http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed here"))
			})
			r.Get("/openapi.json", serveSpec)
			strict := apigen.NewStrictHandlerWithOptions(s, nil, apigen.StrictHTTPServerOptions{
				RequestErrorHandlerFunc:  writeBadRequest,
				ResponseErrorHandlerFunc: writeError(deps.Log),
			})
			apigen.HandlerWithOptions(strict, apigen.ChiServerOptions{
				BaseRouter:       r,
				ErrorHandlerFunc: writeBadRequest,
			})
		})
	})
	return r, nil
}

// serveMetrics serves Prometheus metrics to callers with metrics:read.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	switch {
	case p == nil:
		writeProblem(w, r, NewProblem(http.StatusUnauthorized, "unauthenticated", "send an API key with the metrics:read scope"))
	case !p.HasScope(auth.ScopeMetricsRead):
		writeProblem(w, r, NewProblem(http.StatusForbidden, "insufficient_scope", "needs the metrics:read scope"))
	default:
		s.deps.Metrics.Handler().ServeHTTP(w, r)
	}
}

// serveSpec serves the OpenAPI document this server implements.
func serveSpec(w http.ResponseWriter, r *http.Request) {
	spec, err := apigen.GetSpecJSON()
	if err != nil {
		writeError(slog.Default())(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(spec)
}
