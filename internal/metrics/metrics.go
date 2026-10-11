// SPDX-License-Identifier: GPL-3.0-or-later

// Package metrics defines the server's Prometheus metrics. Labels stay
// bounded: route patterns, methods, status codes, job and source IDs, never
// URLs with IDs, titles, user names or credentials.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector the server updates. A nil *Metrics is valid
// and records nothing, so code doesn't have to check whether metrics are on.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
}

// New registers the server's metrics, plus the Go runtime and process ones,
// on a fresh registry.
func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{Registry: r}
	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "panelarr_http_requests_total", Help: "HTTP requests by route pattern, method and status code.",
	}, []string{"route", "method", "code"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "panelarr_http_request_duration_seconds", Help: "HTTP request latency by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})
	r.MustRegister(m.HTTPRequests, m.HTTPDuration)
	return m
}

// HTTP records one request. route is the matched pattern (e.g.
// "/api/v1/works/{id}"), or "unmatched".
func (m *Metrics) HTTP(route, method string, code int, d time.Duration) {
	if m == nil {
		return
	}
	m.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.HTTPDuration.WithLabelValues(route).Observe(d.Seconds())
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
