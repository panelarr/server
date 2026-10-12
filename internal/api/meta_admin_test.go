// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"testing"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta/metatest"
)

// connectedMeta connects a fake meta with one provider.
func connectedMeta(t *testing.T, admin *client) *metatest.Fake {
	t.Helper()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 2})
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}, 200)
	return fake
}

func TestMetaJobs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/jobs", nil, 503, "meta_unavailable")
	fake := connectedMeta(t, admin)
	fake.AddJob(&metav1.Job{
		Id: "duplicates", Name: "Find duplicates", Group: "Catalogue", Cancellable: true,
		Schedule: &metav1.Schedule{Kind: &metav1.Schedule_Cron{Cron: "30 4 * * *"}, Enabled: true, Source: "duplicates.schedule"},
		Params:   []*metav1.JobParam{{Name: "full", Type: "bool", Default: "false"}},
	})
	fake.AddJob(&metav1.Job{Id: "search-rebuild", Name: "Rebuild the search index"})

	list := call[apigen.JobList](admin, http.MethodGet, "/api/v1/system/meta/jobs", nil, 200)
	if len(list.Jobs) != 2 || list.Jobs[0].Schedule.Cron == nil || *list.Jobs[0].Schedule.Cron != "30 4 * * *" ||
		*list.Jobs[0].Schedule.Source != "duplicates.schedule" || list.Jobs[0].Params[0].Name != "full" || list.Jobs[1].Schedule.Enabled {
		t.Fatalf("jobs %+v", list)
	}

	run := call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/system/meta/jobs/duplicates/run", map[string]any{"params": map[string]string{"full": "true"}}, 202)
	if run.Status != apigen.JobRunStatusRunning || run.Trigger != apigen.JobRunSummaryTriggerManual || run.JobId != "duplicates" {
		t.Fatalf("run %+v", run)
	}
	p := admin.expectProblem(http.MethodPost, "/api/v1/system/meta/jobs/duplicates/run", nil, 409, "already_running")
	if p.CurrentRunId == nil || *p.CurrentRunId != run.Id {
		t.Fatalf("busy %+v", p)
	}
	if j := call[apigen.Job](admin, http.MethodGet, "/api/v1/system/meta/jobs/duplicates", nil, 200); !j.Running || j.CurrentRunId == nil || *j.CurrentRunId != run.Id {
		t.Fatalf("job %+v", j)
	}
	full := call[apigen.JobRun](admin, http.MethodGet, "/api/v1/system/meta/job-runs/"+run.Id.String(), nil, 200)
	if full.Params["full"] != "true" || full.Progress == nil || full.Progress.Done != 1 || *full.Progress.Total != 10 {
		t.Fatalf("run in full %+v", full)
	}
	if c := call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/system/meta/job-runs/"+run.Id.String()+"/cancel", nil, 202); c.Status != apigen.JobRunStatusCancelled {
		t.Fatalf("cancelled %+v", c)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/job-runs/"+run.Id.String()+"/cancel", nil, 409, "not_running")
	runs := call[apigen.JobRunList](admin, http.MethodGet, "/api/v1/system/meta/jobs/duplicates/runs?status=cancelled", nil, 200)
	if len(runs.Runs) != 1 || runs.Runs[0].Id != run.Id {
		t.Fatalf("runs %+v", runs)
	}

	// The search index rebuild is a job too.
	rb := call[apigen.JobRunSummary](admin, http.MethodPost, "/api/v1/system/meta/jobs/search-rebuild/run", nil, 202)
	fake.FinishRun(rb.Id.String())
	if r := call[apigen.JobRun](admin, http.MethodGet, "/api/v1/system/meta/job-runs/"+rb.Id.String(), nil, 200); r.Summary.Status != apigen.JobRunStatusSucceeded {
		t.Fatalf("rebuild %+v", r.Summary)
	}

	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/jobs/nope", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/jobs/nope/run", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, "/api/v1/system/meta/jobs", nil, 403, "forbidden")
}
