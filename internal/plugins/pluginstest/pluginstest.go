// SPDX-License-Identifier: GPL-3.0-or-later

// Package pluginstest serves made-up source plugins for tests over the
// real wire (gRPC on cleartext HTTP/2).
package pluginstest

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/panelarr/source-api/sdk/plugin"
)

// Serve runs p behind apiKey and returns its URL.
func Serve(t testing.TB, p plugin.Plugin, apiKey string) string {
	t.Helper()
	return ServeWithRoutes(t, p, apiKey, nil)
}

// ServeWithRoutes is Serve with more routes on the plugin's address, such
// as files it links to by a relative path (icons).
func ServeWithRoutes(t testing.TB, p plugin.Plugin, apiKey string, routes map[string]http.Handler) string {
	t.Helper()
	h := plugin.NewHandler(p, apiKey)
	if len(routes) > 0 {
		mux := http.NewServeMux()
		for pattern, rh := range routes {
			mux.Handle(pattern, rh)
		}
		mux.Handle("/", h)
		h = mux
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// Switchable is a test plugin service that can stop answering.
type Switchable struct {
	URL string
	// Down cuts every connection without an answer, like a service that
	// isn't listening.
	Down atomic.Bool
	// Requests counts requests that reached the service, answered or not.
	Requests atomic.Int64
}

// ServeSwitchable runs p behind apiKey and returns the switchable service.
func ServeSwitchable(t testing.TB, p plugin.Plugin, apiKey string) *Switchable {
	t.Helper()
	s := &Switchable{}
	inner := plugin.NewHandler(p, apiKey)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Requests.Add(1)
		if s.Down.Load() {
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}
