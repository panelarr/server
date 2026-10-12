// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta/metatest"
)

func TestCanceledRequestIsNotAnError(t *testing.T) {
	t.Parallel()
	logs := &logRecorder{}
	write := writeError(slog.New(logs))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	write(w, httptest.NewRequest(http.MethodGet, "/api/v1/library", nil).WithContext(ctx), fmt.Errorf("read: %w", context.Canceled))
	if w.Code != StatusClientClosedRequest || len(logs.warnings()) != 0 {
		t.Fatalf("canceled: %d, %v", w.Code, logs.warnings())
	}
	// Even a mapped problem: the client is gone.
	w = httptest.NewRecorder()
	write(w, httptest.NewRequest(http.MethodGet, "/api/v1/library", nil).WithContext(ctx), NewProblem(http.StatusServiceUnavailable, "meta_unavailable", "x"))
	if w.Code != StatusClientClosedRequest {
		t.Fatalf("canceled problem: %d", w.Code)
	}

	// A real failure stays a 500 and an error.
	w = httptest.NewRecorder()
	write(w, httptest.NewRequest(http.MethodGet, "/api/v1/library", nil), errors.New("boom"))
	if w.Code != http.StatusInternalServerError || len(logs.warnings()) != 1 {
		t.Fatalf("failure: %d, %v", w.Code, logs.warnings())
	}
}

func TestClientGoingAwayIsQuiet(t *testing.T) {
	t.Parallel()
	logs := &logRecorder{}
	h := newHarness(t, withLog(slog.New(logs)))
	admin, _ := h.setupAdmin()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 2})
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)
	fake.SetDelay(500 * time.Millisecond)

	// The browser tab closes while the server waits for meta.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/api/v1/system/meta/network", nil)
	req.Header.Set(APIKeyHeader, admin.newAPIKey("read", "admin"))
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the request finished")
	}
	time.Sleep(time.Second) // the server finishes the request
	if w := logs.warnings(); len(w) != 0 {
		t.Fatalf("warnings and errors for a client that went away: %v", w)
	}
}
