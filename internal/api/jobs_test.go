// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/queue"
)

func TestJobsAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()

	list := call[apigen.JobList](admin, http.MethodGet, "/api/v1/jobs", nil, 200)
	if len(list.Jobs) != 1 || list.Jobs[0].Id != "demo.job" || list.Jobs[0].Params[0].Default == nil {
		t.Fatalf("jobs %+v", list)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/jobs/demo.job/run", map[string]any{"params": map[string]string{"level": "high"}}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPost, "/api/v1/jobs/nope/run", nil, 404, CodeNotFound)

	run := call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/jobs/demo.job/run", map[string]any{"params": map[string]string{"level": "3"}}, 202)
	if run.Status != apigen.JobRunStatusRunning || run.Trigger != apigen.JobRunSummaryTriggerManual || run.TriggeredBy == nil || *run.TriggeredBy != "Admin" {
		t.Fatalf("run %+v", run)
	}
	p := admin.expectProblem(http.MethodPost, "/api/v1/jobs/demo.job/run", nil, 409, "already_running")
	if !strings.Contains(*p.Detail, run.Id.String()) {
		t.Fatalf("detail %q", *p.Detail)
	}
	job := call[apigen.Job](admin, http.MethodGet, "/api/v1/jobs/demo.job", nil, 200)
	if !job.Running || job.CurrentRunId == nil || *job.CurrentRunId != run.Id {
		t.Fatalf("job %+v", job)
	}
	call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/job-runs/"+run.Id.String()+"/cancel", nil, 202)
	var full apigen.JobRun
	deadline := time.Now().Add(5 * time.Second)
	for {
		full = call[apigen.JobRun](admin, http.MethodGet, "/api/v1/job-runs/"+run.Id.String(), nil, 200)
		if full.Summary.Status != apigen.JobRunStatusRunning || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if full.Summary.Status != apigen.JobRunStatusCancelled || full.Params["level"] != "3" || full.Summary.Counts["seen"] != 1 {
		t.Fatalf("run %+v", full)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/job-runs/"+run.Id.String()+"/cancel", nil, 409, "not_running")
	for deadline := time.Now().Add(5 * time.Second); call[apigen.Job](admin, http.MethodGet, "/api/v1/jobs/demo.job", nil, 200).Running; {
		if time.Now().After(deadline) {
			t.Fatal("job still running after its run was cancelled")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A finished run, then paging through runs.
	second := call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/jobs/demo.job/run", nil, 202)
	close(h.release)
	waitJobDone(t, admin, second.Id.String())
	page := call[apigen.JobRunList](admin, http.MethodGet, "/api/v1/jobs/demo.job/runs?page_size=1", nil, 200)
	if len(page.Runs) != 1 || page.Runs[0].Id != second.Id || page.NextPageToken == "" {
		t.Fatalf("page 1 %+v", page)
	}
	page = call[apigen.JobRunList](admin, http.MethodGet, "/api/v1/jobs/demo.job/runs?page_size=1&page_token="+page.NextPageToken, nil, 200)
	if len(page.Runs) != 1 || page.Runs[0].Id != run.Id || page.NextPageToken != "" {
		t.Fatalf("page 2 %+v", page)
	}
	if f := call[apigen.JobRunList](admin, http.MethodGet, "/api/v1/jobs/demo.job/runs?status=cancelled", nil, 200); len(f.Runs) != 1 {
		t.Fatalf("filtered %+v", f)
	}
}

func waitJobDone(t *testing.T, c *client, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := call[apigen.JobRun](c, http.MethodGet, "/api/v1/job-runs/"+id, nil, 200)
		if r.Summary.Status != apigen.JobRunStatusRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s still running", id)
}

func waitTask(t *testing.T, c *client, id string, state apigen.TaskState) apigen.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := call[apigen.Task](c, http.MethodGet, "/api/v1/tasks/"+id, nil, 200)
		if task.State == state {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: %s, want %s", id, task.State, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTasksAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, me := h.setupAdmin()
	ctx := context.Background()

	ok, _, err := h.queue.Enqueue(ctx, queue.NewTask{Kind: "demo.task", CreatedBy: me.Id, LimitKey: "source:x:1"})
	if err != nil {
		t.Fatal(err)
	}
	bad, _, _ := h.queue.Enqueue(ctx, queue.NewTask{Kind: "demo.task", Payload: map[string]bool{"fail": true}})
	waitTask(t, admin, ok.ID.String(), apigen.TaskStateSucceeded)
	failed := waitTask(t, admin, bad.ID.String(), apigen.TaskStateFailed)
	if failed.LastError == nil || *failed.LastError != "demo failure" || failed.Payload["fail"] != true {
		t.Fatalf("failed %+v", failed)
	}

	list := call[apigen.TaskList](admin, http.MethodGet, "/api/v1/tasks?state=failed", nil, 200)
	if len(list.Tasks) != 1 || list.Tasks[0].Id != bad.ID {
		t.Fatalf("failed list %+v", list)
	}
	all := call[apigen.TaskList](admin, http.MethodGet, "/api/v1/tasks?kind=demo.task&page_size=1", nil, 200)
	if len(all.Tasks) != 1 || all.NextPageToken == "" {
		t.Fatalf("page %+v", all)
	}
	state := call[apigen.QueueState](admin, http.MethodGet, "/api/v1/queue", nil, 200)
	if len(state.Kinds) != 1 || state.Kinds[0].Failed != 1 {
		t.Fatalf("queue %+v", state)
	}

	// Pausing holds new tasks; retry and cancel work on them.
	state = call[apigen.QueueState](admin, http.MethodPatch, "/api/v1/queue/kinds/demo.task", map[string]bool{"paused": true}, 200)
	if !state.Kinds[0].Paused {
		t.Fatal("not paused")
	}
	admin.expectProblem(http.MethodPatch, "/api/v1/queue/kinds/nope", map[string]bool{"paused": true}, 404, CodeNotFound)
	retried := call[apigen.Task](admin, http.MethodPost, "/api/v1/tasks/"+bad.ID.String()+"/retry", nil, 200)
	if retried.State != apigen.TaskStateQueued || retried.Attempts != 0 {
		t.Fatalf("retried %+v", retried)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/tasks/"+bad.ID.String()+"/retry", nil, 409, "not_retryable")
	cancelled := call[apigen.Task](admin, http.MethodPost, "/api/v1/tasks/"+bad.ID.String()+"/cancel", nil, 200)
	if cancelled.State != apigen.TaskStateCancelled {
		t.Fatalf("cancelled %+v", cancelled)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/tasks/"+ok.ID.String()+"/cancel", nil, 409, "task_finished")
	admin.expectProblem(http.MethodGet, "/api/v1/tasks/00000000-0000-0000-0000-000000000001", nil, 404, CodeNotFound)
}

// readEvents opens the event stream and returns the next n event types.
func readEvents(t *testing.T, c *client, n int, trigger func()) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.h.srv.URL+"/api/v1/events", nil)
	if c.apiKey != "" {
		req.Header.Set(APIKeyHeader, c.apiKey)
	}
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(res.Body)
	var types []string
	triggered := false
	for sc.Scan() {
		line := sc.Text()
		if typ, ok := strings.CutPrefix(line, "event: "); ok {
			types = append(types, typ)
			if typ == "ready" && !triggered {
				triggered = true
				go trigger()
			}
			if len(types) >= n {
				return types
			}
		}
	}
	return types
}

func TestEventStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, u := h.createUser(admin, "reader", nil)
	h.client().expectProblem(http.MethodGet, "/api/v1/events", nil, 401, "unauthenticated")

	// A user sees their own task's events.
	got := readEvents(t, user, 3, func() {
		_, _, _ = h.queue.Enqueue(context.Background(), queue.NewTask{Kind: "demo.task", CreatedBy: u.Id})
	})
	if len(got) < 3 || got[0] != "ready" || got[1] != "task.queued" || got[2] != "task.started" {
		t.Fatalf("user events %v", got)
	}

	// Admins see job events; a user doesn't see others' tasks.
	adminKey := h.keyClient(admin.newAPIKey("read"))
	got = readEvents(t, adminKey, 2, func() {
		call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/jobs/demo.job/run", nil, 202)
	})
	if len(got) < 2 || got[1] != "job.started" {
		t.Fatalf("admin events %v", got)
	}
	sub := h.hub.Subscribe(eventsViewer(u.Id, false))
	defer sub.Close()
	_, _, _ = h.queue.Enqueue(context.Background(), queue.NewTask{Kind: "demo.task"})
	time.Sleep(100 * time.Millisecond)
	if len(sub.C) != 0 {
		t.Fatalf("user saw someone else's task: %v", (<-sub.C).Type)
	}
}

func eventsViewer(id uuid.UUID, admin bool) events.Viewer {
	return events.Viewer{UserID: id, Admin: admin}
}
