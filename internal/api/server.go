// SPDX-License-Identifier: GPL-3.0-or-later

// Package api serves the server's HTTP surface: the REST API under /api/v1
// (generated from api/openapi.yaml), the health probes and, later, the
// event stream and the web app.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/buildinfo"
	"github.com/panelarr/server/internal/metrics"
)

// BasePath is where the REST API is served.
const BasePath = "/api/v1"

// Deps are what the HTTP layer needs from the rest of the server.
type Deps struct {
	Log     *slog.Logger
	Metrics *metrics.Metrics // nil when metrics are off
	// Checks are the readiness dependencies, e.g. the database.
	Checks []Check
}

// Server implements the generated strict API interface.
type Server struct {
	deps Deps
}

var _ apigen.StrictServerInterface = (*Server)(nil)

// NewHandler builds the root HTTP handler.
func NewHandler(deps Deps) http.Handler {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	s := &Server{deps: deps}

	r := chi.NewRouter()
	r.Use(withRequestID, observe(deps.Log, deps.Metrics), recoverer(deps.Log))
	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(deps.Checks))

	r.Route(BasePath, func(r chi.Router) {
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
	return r
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

// GetSystemInfo identifies the server.
func (s *Server) GetSystemInfo(context.Context, apigen.GetSystemInfoRequestObject) (apigen.GetSystemInfoResponseObject, error) {
	return apigen.GetSystemInfo200JSONResponse{Name: apigen.Panelarr, Version: buildinfo.Version}, nil
}
