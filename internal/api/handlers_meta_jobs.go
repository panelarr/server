// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta"
)

// meta's jobs, in the server's own jobs shape (the shared jobs contract).

// metaJobError maps meta's job errors onto the server's job problems.
func metaJobError(err error) error {
	var busy *meta.JobRunningError
	var bad *meta.BadJobParamsError
	var off *meta.JobDisabledError
	switch {
	case errors.As(err, &off):
		return NewProblem(http.StatusConflict, "job_disabled", off.Reason)
	case errors.As(err, &busy):
		p := &Problem{Status: http.StatusConflict, Code: "already_running", Detail: "the job is running as " + busy.RunID}
		if id, perr := uuid.Parse(busy.RunID); perr == nil {
			p.CurrentRunID = &id
		}
		return p
	case errors.As(err, &bad):
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, bad.Reason)
	case errors.Is(err, meta.ErrJobNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, meta.ErrJobNotRunning):
		return NewProblem(http.StatusConflict, "not_running", "the run has finished")
	case errors.Is(err, meta.ErrJobNotCancelable):
		return NewProblem(http.StatusConflict, "not_cancellable", "this job can't be cancelled")
	}
	return metaError(err)
}

func enumName(s, prefix string) string { return strings.ToLower(strings.TrimPrefix(s, prefix)) }

func fromMetaRunSummary(r *metav1.JobRunSummary) apigen.JobRunSummary {
	id, _ := uuid.Parse(r.GetId())
	out := apigen.JobRunSummary{
		Id: id, JobId: r.GetJobId(), Status: apigen.JobRunStatus(enumName(r.GetStatus().String(), "JOB_RUN_STATUS_")),
		Trigger: apigen.JobRunSummaryTrigger(enumName(r.GetTrigger().String(), "JOB_TRIGGER_")),
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

func fromMetaJob(j *metav1.Job) apigen.Job {
	out := apigen.Job{
		Id: j.GetId(), Name: j.GetName(), Description: j.GetDescription(), Group: j.GetGroup(), Running: j.GetRunning(),
		Cancellable: j.GetCancellable(), Params: make([]apigen.JobParam, 0, len(j.GetParams())),
		Schedule:       apigen.JobSchedule{Enabled: j.GetSchedule().GetEnabled(), Source: nonEmpty(j.GetSchedule().GetSource())},
		DisabledReason: nonEmpty(j.GetDisabledReason()),
	}
	if c := j.GetSchedule().GetCron(); c != "" {
		out.Schedule.Cron = &c
		// meta runs its cron in UTC unless an expression names a zone;
		// "@every …" is an interval, with no zone.
		zone := cronZone(c)
		if zone == "" && !strings.HasPrefix(strings.TrimSpace(c), "@every") {
			zone = "UTC"
		}
		if zone != "" {
			out.Schedule.TimeZone = &zone
		}
	}
	if iv := j.GetSchedule().GetInterval(); iv != nil {
		out.Schedule.IntervalSeconds = ptr(int(iv.AsDuration().Seconds()))
	}
	if id, err := uuid.Parse(j.GetCurrentRunId()); err == nil && j.GetRunning() {
		out.CurrentRunId = &id
	}
	if lr := j.GetLastRun(); lr != nil {
		sum := fromMetaRunSummary(lr)
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

// ListMetaJobs lists meta's jobs.
func (s *Server) ListMetaJobs(ctx context.Context, _ apigen.ListMetaJobsRequestObject) (apigen.ListMetaJobsResponseObject, error) {
	list, err := s.deps.Meta.Jobs(ctx)
	if err != nil {
		return nil, metaJobError(err)
	}
	out := apigen.ListMetaJobs200JSONResponse{Jobs: make([]apigen.Job, len(list))}
	for i, j := range list {
		out.Jobs[i] = fromMetaJob(j)
	}
	return out, nil
}

// GetMetaJob returns one of meta's jobs.
func (s *Server) GetMetaJob(ctx context.Context, req apigen.GetMetaJobRequestObject) (apigen.GetMetaJobResponseObject, error) {
	j, err := s.deps.Meta.Job(ctx, req.JobId)
	if err != nil {
		return nil, metaJobError(err)
	}
	return apigen.GetMetaJob200JSONResponse(fromMetaJob(j)), nil
}

// ListMetaJobRuns lists a meta job's runs.
func (s *Server) ListMetaJobRuns(ctx context.Context, req apigen.ListMetaJobRunsRequestObject) (apigen.ListMetaJobRunsResponseObject, error) {
	status := metav1.JobRunStatus_JOB_RUN_STATUS_UNSPECIFIED
	if req.Params.Status != nil {
		status = metav1.JobRunStatus(metav1.JobRunStatus_value["JOB_RUN_STATUS_"+strings.ToUpper(string(*req.Params.Status))])
	}
	runs, next, err := s.deps.Meta.JobRuns(ctx, req.JobId, status, int32(pageSize(req.Params.PageSize)), deref(req.Params.PageToken)) //nolint:gosec // bounded by the spec
	if err != nil {
		return nil, metaJobError(err)
	}
	out := apigen.ListMetaJobRuns200JSONResponse{Runs: make([]apigen.JobRunSummary, len(runs)), NextPageToken: next}
	for i, r := range runs {
		out.Runs[i] = fromMetaRunSummary(r)
	}
	return out, nil
}

// RunMetaJob starts one of meta's jobs.
func (s *Server) RunMetaJob(ctx context.Context, req apigen.RunMetaJobRequestObject) (apigen.RunMetaJobResponseObject, error) {
	var params map[string]string
	if req.Body != nil && req.Body.Params != nil {
		params = *req.Body.Params
	}
	r, err := s.deps.Meta.RunJob(ctx, req.JobId, params)
	if err != nil {
		return nil, metaJobError(err)
	}
	s.deps.Log.InfoContext(ctx, "meta job started", "component", "audit", "job", req.JobId)
	return apigen.RunMetaJob202JSONResponse(fromMetaRunSummary(r)), nil
}

// GetMetaJobRun returns a meta job's run in full.
func (s *Server) GetMetaJobRun(ctx context.Context, req apigen.GetMetaJobRunRequestObject) (apigen.GetMetaJobRunResponseObject, error) {
	r, err := s.deps.Meta.JobRun(ctx, req.RunId.String())
	if err != nil {
		return nil, metaJobError(err)
	}
	out := apigen.GetMetaJobRun200JSONResponse{Summary: fromMetaRunSummary(r.GetSummary()), Params: r.GetParams(), Notes: []apigen.JobRunNote{}}
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

// CancelMetaJobRun cancels a running meta job run.
func (s *Server) CancelMetaJobRun(ctx context.Context, req apigen.CancelMetaJobRunRequestObject) (apigen.CancelMetaJobRunResponseObject, error) {
	r, err := s.deps.Meta.CancelJobRun(ctx, req.RunId.String())
	if err != nil {
		return nil, metaJobError(err)
	}
	s.deps.Log.InfoContext(ctx, "meta job run cancelled", "component", "audit", "run", req.RunId)
	return apigen.CancelMetaJobRun202JSONResponse(fromMetaRunSummary(r)), nil
}

// cronZone is the zone a cron expression names with a CRON_TZ= or TZ=
// prefix, or "".
func cronZone(expr string) string {
	for _, p := range []string{"CRON_TZ=", "TZ="} {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(expr), p); ok {
			zone, _, _ := strings.Cut(rest, " ")
			return zone
		}
	}
	return ""
}
