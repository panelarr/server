// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	jobsv1 "github.com/panelarr/source-api/gen/panelarr/jobs/v1"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/plugins"
)

// A plugin's jobs, in the server's jobs shape with the plugin's own run
// IDs (PluginJob*).

func pluginJobError(ctx context.Context, err error) error {
	var pe *plugin.Error
	switch {
	case errors.Is(err, plugins.ErrNoJobs):
		return NewProblem(http.StatusUnprocessableEntity, "no_jobs", "the plugin has no jobs")
	case errors.As(err, &pe) && pe.Reason == plugin.ReasonAlreadyRunning:
		return &Problem{Status: http.StatusConflict, Code: "already_running", Detail: "the job is running as " + pe.CurrentRunID, CurrentRun: pe.CurrentRunID}
	case errors.As(err, &pe) && pe.Code == plugin.CodeFailedPrecondition && pe.Reason == "":
		return NewProblem(http.StatusConflict, "not_cancellable", pe.Message)
	case errors.As(err, &pe) && pe.Code == plugin.CodeInvalidArgument && pe.Reason == "":
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, pe.Message)
	}
	return pluginError(ctx, err)
}

func fromPluginRunSummary(r *jobsv1.JobRunSummary) apigen.PluginJobRunSummary {
	out := apigen.PluginJobRunSummary{
		Id: r.GetId(), JobId: r.GetJobId(), Status: apigen.JobRunStatus(enumName(r.GetStatus().String(), "JOB_RUN_STATUS_")),
		Trigger: apigen.PluginJobRunSummaryTrigger(enumName(r.GetTrigger().String(), "JOB_TRIGGER_")),
		Message: r.GetMessage(), Counts: r.GetCounts(), TriggeredBy: nonEmpty(r.GetTriggeredBy()),
	}
	if out.Counts == nil {
		out.Counts = map[string]int64{}
	}
	if t := r.GetStartedAt(); t != nil {
		out.StartedAt = t.AsTime()
	}
	if t := r.GetFinishedAt(); t != nil {
		at := t.AsTime()
		out.FinishedAt = &at
	}
	return out
}

func fromPluginJob(j *jobsv1.Job) apigen.PluginJob {
	out := apigen.PluginJob{
		Id: j.GetId(), Name: j.GetName(), Description: j.GetDescription(), Group: j.GetGroup(), Running: j.GetRunning(),
		Cancellable: j.GetCancellable(), Params: make([]apigen.JobParam, 0, len(j.GetParams())),
		Schedule: apigen.JobSchedule{Enabled: j.GetSchedule().GetEnabled(), Source: nonEmpty(j.GetSchedule().GetSource())},
	}
	if c := j.GetSchedule().GetCron(); c != "" {
		out.Schedule.Cron = &c
		if zone := cronZone(c); zone != "" {
			out.Schedule.TimeZone = &zone
		}
	}
	if iv := j.GetSchedule().GetInterval(); iv != nil {
		out.Schedule.IntervalSeconds = ptr(int(iv.AsDuration().Seconds()))
	}
	if j.GetRunning() {
		out.CurrentRunId = nonEmpty(j.GetCurrentRunId())
	}
	if lr := j.GetLastRun(); lr != nil {
		sum := fromPluginRunSummary(lr)
		out.LastRun = &sum
	}
	if t := j.GetNextRunAt(); t != nil {
		at := t.AsTime()
		out.NextRunAt = &at
	}
	for _, p := range j.GetParams() {
		jp := apigen.JobParam{
			Name: p.GetName(), Type: apigen.JobParamType(p.GetType()), Required: p.GetRequired(),
			Description: nonEmpty(p.GetDescription()), Default: nonEmpty(p.GetDefault()),
		}
		if c := p.GetChoices(); len(c) > 0 {
			jp.Choices = &c
		}
		out.Params = append(out.Params, jp)
	}
	return out
}

// ListPluginJobs lists a plugin's jobs.
func (s *Server) ListPluginJobs(ctx context.Context, req apigen.ListPluginJobsRequestObject) (apigen.ListPluginJobsResponseObject, error) {
	list, err := s.deps.Plugins.Jobs(ctx, req.PluginId)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	out := apigen.ListPluginJobs200JSONResponse{Jobs: make([]apigen.PluginJob, len(list))}
	for i, j := range list {
		out.Jobs[i] = fromPluginJob(j)
	}
	return out, nil
}

