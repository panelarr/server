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

	JobRuns     *prometheus.CounterVec
	JobDuration *prometheus.HistogramVec
	JobRunning  *prometheus.GaugeVec

	TasksFinished *prometheus.CounterVec
	TaskDuration  *prometheus.HistogramVec
	TasksRunning  *prometheus.GaugeVec
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
	m.JobRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "panelarr_job_runs_total", Help: "Finished background job runs by job, outcome and trigger.",
	}, []string{"job", "status", "trigger"})
	m.JobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "panelarr_job_duration_seconds", Help: "Duration of background job runs.",
		Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600},
	}, []string{"job"})
	m.JobRunning = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "panelarr_job_running", Help: "1 while a run of the job is going on.",
	}, []string{"job"})
	m.TasksFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "panelarr_tasks_finished_total", Help: "Task attempts that ended, by kind and outcome (succeeded, failed, retried, cancelled).",
	}, []string{"kind", "outcome"})
	m.TaskDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "panelarr_task_duration_seconds", Help: "Duration of task attempts by kind.",
		Buckets: []float64{.1, .5, 1, 5, 15, 60, 300, 900},
	}, []string{"kind"})
	m.TasksRunning = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "panelarr_tasks_running", Help: "Tasks running now by kind.",
	}, []string{"kind"})
	r.MustRegister(m.HTTPRequests, m.HTTPDuration, m.JobRuns, m.JobDuration, m.JobRunning, m.TasksFinished, m.TaskDuration, m.TasksRunning)
	return m
}

// JobStarted marks a job as running. Safe on a nil *Metrics.
func (m *Metrics) JobStarted(job string) {
	if m == nil {
		return
	}
	m.JobRunning.WithLabelValues(job).Set(1)
}

// JobFinished records a finished (or skipped) run.
func (m *Metrics) JobFinished(job, status, trigger string, d time.Duration) {
	if m == nil {
		return
	}
	if status != "skipped" {
		m.JobRunning.WithLabelValues(job).Set(0)
		m.JobDuration.WithLabelValues(job).Observe(d.Seconds())
	}
	m.JobRuns.WithLabelValues(job, status, trigger).Inc()
}

// TaskStarted counts a running task.
func (m *Metrics) TaskStarted(kind string) {
	if m == nil {
		return
	}
	m.TasksRunning.WithLabelValues(kind).Inc()
}

// TaskEnded records how a task attempt ended.
func (m *Metrics) TaskEnded(kind, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.TasksRunning.WithLabelValues(kind).Dec()
	m.TasksFinished.WithLabelValues(kind, outcome).Inc()
	m.TaskDuration.WithLabelValues(kind).Observe(d.Seconds())
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
