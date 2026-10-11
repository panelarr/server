// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
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

// Overall states.
const (
	statusOK          = "ok"
	statusDegraded    = "degraded"
	statusUnavailable = "unavailable"
)

type checkResult struct {
	Name     string
	Required bool
	Err      error
	Latency  time.Duration
}

// checkTimeout bounds each readiness check, so a probe answers in time.
const checkTimeout = 2 * time.Second

// runChecks runs every check at once and returns the overall state and
// the results in name order.
func runChecks(ctx context.Context, checks []Check) (string, []checkResult) {
	results := make([]checkResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			start := time.Now()
			err := c.Run(ctx)
			results[i] = checkResult{Name: c.Name, Required: c.Required, Err: err, Latency: time.Since(start)}
		})
	}
	wg.Wait()
	slices.SortFunc(results, func(a, b checkResult) int { return strings.Compare(a.Name, b.Name) })
	overall := statusOK
	for _, r := range results {
		switch {
		case r.Err == nil:
		case r.Required:
			overall = statusUnavailable
		case overall == statusOK:
			overall = statusDegraded
		}
	}
	return overall, results
}

// healthz answers liveness probes: the process is up and serving HTTP.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": statusOK})
}

type readiness struct {
	Status string                       `json:"status"`
	Checks map[string]map[string]string `json:"checks"`
}

// readyz answers readiness probes: 200 when every required dependency
// works (also while degraded), 503 otherwise. Error messages are left out:
// the probe needs no authentication. Admins see them in the detailed
// health.
func readyz(checks []Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		overall, results := runChecks(r.Context(), checks)
		out := readiness{Status: overall, Checks: make(map[string]map[string]string, len(results))}
		for _, res := range results {
			st := statusOK
			if res.Err != nil {
				st = "failing"
			}
			out.Checks[res.Name] = map[string]string{"status": st}
		}
		status := http.StatusOK
		if overall == statusUnavailable {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, out)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
