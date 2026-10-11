// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Check is one readiness dependency. A failing required check makes the
// server not ready (e.g. the database). A failing optional one only marks it
// degraded: the server keeps serving what it can (e.g. reading downloaded
// chapters while a connected service is down).
type Check struct {
	Name     string
	Required bool
	Run      func(ctx context.Context) error
}

type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type readiness struct {
	Status string                 `json:"status"` // ok, degraded or unavailable
	Checks map[string]checkResult `json:"checks"`
}

// checkTimeout bounds each readiness check, so a probe answers in time.
const checkTimeout = 2 * time.Second

func runChecks(ctx context.Context, checks []Check) readiness {
	out := readiness{Status: "ok", Checks: make(map[string]checkResult, len(checks))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			res := checkResult{Status: "ok"}
			if err := c.Run(ctx); err != nil {
				res = checkResult{Status: "failing", Error: err.Error()}
			}
			mu.Lock()
			defer mu.Unlock()
			out.Checks[c.Name] = res
			switch {
			case res.Status == "ok":
			case c.Required:
				out.Status = "unavailable"
			case out.Status == "ok":
				out.Status = "degraded"
			}
		})
	}
	wg.Wait()
	return out
}

// healthz answers liveness probes: the process is up and serving HTTP.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers readiness probes: 200 when every required dependency
// works (also while degraded), 503 otherwise. Error messages are left out:
// the probe needs no authentication.
func readyz(checks []Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res := runChecks(r.Context(), checks)
		for name, c := range res.Checks {
			c.Error = ""
			res.Checks[name] = c
		}
		status := http.StatusOK
		if res.Status == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, res)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
