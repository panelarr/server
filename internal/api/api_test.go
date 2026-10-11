// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/metrics"
)

func do(t *testing.T, h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func ok(context.Context) error { return nil }

func failing(context.Context) error { return errors.New("connection refused to 10.0.0.1") }

func TestSpecIsValid(t *testing.T) {
	spec, err := apigen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Deps{Checks: []Check{{Name: "database", Required: true, Run: failing}}})
	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness must not depend on dependencies: %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []Check
		code   int
		status string
	}{
		{"ok", []Check{{Name: "database", Required: true, Run: ok}}, 200, "ok"},
		{"degraded", []Check{{Name: "database", Required: true, Run: ok}, {Name: "meta", Run: failing}}, 200, "degraded"},
		{"unavailable", []Check{{Name: "database", Required: true, Run: failing}, {Name: "meta", Run: ok}}, 503, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, NewHandler(Deps{Checks: tc.checks}), http.MethodGet, "/readyz")
			if rec.Code != tc.code {
				t.Fatalf("code %d, want %d", rec.Code, tc.code)
			}
			got := decode[readiness](t, rec)
			if got.Status != tc.status || len(got.Checks) != len(tc.checks) {
				t.Fatalf("got %+v", got)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.1") {
				t.Fatal("unauthenticated probe leaks error details")
			}
		})
	}
}

func TestSystemInfo(t *testing.T) {
	rec := do(t, NewHandler(Deps{}), http.MethodGet, "/api/v1/system/info")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	info := decode[apigen.SystemInfo](t, rec)
	if info.Name != apigen.Panelarr || info.Version == "" {
		t.Fatalf("got %+v", info)
	}
}

func TestOpenAPIServed(t *testing.T) {
	rec := do(t, NewHandler(Deps{}), http.MethodGet, "/api/v1/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	doc := decode[map[string]any](t, rec)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", doc["openapi"])
	}
}

func TestProblems(t *testing.T) {
	h := NewHandler(Deps{})
	for _, tc := range []struct {
		method, path string
		code         int
		errCode      string
	}{
		{http.MethodGet, "/api/v1/nope", 404, CodeNotFound},
		{http.MethodDelete, "/api/v1/system/info", 405, CodeMethodNotAllowed},
	} {
		rec := do(t, h, tc.method, tc.path, RequestIDHeader, "abc-123")
		if rec.Code != tc.code || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		p := decode[apigen.Problem](t, rec)
		if p.Status != tc.code || p.Code != tc.errCode || p.RequestId == nil || *p.RequestId != "abc-123" {
			t.Fatalf("problem %+v", p)
		}
	}
}

func TestRequestID(t *testing.T) {
	h := NewHandler(Deps{})
	if id := do(t, h, http.MethodGet, "/healthz", RequestIDHeader, "client.id_1").Header().Get(RequestIDHeader); id != "client.id_1" {
		t.Fatalf("client ID not kept: %q", id)
	}
	id := do(t, h, http.MethodGet, "/healthz", RequestIDHeader, "bad id\n<script>").Header().Get(RequestIDHeader)
	if !validRequestID.MatchString(id) || strings.Contains(id, "script") {
		t.Fatalf("unsafe ID kept: %q", id)
	}
}

func TestErrorsHideInternals(t *testing.T) {
	r := chi.NewRouter()
	r.Use(withRequestID, recoverer(discard()))
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("db password is hunter2") })
	r.Get("/err", func(w http.ResponseWriter, r *http.Request) {
		writeError(discard())(w, r, errors.New("db password is hunter2"))
	})
	r.Get("/problem", func(w http.ResponseWriter, r *http.Request) {
		writeError(discard())(w, r, NewProblem(http.StatusConflict, "already_exists", "it exists"))
	})
	for _, path := range []string{"/panic", "/err"} {
		rec := do(t, r, http.MethodGet, path)
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	p := decode[apigen.Problem](t, do(t, r, http.MethodGet, "/problem"))
	if p.Status != 409 || p.Code != "already_exists" || p.Detail == nil || *p.Detail != "it exists" {
		t.Fatalf("problem %+v", p)
	}
}

func TestMetricsUseRoutePatterns(t *testing.T) {
	m := metrics.New()
	h := NewHandler(Deps{Metrics: m})
	do(t, h, http.MethodGet, "/api/v1/system/info")
	do(t, h, http.MethodGet, "/api/v1/does/not/exist")
	if n := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("/api/v1/system/info", "GET", "200")); n != 1 {
		t.Fatalf("system info requests = %v", n)
	}
	// Unknown paths must not create a label per path.
	if n := testutil.CollectAndCount(m.HTTPRequests); n != 2 {
		t.Fatalf("%d series, want 2", n)
	}
}
