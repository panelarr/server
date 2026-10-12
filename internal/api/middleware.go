// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/panelarr/server/internal/metrics"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDHeader carries the request ID in requests and responses.
const RequestIDHeader = "X-Request-Id"

// A client-supplied request ID is kept only when it looks harmless.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID returns the request's ID, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// withRequestID gives every request an ID (the client's X-Request-Id when
// valid) and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// apiHeaders lock down everything the API sends (JSON, images, events):
// never sniffed into another type, never rendered as a page that runs
// code, never framed, never leaking the URL as a referrer.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// observe logs each request and records its metrics. Routes are labelled
// by their pattern, never the raw path, so labels stay bounded.
func observe(log *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			d := time.Since(start)
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			m.HTTP(route, r.Method, status, d)
			level := slog.LevelInfo
			switch {
			case (status == http.StatusBadGateway || status == http.StatusGatewayTimeout) && imageRoute(route):
				// An image or icon behind the server failed; its handler
				// logs why at most once in a while, so a page of failing
				// images doesn't flood the log.
				level = slog.LevelInfo
			case status >= 500:
				level = slog.LevelError
			case route == "/healthz" || route == "/readyz":
				level = slog.LevelDebug
			}
			// The query string is left out: it can carry tokens.
			log.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path, "route", route, "status", status,
				"bytes", ww.BytesWritten(), "duration_ms", d.Milliseconds(), "request_id", RequestID(r.Context()))
		})
	}
}

// imageRoute reports whether route serves images whose handlers log their
// failures themselves (source images and icons, package icons).
func imageRoute(route string) bool {
	return strings.HasSuffix(route, "/sources/{source_id}/image") ||
		strings.HasSuffix(route, "/packages/{package_id}/icon")
}

// recoverer turns a panic into a 500 problem and logs the stack.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic", "panic", v, "path", r.URL.Path, "request_id", RequestID(r.Context()), "stack", string(debug.Stack()))
					writeProblem(w, r, NewProblem(http.StatusInternalServerError, CodeInternal, ""))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
