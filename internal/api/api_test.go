// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/metrics"
)

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
	if _, err := loadPolicies(); err != nil {
		t.Fatalf("every operation needs a valid x-panelarr-access: %v", err)
	}
}

func TestProbes(t *testing.T) {
	t.Parallel()
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
			h := newHarness(t, withChecks(tc.checks...))
			c := h.client()
			if res := c.do(http.MethodGet, "/healthz", nil); res.status != 200 {
				t.Fatalf("liveness must not depend on dependencies: %d", res.status)
			}
			res := c.do(http.MethodGet, "/readyz", nil)
			if res.status != tc.code {
				t.Fatalf("readyz %d, want %d", res.status, tc.code)
			}
			var got readiness
			_ = json.Unmarshal(res.body, &got)
			if got.Status != tc.status || len(got.Checks) != len(tc.checks) {
				t.Fatalf("got %+v", got)
			}
			if strings.Contains(string(res.body), "10.0.0.1") {
				t.Fatal("unauthenticated probe leaks error details")
			}

			// Admins see the details.
			admin, _ := h.setupAdmin()
			health := call[apigen.SystemHealth](admin, http.MethodGet, "/api/v1/system/health", nil, 200)
			if string(health.Status) != tc.status || len(health.Components) != len(tc.checks) {
				t.Fatalf("health %+v", health)
			}
			for _, comp := range health.Components {
				if comp.Status == apigen.ComponentHealthStatusFailing && (comp.Error == nil || !strings.Contains(*comp.Error, "10.0.0.1")) {
					t.Fatalf("admin health hides the error: %+v", comp)
				}
			}
		})
	}
}

func TestSystemInfoAndSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.client()
	info := call[apigen.SystemInfo](c, http.MethodGet, "/api/v1/system/info", nil, 200)
	if info.Name != apigen.SystemInfoNamePanelarr || info.Version == "" || !info.SetupRequired {
		t.Fatalf("got %+v", info)
	}
	doc := call[map[string]any](c, http.MethodGet, "/api/v1/openapi.json", nil, 200)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", doc["openapi"])
	}
	h.setupAdmin()
	if info := call[apigen.SystemInfo](c, http.MethodGet, "/api/v1/system/info", nil, 200); info.SetupRequired {
		t.Fatal("still needs setup after the admin exists")
	}
}

func TestProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setupAdmin()
	c := h.client()
	c.header.Set(RequestIDHeader, "abc-123")
	for _, tc := range []struct {
		method, path string
		code         int
		errCode      string
	}{
		{http.MethodGet, "/api/v1/nope", 404, CodeNotFound},
		{http.MethodDelete, "/api/v1/system/info", 405, CodeMethodNotAllowed},
	} {
		p := c.expectProblem(tc.method, tc.path, nil, tc.code, tc.errCode)
		if p.RequestId == nil || *p.RequestId != "abc-123" {
			t.Fatalf("problem %+v", p)
		}
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.client()
	c.header.Set(RequestIDHeader, "client.id_1")
	if id := c.do(http.MethodGet, "/healthz", nil).header.Get(RequestIDHeader); id != "client.id_1" {
		t.Fatalf("client ID not kept: %q", id)
	}
	c.header.Set(RequestIDHeader, "bad id <script>")
	id := c.do(http.MethodGet, "/healthz", nil).header.Get(RequestIDHeader)
	if !validRequestID.MatchString(id) || strings.Contains(id, "script") {
		t.Fatalf("unsafe ID kept: %q", id)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
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
		rec := get(t, r, path)
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	var p apigen.Problem
	_ = json.Unmarshal(get(t, r, "/problem").Body.Bytes(), &p)
	if p.Status != 409 || p.Code != "already_exists" || p.Detail == nil || *p.Detail != "it exists" {
		t.Fatalf("problem %+v", p)
	}
}

func TestMetricsUseRoutePatterns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withMetricsOnAPI())
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	m := h.metrics
	admin.do(http.MethodGet, "/api/v1/users/"+"00000000-0000-0000-0000-000000000001", nil)
	admin.do(http.MethodGet, "/api/v1/users/"+"00000000-0000-0000-0000-000000000002", nil)
	admin.do(http.MethodGet, "/api/v1/does/not/exist", nil)
	if n := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("/api/v1/users/{id}", "GET", "404")); n != 2 {
		t.Fatalf("user lookups = %v", n)
	}
	if n := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("/api/v1/*", "GET", "404")); n != 1 {
		t.Fatalf("unknown API paths share one label: %v", n)
	}

	// /metrics on the API port needs the metrics:read scope.
	anon := h.client()
	anon.expectProblem(http.MethodGet, "/metrics", nil, 401, "unauthenticated")
	h.keyClient(user.newAPIKey("read")).expectProblem(http.MethodGet, "/metrics", nil, 403, "insufficient_scope")
	scrape := h.keyClient(admin.newAPIKey("metrics:read"))
	res := scrape.do(http.MethodGet, "/metrics", nil)
	if res.status != 200 || !strings.Contains(string(res.body), "panelarr_http_requests_total") {
		t.Fatalf("metrics: %d", res.status)
	}
	if strings.Contains(string(res.body), "00000000-0000-0000-0000-000000000001") {
		t.Fatal("metric labels carry raw paths")
	}
}

func TestMetricsCollector(t *testing.T) {
	m := metrics.New()
	m.HTTP("/x", "GET", 200, 0)
	if testutil.CollectAndCount(m.HTTPRequests) != 1 {
		t.Fatal("not recorded")
	}
	var nilMetrics *metrics.Metrics
	nilMetrics.HTTP("/x", "GET", 200, 0) // must not panic
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{mustPrefix(t, "10.0.0.0/8"), mustPrefix(t, "127.0.0.1/32")}
	for _, tc := range []struct {
		remote, xff, want string
	}{
		{"203.0.113.9:5000", "198.51.100.1", "203.0.113.9"},                        // untrusted peer: XFF ignored
		{"10.0.0.2:5000", "198.51.100.1", "198.51.100.1"},                          // trusted proxy
		{"10.0.0.2:5000", "198.51.100.66, 198.51.100.1, 10.0.0.3", "198.51.100.1"}, // spoofed first hop ignored
		{"10.0.0.2:5000", "", "10.0.0.2"},
		{"10.0.0.2:5000", "garbage", "10.0.0.2"},
		{"[::ffff:10.0.0.2]:5000", "198.51.100.1", "198.51.100.1"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := clientIP(r, trusted); got != tc.want {
			t.Errorf("%s / %q: got %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}
