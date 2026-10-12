// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
)

// meta's jobs follow the same model as the server's (the shared jobs
// contract), so Panelarr shows them in the same jobs view.

// Job errors.
var (
	ErrJobNotFound      = errors.New("meta: no such job or run")
	ErrJobNotRunning    = errors.New("meta: the run has finished")
	ErrJobNotCancelable = errors.New("meta: this job can't be cancelled")
)

// JobRunningError is a job that is running already.
type JobRunningError struct{ RunID string }

func (e *JobRunningError) Error() string { return "meta: the job is running as " + e.RunID }

// BadJobParamsError is a run meta refused for its params.
type BadJobParamsError struct{ Reason string }

func (e *BadJobParamsError) Error() string { return "meta: " + e.Reason }

func jobError(op string, err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) || !connect.IsWireError(err) {
		return wrap(op, err)
	}
	switch ce.Code() {
	case connect.CodeNotFound:
		return ErrJobNotFound
	case connect.CodeAlreadyExists:
		for _, d := range ce.Details() {
			if v, derr := d.Value(); derr == nil {
				if busy, ok := v.(*metav1.JobAlreadyRunning); ok {
					return &JobRunningError{RunID: busy.GetCurrentRunId()}
				}
			}
		}
		return &JobRunningError{}
	case connect.CodeInvalidArgument:
		return &BadJobParamsError{Reason: ce.Message()}
	case connect.CodeFailedPrecondition:
		return ErrJobNotCancelable
	}
	return wrap(op, providerError(err, ""))
}

// Jobs lists meta's jobs.
func (s *Service) Jobs(ctx context.Context) ([]*metav1.Job, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Jobs.ListJobs(ctx, connect.NewRequest(&metav1.ListJobsRequest{}))
	if err != nil {
		return nil, jobError("list jobs", err)
	}
	return res.Msg.GetJobs(), nil
}

// Job returns one of meta's jobs.
func (s *Service) Job(ctx context.Context, id string) (*metav1.Job, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Jobs.GetJob(ctx, connect.NewRequest(&metav1.GetJobRequest{JobId: id}))
	if err != nil {
		return nil, jobError("get job", err)
	}
	return res.Msg.GetJob(), nil
}

// JobRuns lists a job's runs, newest first.
func (s *Service) JobRuns(ctx context.Context, id string, status metav1.JobRunStatus, pageSize int32, token string) ([]*metav1.JobRunSummary, string, error) {
	c, err := s.Client()
	if err != nil {
		return nil, "", err
	}
	res, err := c.Jobs.ListJobRuns(ctx, connect.NewRequest(&metav1.ListJobRunsRequest{
		JobId: id, Status: status, PageSize: pageSize, PageToken: token,
	}))
	if err != nil {
		return nil, "", jobError("list job runs", err)
	}
	return res.Msg.GetRuns(), res.Msg.GetNextPageToken(), nil
}

// JobRun returns one run in full.
func (s *Service) JobRun(ctx context.Context, runID string) (*metav1.JobRun, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Jobs.GetJobRun(ctx, connect.NewRequest(&metav1.GetJobRunRequest{RunId: runID}))
	if err != nil {
		return nil, jobError("get job run", err)
	}
	return res.Msg.GetRun(), nil
}

// RunJob starts one of meta's jobs now.
func (s *Service) RunJob(ctx context.Context, id string, params map[string]string) (*metav1.JobRunSummary, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Jobs.RunJob(ctx, connect.NewRequest(&metav1.RunJobRequest{JobId: id, Params: params}))
	if err != nil {
		return nil, jobError("run job", err)
	}
	return res.Msg.GetRun(), nil
}

// CancelJobRun cancels a running run.
func (s *Service) CancelJobRun(ctx context.Context, runID string) (*metav1.JobRunSummary, error) {
	run, err := s.JobRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.GetSummary().GetStatus() != metav1.JobRunStatus_JOB_RUN_STATUS_RUNNING {
		return nil, ErrJobNotRunning
	}
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := c.Jobs.CancelJobRun(ctx, connect.NewRequest(&metav1.CancelJobRunRequest{RunId: runID}))
	if err != nil {
		return nil, jobError("cancel job run", err)
	}
	return res.Msg.GetRun(), nil
}
