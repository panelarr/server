// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/jobs"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
)

func jobsError(err error) error {
	var busy *jobs.AlreadyRunningError
	var bad *jobs.BadParamError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &busy):
		return NewProblem(http.StatusConflict, "already_running", "the job is running as "+busy.RunID.String())
	case errors.As(err, &bad):
		return fieldProblem("params."+bad.Name, bad.Reason)
	case errors.Is(err, jobs.ErrNotFound), errors.Is(err, queue.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, jobs.ErrNotRunning):
		return NewProblem(http.StatusConflict, "not_running", "the run has finished")
	case errors.Is(err, jobs.ErrNotCancellable):
		return NewProblem(http.StatusConflict, "not_cancellable", "this job can't be cancelled")
	case errors.Is(err, queue.ErrFinished):
		return NewProblem(http.StatusConflict, "task_finished", "the task has finished")
	case errors.Is(err, queue.ErrNotRetrying):
		return NewProblem(http.StatusConflict, "not_retryable", "only failed or cancelled tasks can be retried, once no other task does the same work")
	case errors.Is(err, queue.ErrUnknownKind):
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such kind of task")
	}
	return err
}

// Cursors of time-ordered lists: "<time>|<id>", base64url.
func timeCursor(at time.Time, id uuid.UUID) string {
	return encodePageToken(at.UTC().Format(time.RFC3339Nano) + "|" + id.String())
}

func parseTimeCursor(tok *string) (*time.Time, uuid.UUID, error) {
	raw, err := decodePageToken(tok)
	if err != nil || raw == "" {
		return nil, uuid.Nil, err
	}
	ts, id, ok := strings.Cut(raw, "|")
	at, terr := time.Parse(time.RFC3339Nano, ts)
	uid, uerr := uuid.Parse(id)
	if !ok || terr != nil || uerr != nil {
		return nil, uuid.Nil, fieldProblem("page_token", "not a token from this API")
	}
	return &at, uid, nil
}

func toRunSummary(r db.JobRun) apigen.JobRunSummary {
	out := apigen.JobRunSummary{
		Id: r.ID, JobId: r.JobID, Status: apigen.JobRunStatus(r.Status), Trigger: apigen.JobRunSummaryTrigger(r.Trigger),
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Message: r.Message, Counts: map[string]int64{},
		TriggeredBy: nonEmpty(r.TriggeredBy),
	}
	_ = json.Unmarshal(r.Counts, &out.Counts)
	return out
}

func toJob(v jobs.View) apigen.Job {
	out := apigen.Job{
		Id: v.ID, Name: v.Name, Description: v.Description, Group: v.Group, Running: v.Running,
		Cancellable: v.Cancellable, NextRunAt: v.NextRunAt, Params: make([]apigen.JobParam, len(v.Params)),
		Schedule: apigen.JobSchedule{Enabled: v.Interval > 0, Source: nonEmpty(v.ScheduleSource)},
	}
	if v.Interval > 0 {
		out.Schedule.IntervalSeconds = ptr(int(v.Interval.Seconds()))
	}
	if v.Running {
		out.CurrentRunId = &v.CurrentRunID
	}
	if v.LastRun != nil {
		sum := toRunSummary(*v.LastRun)
		out.LastRun = &sum
	}
	for i, p := range v.Params {
		jp := apigen.JobParam{Name: p.Name, Type: apigen.JobParamType(p.Type), Required: p.Required,
			Description: nonEmpty(p.Description), Default: nonEmpty(p.Default)}
		if len(p.Choices) > 0 {
			jp.Choices = &p.Choices
		}
		out.Params[i] = jp
	}
	return out
}