// GetPluginJob returns one of a plugin's jobs.
func (s *Server) GetPluginJob(ctx context.Context, req apigen.GetPluginJobRequestObject) (apigen.GetPluginJobResponseObject, error) {
	j, err := s.deps.Plugins.Job(ctx, req.PluginId, req.JobId)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	return apigen.GetPluginJob200JSONResponse(fromPluginJob(j)), nil
}

// ListPluginJobRuns lists a plugin job's runs.
func (s *Server) ListPluginJobRuns(ctx context.Context, req apigen.ListPluginJobRunsRequestObject) (apigen.ListPluginJobRunsResponseObject, error) {
	status := jobsv1.JobRunStatus_JOB_RUN_STATUS_UNSPECIFIED
	if req.Params.Status != nil {
		status = jobsv1.JobRunStatus(jobsv1.JobRunStatus_value["JOB_RUN_STATUS_"+strings.ToUpper(string(*req.Params.Status))])
	}
	runs, next, err := s.deps.Plugins.JobRuns(ctx, req.PluginId, req.JobId, status, int32(min(pageSize(req.Params.PageSize), 100)), deref(req.Params.PageToken)) //nolint:gosec // at most 100
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	out := apigen.ListPluginJobRuns200JSONResponse{Runs: make([]apigen.PluginJobRunSummary, len(runs)), NextPageToken: next}
	for i, r := range runs {
		out.Runs[i] = fromPluginRunSummary(r)
	}
	return out, nil
}

// RunPluginJob starts one of a plugin's jobs.
func (s *Server) RunPluginJob(ctx context.Context, req apigen.RunPluginJobRequestObject) (apigen.RunPluginJobResponseObject, error) {
	var params map[string]string
	if req.Body != nil && req.Body.Params != nil {
		params = *req.Body.Params
	}
	r, err := s.deps.Plugins.RunJob(ctx, req.PluginId, req.JobId, params, auth.FromContext(ctx).User.Username)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	return apigen.RunPluginJob202JSONResponse(fromPluginRunSummary(r)), nil
}

// GetPluginJobRun returns a plugin job's run in full.
func (s *Server) GetPluginJobRun(ctx context.Context, req apigen.GetPluginJobRunRequestObject) (apigen.GetPluginJobRunResponseObject, error) {
	r, err := s.deps.Plugins.JobRun(ctx, req.PluginId, req.RunId)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	out := apigen.GetPluginJobRun200JSONResponse{Summary: fromPluginRunSummary(r.GetSummary()), Params: r.GetParams(), Notes: []apigen.JobRunNote{}}
	if out.Params == nil {
		out.Params = map[string]string{}
	}
	for _, n := range r.GetNotes() {
		out.Notes = append(out.Notes, apigen.JobRunNote{At: n.GetAt().AsTime(), Level: n.GetLevel(), Message: n.GetMessage(), Ref: nonEmpty(n.GetRef())})
	}
	if p := r.GetProgress(); p != nil {
		out.Progress = &apigen.JobProgress{Done: p.GetDone(), Phase: nonEmpty(p.GetPhase()), Unit: nonEmpty(p.GetUnit())}
		if p.GetTotal() > 0 {
			out.Progress.Total = ptr(p.GetTotal())
		}
	}
	return out, nil
}

// CancelPluginJobRun cancels a plugin job's running run.
func (s *Server) CancelPluginJobRun(ctx context.Context, req apigen.CancelPluginJobRunRequestObject) (apigen.CancelPluginJobRunResponseObject, error) {
	run, err := s.deps.Plugins.JobRun(ctx, req.PluginId, req.RunId)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	if run.GetSummary().GetStatus() != jobsv1.JobRunStatus_JOB_RUN_STATUS_RUNNING {
		return nil, NewProblem(http.StatusConflict, "not_running", "the run has finished")
	}
	r, err := s.deps.Plugins.CancelJobRun(ctx, req.PluginId, req.RunId)
	if err != nil {
		return nil, pluginJobError(ctx, err)
	}
	return apigen.CancelPluginJobRun202JSONResponse(fromPluginRunSummary(r)), nil
}
