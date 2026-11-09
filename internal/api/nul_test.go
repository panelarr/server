// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRejectNUL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
		params map[string]string
		body   string
		field  string // "" = accepted
		reject bool
	}{
		{name: "clean", target: "/x?q=a", body: `{"name":"a","tags":["b"]}`},
		{name: "escaped backslash is no NUL", target: "/x", body: `{"name":"a\\u0000b"}`},
		{name: "not JSON is left to the validator", target: "/x", body: `{"name":"\u0000`},
		{name: "top-level string", target: "/x", body: `{"name":"a\u0000"}`, field: "name", reject: true},
		{name: "nested array", target: "/x", body: `{"definition":{"types":["manga","\u0000"]}}`, field: "definition.types.1", reject: true},
		{name: "object key", target: "/x", body: `{"definition":{"a\u0000":1}}`, field: "definition", reject: true},
		{name: "bare string body", target: "/x", body: `"\u0000"`, field: "", reject: true},
		{name: "query value", target: "/x?q=a%00", field: "q", reject: true},
		{name: "query name", target: "/x?a%00=1", reject: true},
		{name: "path parameter", target: "/x", params: map[string]string{"source_id": "a\x00"}, field: "source_id", reject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			r := httptest.NewRequest(http.MethodPost, tc.target, body)
			rc := chi.NewRouteContext()
			for k, v := range tc.params {
				rc.URLParams.Add(k, v)
			}
			p := rejectNUL(r, rc)
			if !tc.reject {
				if p != nil {
					t.Fatalf("rejected: %+v", p)
				}
				// The body is still there for the validator and handler.
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != tc.body {
					t.Fatalf("body after check %q, %v; want %q", got, err, tc.body)
				}
				return
			}
			if p == nil {
				t.Fatal("accepted")
			}
			if p.Status != http.StatusBadRequest || p.Code != CodeInvalidRequest {
				t.Fatalf("problem %+v", p)
			}
			if tc.field != "" && (len(p.Fields) != 1 || p.Fields[0].Field != tc.field) {
				t.Fatalf("fields %+v, want %q", p.Fields, tc.field)
			}
		})
	}
}

// Text with NUL is refused as a 400 by every endpoint, never stored (the
// database would fail with a 500).
func TestNULInputIsABadRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)

	check := func(c *client, method, path string, body any, field string) {
		t.Helper()
		p := c.expectProblem(method, path, body, http.StatusBadRequest, CodeInvalidRequest)
		if p.Errors == nil || len(*p.Errors) != 1 || (*p.Errors)[0].Field != field {
			t.Fatalf("%s %s: errors %+v, want field %q", method, path, p.Errors, field)
		}
	}
	check(user, http.MethodPatch, "/api/v1/me", map[string]any{"display_name": "a\x00b"}, "display_name")
	check(user, http.MethodPost, viewsPath, map[string]any{"name": "a\x00", "definition": map[string]any{}}, "name")
	check(user, http.MethodPost, viewsPath, map[string]any{"name": "Mine", "definition": map[string]any{"types": []string{"\x00"}}}, "definition.types.0")
	check(user, http.MethodPost, viewsPath, map[string]any{"name": "Mine", "definition": map[string]any{"statuses": []string{"ongoing\x00"}}}, "definition.statuses.0")
	check(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "x\x00", "password": "user password 1"}, "username")
	check(user, http.MethodGet, "/api/v1/catalog/search?q=a%00", nil, "q")

	// Nothing was stored, and clean input still works.
	if vs := listViews(user); len(vs) != 0 {
		t.Fatalf("views %+v", vs)
	}
	createView(user, "Mine", map[string]any{"types": []string{"manga"}})
	call[map[string]any](user, http.MethodPatch, "/api/v1/me", map[string]any{"display_name": "Reader"}, 200)
}