// ListJobs lists the background jobs.
func (s *Server) ListJobs(ctx context.Context, _ apigen.ListJobsRequestObject) (apigen.ListJobsResponseObject, error) {
	views, err := s.deps.Jobs.List(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListJobs200JSONResponse{Jobs: make([]apigen.Job, len(views))}
	for i, v := range views {
		out.Jobs[i] = toJob(v)
	}
	return out, nil
}

// GetJob returns one job.
func (s *Server) GetJob(ctx context.Context, req apigen.GetJobRequestObject) (apigen.GetJobResponseObject, error) {
	v, err := s.deps.Jobs.Get(ctx, req.JobId)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.GetJob200JSONResponse(toJob(v)), nil
}

// ListJobRuns lists a job's runs.
func (s *Server) ListJobRuns(ctx context.Context, req apigen.ListJobRunsRequestObject) (apigen.ListJobRunsResponseObject, error) {
	at, id, err := parseTimeCursor(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	var before *db.JobRun
	if at != nil {
		before = &db.JobRun{StartedAt: *at, ID: id}
	}
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	limit := pageSize(req.Params.PageSize)
	rows, err := s.deps.Jobs.Runs(ctx, req.JobId, status, before, limit+1)
	if err != nil {
		return nil, jobsError(err)
	}
	out := apigen.ListJobRuns200JSONResponse{Runs: []apigen.JobRunSummary{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.NextPageToken = timeCursor(last.StartedAt, last.ID)
	}
	for _, r := range rows {
		out.Runs = append(out.Runs, toRunSummary(r))
	}
	return out, nil
}

// RunJob starts a job now.
func (s *Server) RunJob(ctx context.Context, req apigen.RunJobRequestObject) (apigen.RunJobResponseObject, error) {
	var params map[string]string
	if req.Body != nil && req.Body.Params != nil {
		params = *req.Body.Params
	}
	r, err := s.deps.Jobs.RunNow(ctx, req.JobId, params, auth.FromContext(ctx).User.Username)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.RunJob202JSONResponse(toRunSummary(r)), nil
}

// GetJobRun returns one run in full.
func (s *Server) GetJobRun(ctx context.Context, req apigen.GetJobRunRequestObject) (apigen.GetJobRunResponseObject, error) {
	r, err := s.deps.Jobs.GetRun(ctx, req.RunId)
	if err != nil {
		return nil, jobsError(err)
	}
	out := apigen.JobRun{Summary: toRunSummary(r), Params: map[string]string{}, Notes: []apigen.JobRunNote{}}
	_ = json.Unmarshal(r.Params, &out.Params)
	var notes []jobs.Note
	_ = json.Unmarshal(r.Notes, &notes)
	for _, n := range notes {
		out.Notes = append(out.Notes, apigen.JobRunNote{At: n.At, Level: n.Level, Message: n.Message, Ref: nonEmpty(n.Ref)})
	}
	if len(r.Progress) > 0 {
		var p apigen.JobProgress
		if json.Unmarshal(r.Progress, &p) == nil {
			out.Progress = &p
		}
	}
	return apigen.GetJobRun200JSONResponse(out), nil
}

// CancelJobRun cancels a running run.
func (s *Server) CancelJobRun(ctx context.Context, req apigen.CancelJobRunRequestObject) (apigen.CancelJobRunResponseObject, error) {
	r, err := s.deps.Jobs.Cancel(ctx, req.RunId)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.CancelJobRun202JSONResponse(toRunSummary(r)), nil
}

func toTask(t db.Task) apigen.Task {
	out := apigen.Task{
		Id: t.ID, Kind: t.Kind, State: apigen.TaskState(t.State), Priority: int(t.Priority),
		Attempts: int(t.Attempts), MaxAttempts: int(t.MaxAttempts), RunAfter: t.RunAfter,
		LastError: nonEmpty(t.LastError), LimitKey: nonEmpty(t.LimitKey), Payload: map[string]any{},
		CreatedAt: t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
	}
	_ = json.Unmarshal(t.Payload, &out.Payload)
	if t.CreatedBy.Valid {
		out.CreatedBy = &t.CreatedBy.UUID
	}
	if len(t.Progress) > 0 {
		var p apigen.JobProgress
		if json.Unmarshal(t.Progress, &p) == nil {
			out.Progress = &p
		}
	}
	return out
}

// ListTasks lists tasks, newest first.
func (s *Server) ListTasks(ctx context.Context, req apigen.ListTasksRequestObject) (apigen.ListTasksResponseObject, error) {
	at, id, err := parseTimeCursor(req.Params.PageToken)
	if err != nil {
		return nil, err
	}
	limit := pageSize(req.Params.PageSize)
	p := db.ListTasksParams{BeforeCreated: at, Lim: int32(limit + 1)} //nolint:gosec // bounded
	if at != nil {
		p.BeforeID = uuid.NullUUID{UUID: id, Valid: true}
	}
	if req.Params.Kind != nil {
		p.Kind.String, p.Kind.Valid = *req.Params.Kind, true
	}
	if req.Params.State != nil {
		p.State.String, p.State.Valid = string(*req.Params.State), true
	}
	rows, err := s.deps.Store.ListTasks(ctx, p)
	if err != nil {
		return nil, err
	}
	out := apigen.ListTasks200JSONResponse{Tasks: []apigen.Task{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.NextPageToken = timeCursor(last.CreatedAt, last.ID)
	}
	for _, t := range rows {
		out.Tasks = append(out.Tasks, toTask(t))
	}
	return out, nil
}

// GetTask returns a task.
func (s *Server) GetTask(ctx context.Context, req apigen.GetTaskRequestObject) (apigen.GetTaskResponseObject, error) {
	t, err := s.deps.Queue.Get(ctx, req.TaskId)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.GetTask200JSONResponse(toTask(t)), nil
}

// CancelTask cancels a task.
func (s *Server) CancelTask(ctx context.Context, req apigen.CancelTaskRequestObject) (apigen.CancelTaskResponseObject, error) {
	t, err := s.deps.Queue.Cancel(ctx, req.TaskId)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.CancelTask200JSONResponse(toTask(t)), nil
}

// RetryTask queues a failed or cancelled task again.
func (s *Server) RetryTask(ctx context.Context, req apigen.RetryTaskRequestObject) (apigen.RetryTaskResponseObject, error) {
	t, err := s.deps.Queue.Retry(ctx, req.TaskId)
	if err != nil {
		return nil, jobsError(err)
	}
	return apigen.RetryTask200JSONResponse(toTask(t)), nil
}

func (s *Server) queueState(ctx context.Context) (apigen.QueueState, error) {
	stats, err := s.deps.Store.TaskStats(ctx)
	if err != nil {
		return apigen.QueueState{}, err
	}
	counts := map[string]map[string]int64{}
	for _, st := range stats {
		if counts[st.Kind] == nil {
			counts[st.Kind] = map[string]int64{}
		}
		counts[st.Kind][st.State] = st.N
	}
	out := apigen.QueueState{Kinds: []apigen.QueueKind{}}
	for _, k := range s.deps.Queue.State() {
		out.Kinds = append(out.Kinds, apigen.QueueKind{
			Kind: k.Kind, Paused: k.Paused, Running: k.Running, Concurrency: k.Concurrency,
			Queued: counts[k.Kind][queue.StateQueued], Failed: counts[k.Kind][queue.StateFailed],
		})
	}
	return out, nil
}

// GetQueue returns the queue per kind.
func (s *Server) GetQueue(ctx context.Context, _ apigen.GetQueueRequestObject) (apigen.GetQueueResponseObject, error) {
	st, err := s.queueState(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.GetQueue200JSONResponse(st), nil
}

// UpdateQueueKind pauses or resumes a kind.
func (s *Server) UpdateQueueKind(ctx context.Context, req apigen.UpdateQueueKindRequestObject) (apigen.UpdateQueueKindResponseObject, error) {
	if err := s.deps.Queue.SetPaused(req.Kind, req.Body.Paused); err != nil {
		return nil, jobsError(err)
	}
	s.deps.Log.InfoContext(ctx, "queue kind changed", "component", "audit", "kind", req.Kind, "paused", req.Body.Paused,
		"actor", auth.FromContext(ctx).User.ID)
	st, err := s.queueState(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateQueueKind200JSONResponse(st), nil
}
