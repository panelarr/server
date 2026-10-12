// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"slices"

	jobsv1 "github.com/panelarr/source-api/gen/panelarr/jobs/v1"
	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"
)

// Plugins with the jobs capability run background jobs of their own (e.g.
// store refreshes, source health checks) in the shared jobs model, so
// Panelarr shows them in the same jobs view as its own and meta's.

// ErrNoJobs is a plugin without the jobs capability.
var ErrNoJobs = errors.New("plugins: the plugin has no jobs")

// HasJobs reports whether a plugin declares the jobs capability.
func HasJobs(info *sourcev1.PluginInfo) bool {
	return slices.Contains(info.GetCapabilities(), sourcev1.PluginCapability_PLUGIN_CAPABILITY_JOBS)
}

func (h *Host) jobsClient(ctx context.Context, id string) (client.Client, error) {
	p, err := h.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, ErrDisabled
	}
	if !HasJobs(p.Info) {
		return nil, ErrNoJobs
	}
	return h.Client(ctx, id)
}

// Jobs lists a plugin's jobs.
func (h *Host) Jobs(ctx context.Context, id string) ([]*jobsv1.Job, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.ListJobs(ctx, &sourcev1.ListJobsRequest{})
	if err != nil {
		return nil, err
	}
	return res.GetJobs(), nil
}

// Job returns one of a plugin's jobs.
func (h *Host) Job(ctx context.Context, id, job string) (*jobsv1.Job, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.GetJob(ctx, &sourcev1.GetJobRequest{JobId: job})
	if err != nil {
		return nil, err
	}
	return res.GetJob(), nil
}

// JobRuns lists a plugin job's runs, newest first.
func (h *Host) JobRuns(ctx context.Context, id, job string, status jobsv1.JobRunStatus, pageSize int32, token string) ([]*jobsv1.JobRunSummary, string, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.ListJobRuns(ctx, &sourcev1.ListJobRunsRequest{JobId: job, PageSize: pageSize, PageToken: token, Status: status})
	if err != nil {
		return nil, "", err
	}
	return res.GetRuns(), res.GetNextPageToken(), nil
}

// JobRun returns a plugin job's run in full.
func (h *Host) JobRun(ctx context.Context, id, run string) (*jobsv1.JobRun, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.GetJobRun(ctx, &sourcev1.GetJobRunRequest{RunId: run})
	if err != nil {
		return nil, err
	}
	return res.GetRun(), nil
}

// RunJob starts a plugin's job now, on behalf of who (recorded by the
// plugin as who triggered it).
func (h *Host) RunJob(ctx context.Context, id, job string, params map[string]string, who string) (*jobsv1.JobRunSummary, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.RunJob(cctx, &sourcev1.RunJobRequest{JobId: job, Params: params, OnBehalfOf: who})
	if err != nil {
		return nil, err
	}
	h.log.InfoContext(ctx, "plugin job started", "component", "audit", "plugin", id, "job", job)
	return res.GetRun(), nil
}

// CancelJobRun cancels a plugin job's running run.
func (h *Host) CancelJobRun(ctx context.Context, id, run string) (*jobsv1.JobRunSummary, error) {
	c, err := h.jobsClient(ctx, id)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.CancelJobRun(cctx, &sourcev1.CancelJobRunRequest{RunId: run})
	if err != nil {
		return nil, err
	}
	h.log.InfoContext(ctx, "plugin job run cancelled", "component", "audit", "plugin", id, "run", run)
	return res.GetRun(), nil
}
