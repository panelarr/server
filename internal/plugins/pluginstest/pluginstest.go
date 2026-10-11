// SPDX-License-Identifier: GPL-3.0-or-later

// Package pluginstest serves made-up source plugins for tests over the
// real wire (gRPC on cleartext HTTP/2).
package pluginstest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panelarr/source-api/sdk/plugin"
)

// Serve runs p behind apiKey and returns its URL.
func Serve(t testing.TB, p plugin.Plugin, apiKey string) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(plugin.NewHandler(p, apiKey))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}
