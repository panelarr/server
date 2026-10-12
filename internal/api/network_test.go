// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panelarr/server/internal/api/apigen"
)

func TestSolversAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"msg":"FlareSolverr is ready!","version":"3.3.21"}`))
	}))
	t.Cleanup(solver.Close)

	body := map[string]any{"id": "fs", "name": "FlareSolverr", "type": "flaresolverr", "url": solver.URL, "session_ttl_seconds": 600}
	sv := call[apigen.Solver](admin, http.MethodPost, "/api/v1/network/solvers", body, 201)
	if sv.Id != "fs" || sv.SessionTtlSeconds == nil || *sv.SessionTtlSeconds != 600 || sv.MaxTimeoutSeconds != nil {
		t.Fatalf("solver %+v", sv)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/network/solvers", body, 409, "id_taken")
	admin.expectProblem(http.MethodPost, "/api/v1/network/solvers", map[string]any{"id": "x", "name": "x", "type": "flaresolverr", "url": "ftp://x"}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPost, "/api/v1/network/solvers", map[string]any{"id": "Bad", "name": "x", "type": "byparr", "url": solver.URL}, 400, CodeInvalidRequest)

	test := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/solvers/fs/test", nil, 200)
	if !test.Ok || test.Version == nil || *test.Version != "3.3.21" || !strings.Contains(test.Message, "ready") {
		t.Fatalf("test %+v", test)
	}
	// Before saving; one that doesn't answer.
	down := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/solvers/test",
		map[string]any{"id": "y", "name": "y", "type": "byparr", "url": "http://127.0.0.1:1"}, 200)
	if down.Ok || down.Message == "" {
		t.Fatalf("down %+v", down)
	}

	up := call[apigen.Solver](admin, http.MethodPatch, "/api/v1/network/solvers/fs", map[string]any{"name": "Main solver", "max_timeout_seconds": 60}, 200)
	if up.Name != "Main solver" || up.MaxTimeoutSeconds == nil || *up.MaxTimeoutSeconds != 60 || up.Url != solver.URL {
		t.Fatalf("updated %+v", up)
	}
	if l := call[apigen.SolverList](admin, http.MethodGet, "/api/v1/network/solvers", nil, 200); len(l.Solvers) != 1 {
		t.Fatalf("list %+v", l)
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/network/solvers/fs", nil, 204)
	admin.expectProblem(http.MethodGet, "/api/v1/network/solvers/fs", nil, 404, CodeNotFound)
}

// forwardProxy is an HTTP proxy that wants user:pass.
func forwardProxy(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		res, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(res.StatusCode)
	}))
	t.Cleanup(p.Close)
	return p
}

func TestProxiesAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(target.Close)
	proxy := forwardProxy(t, "vpn", "s3cret-pass")

	px := call[apigen.Proxy](admin, http.MethodPost, "/api/v1/network/proxies",
		map[string]any{"id": "vpn-nl", "name": "VPN NL", "url": proxy.URL, "username": "vpn", "password": "s3cret-pass"}, 201)
	if !px.HasPassword || px.Username == nil || *px.Username != "vpn" {
		t.Fatalf("proxy %+v", px)
	}
	// The password is kept encrypted and never shown.
	if strings.Contains(string(admin.do(http.MethodGet, "/api/v1/network/proxies", nil).body), "s3cret") {
		t.Fatal("the password is shown")
	}
	row, err := h.st.GetProxy(context.Background(), "vpn-nl")
	if err != nil || row.PasswordSealed == "" || strings.Contains(row.PasswordSealed, "s3cret") {
		t.Fatalf("stored %q %v", row.PasswordSealed, err)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/network/proxies",
		map[string]any{"id": "x", "name": "x", "url": "http://user:pw@proxy:8080"}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPost, "/api/v1/network/proxies", map[string]any{"id": "x", "name": "x", "url": "ftp://proxy:21"}, 400, CodeInvalidRequest)
	call[apigen.Proxy](admin, http.MethodPost, "/api/v1/network/proxies", map[string]any{"id": "tor", "name": "Tor", "url": "socks5://tor:9050"}, 201)

	// A request through it, with the stored password.
	ok := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/proxies/vpn-nl/test", map[string]string{"url": target.URL}, 200)
	if !ok.Ok || !strings.Contains(ok.Message, "204") {
		t.Fatalf("through %+v", ok)
	}
	// Only connecting.
	if c := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/proxies/vpn-nl/test", nil, 200); !c.Ok {
		t.Fatalf("connect %+v", c)
	}
	// Unsaved, with a wrong password.
	bad := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/proxies/test", map[string]any{
		"proxy": map[string]any{"id": "new", "name": "New", "url": proxy.URL, "username": "vpn", "password": "wrong"}, "url": target.URL,
	}, 200)
	if bad.Ok || !strings.Contains(bad.Message, "credentials") {
		t.Fatalf("wrong password %+v", bad)
	}

	// Editing without the password keeps it; no username drops it.
	if p := call[apigen.Proxy](admin, http.MethodPatch, "/api/v1/network/proxies/vpn-nl", map[string]any{"name": "VPN Amsterdam"}, 200); !p.HasPassword || p.Name != "VPN Amsterdam" {
		t.Fatalf("renamed %+v", p)
	}
	if c := call[apigen.NetworkTest](admin, http.MethodPost, "/api/v1/network/proxies/vpn-nl/test", map[string]string{"url": target.URL}, 200); !c.Ok {
		t.Fatalf("after rename %+v", c)
	}
	if p := call[apigen.Proxy](admin, http.MethodPatch, "/api/v1/network/proxies/vpn-nl", map[string]any{"username": ""}, 200); p.HasPassword || p.Username != nil {
		t.Fatalf("no credentials %+v", p)
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/network/proxies/vpn-nl", nil, 204)
	admin.expectProblem(http.MethodDelete, "/api/v1/network/proxies/vpn-nl", nil, 404, CodeNotFound)
}
