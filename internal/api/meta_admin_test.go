// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"strings"
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
	fake.AddJob(&metav1.Job{Id: "search-indexer", Name: "Index changes", Schedule: &metav1.Schedule{Kind: &metav1.Schedule_Cron{Cron: "@every 1m"}, Enabled: true}})

	list := call[apigen.JobList](admin, http.MethodGet, "/api/v1/system/meta/jobs", nil, 200)
	if len(list.Jobs) != 3 || list.Jobs[2].Schedule.TimeZone != nil || list.Jobs[0].Schedule.Cron == nil || *list.Jobs[0].Schedule.Cron != "30 4 * * *" ||
		*list.Jobs[0].Schedule.Source != "duplicates.schedule" || list.Jobs[0].Params[0].Name != "full" || list.Jobs[1].Schedule.Enabled ||
		list.Jobs[0].Schedule.TimeZone == nil || *list.Jobs[0].Schedule.TimeZone != "UTC" || list.Jobs[1].Schedule.TimeZone != nil {
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

	// A job that can't run now.
	fake.DisableJob("search-rebuild", "the search backend is postgres")
	if p := admin.expectProblem(http.MethodPost, "/api/v1/system/meta/jobs/search-rebuild/run", nil, 409, "job_disabled"); p.Detail == nil || !strings.Contains(*p.Detail, "postgres") {
		t.Fatalf("disabled %+v", p)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/jobs/nope", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/jobs/nope/run", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, "/api/v1/system/meta/jobs", nil, 403, "forbidden")
}

func TestMetaProvidersAndSyncs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	fake := connectedMeta(t, admin)
	fake.AddEntry(&metav1.Entry{Id: "e1", Title: "The Lantern Keeper"})

	call[map[string]any](admin, http.MethodPost, "/api/v1/system/meta/providers/books/sync", map[string]string{"kind": "dump"}, 202)
	provs := call[apigen.MetaProviderList](admin, http.MethodGet, "/api/v1/system/meta/providers", nil, 200)
	if len(provs.Providers) != 1 || provs.Providers[0].Id != "books" || provs.Providers[0].RecordCount != 2 || provs.Providers[0].Health != "ok" || len(provs.Providers[0].LastRuns) != 1 {
		t.Fatalf("providers %+v", provs)
	}
	runs := call[apigen.MetaSyncRunList](admin, http.MethodGet, "/api/v1/system/meta/sync-runs?provider=books", nil, 200)
	if len(runs.Runs) != 1 || runs.Runs[0].Status != "running" || runs.Runs[0].Kind != "dump" || runs.Runs[0].Upserts != 12 {
		t.Fatalf("runs %+v", runs)
	}
	run := call[apigen.MetaSyncRun](admin, http.MethodGet, "/api/v1/system/meta/sync-runs/"+runs.Runs[0].Id, nil, 200)
	if len(run.Notices) != 1 || run.Notices[0].Level != "warning" || run.StartedAt == nil {
		t.Fatalf("run %+v", run)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/sync-runs/nope", nil, 404, CodeNotFound)

	health := call[apigen.MetaServiceHealth](admin, http.MethodGet, "/api/v1/system/meta/health", nil, 200)
	if health.Status != "ok" || len(health.Components) != 2 || health.Components[0].Details["engine"] != "postgres" {
		t.Fatalf("health %+v", health)
	}
	fake.SetProviderDown("books", true)
	if h := call[apigen.MetaServiceHealth](admin, http.MethodGet, "/api/v1/system/meta/health", nil, 200); h.Status != "degraded" || h.Components[1].Status != "down" {
		t.Fatalf("degraded %+v", h)
	}

	ref := call[apigen.MetaEntryRefresh](admin, http.MethodPost, "/api/v1/system/meta/entries/e1/refresh", nil, 200)
	if len(ref.Refreshed) != 0 || ref.Failed["books"] == "" {
		t.Fatalf("refresh %+v", ref)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/entries/missing/refresh", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, "/api/v1/system/meta/providers", nil, 403, "forbidden")
}

func TestMetaDuplicatesReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	fake := connectedMeta(t, admin)
	fake.AddPair(1, "e1", "e2", 0.95)
	fake.AddPair(2, "e3", "e4", 0.4)
	fake.AddPair(3, "e5", "e6", 0.8)

	page := call[apigen.DuplicatePage](admin, http.MethodGet, "/api/v1/system/meta/duplicates", nil, 200)
	if len(page.Pairs) != 3 || page.TotalSize != 3 || page.Pairs[0].EntryA.Id != "e1" || page.Pairs[0].Signals.SharedPeople[0] != "A. Author" ||
		page.Pairs[0].Signals.YearA == nil || *page.Pairs[0].Signals.YearA != 2020 {
		t.Fatalf("open pairs %+v", page)
	}
	if p := call[apigen.DuplicatePage](admin, http.MethodGet, "/api/v1/system/meta/duplicates?min_score=0.5", nil, 200); len(p.Pairs) != 2 {
		t.Fatalf("min score %+v", p)
	}
	if p := call[apigen.DuplicatePair](admin, http.MethodGet, "/api/v1/system/meta/duplicates/2", nil, 200); p.Status != "open" || p.Score < 0.39 {
		t.Fatalf("pair %+v", p)
	}

	// Confirm merges (for good), dismiss and reopen.
	conf := call[apigen.ConfirmedDuplicate](admin, http.MethodPost, "/api/v1/system/meta/duplicates/1/confirm", map[string]string{"keep": "b", "note": "same"}, 200)
	if conf.EntryId != "e2" || conf.Pair.Status != "confirmed" || conf.Pair.DecidedAt == nil || *conf.Pair.Note != "same" {
		t.Fatalf("confirmed %+v", conf)
	}
	admin.expectProblem(http.MethodPost, "/api/v1/system/meta/duplicates/1/reopen", nil, 409, "pair_decided")
	if d := call[apigen.DuplicatePair](admin, http.MethodPost, "/api/v1/system/meta/duplicates/2/dismiss", nil, 200); d.Status != "dismissed" {
		t.Fatalf("dismissed %+v", d)
	}
	if p := call[apigen.DuplicatePage](admin, http.MethodGet, "/api/v1/system/meta/duplicates?status=dismissed", nil, 200); len(p.Pairs) != 1 || p.Pairs[0].Id != 2 {
		t.Fatalf("dismissed list %+v", p)
	}
	if r := call[apigen.DuplicatePair](admin, http.MethodPost, "/api/v1/system/meta/duplicates/2/reopen", nil, 200); r.Status != "open" {
		t.Fatalf("reopened %+v", r)
	}
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/duplicates/99", nil, 404, CodeNotFound)
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/duplicates?page_size=500", nil, 400, CodeInvalidRequest)

	// The decision model: the key is never shown, an empty one keeps it.
	j := call[apigen.DuplicateJudge](admin, http.MethodGet, "/api/v1/system/meta/duplicate-judge", nil, 200)
	if j.Enabled || j.Origin != "config" {
		t.Fatalf("judge %+v", j)
	}
	j = call[apigen.DuplicateJudge](admin, http.MethodPut, "/api/v1/system/meta/duplicate-judge", map[string]any{"enabled": true, "kind": "local", "api_key": "sk-123", "covers": true}, 200)
	if !j.Enabled || !j.HasApiKey || j.Origin != "admin" || fake.JudgeKey() != "sk-123" {
		t.Fatalf("saved %+v", j)
	}
	call[apigen.DuplicateJudge](admin, http.MethodPut, "/api/v1/system/meta/duplicate-judge", map[string]any{"enabled": true, "kind": "local"}, 200)
	if fake.JudgeKey() != "sk-123" {
		t.Fatal("an empty key replaced the stored one")
	}
	if strings.Contains(string(admin.do(http.MethodGet, "/api/v1/system/meta/duplicate-judge", nil).body), "sk-123") {
		t.Fatal("the key is shown")
	}
	if tr := call[apigen.DuplicateJudgeTest](admin, http.MethodPost, "/api/v1/system/meta/duplicate-judge/test", map[string]any{"enabled": true, "kind": "local"}, 200); !tr.Ok || tr.Verdict == nil || tr.Verdict.Choice != "same_work" {
		t.Fatalf("test %+v", tr)
	}
	if tr := call[apigen.DuplicateJudgeTest](admin, http.MethodPost, "/api/v1/system/meta/duplicate-judge/test", map[string]any{"enabled": true, "kind": "broken"}, 200); tr.Ok || tr.Error == nil {
		t.Fatalf("broken test %+v", tr)
	}
	if r := call[apigen.DuplicateJudge](admin, http.MethodPost, "/api/v1/system/meta/duplicate-judge/reset", nil, 200); r.Origin != "config" || r.HasApiKey {
		t.Fatalf("reset %+v", r)
	}
	user.expectProblem(http.MethodGet, "/api/v1/system/meta/duplicates", nil, 403, "forbidden")
}

func TestMetaSearchEngine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	fake := connectedMeta(t, admin)

	se := call[apigen.MetaSearchEngine](admin, http.MethodGet, "/api/v1/system/meta/search-engine", nil, 200)
	if se.Settings.Backend != "postgres" || se.Settings.Origin != "config" || !se.Status.Ready || se.Status.Documents != 42 || se.Status.LagSeconds == nil {
		t.Fatalf("search engine %+v", se)
	}
	body := map[string]any{"backend": "typesense", "url": "http://typesense:8108", "api_key": "ts-secret", "browse": "facets"}
	if tr := call[apigen.SearchEngineTest](admin, http.MethodPost, "/api/v1/system/meta/search-engine/test", body, 200); !tr.Ok {
		t.Fatalf("test %+v", tr)
	}
	if tr := call[apigen.SearchEngineTest](admin, http.MethodPost, "/api/v1/system/meta/search-engine/test", map[string]any{"backend": "typesense", "url": "http://nowhere:1"}, 200); tr.Ok || tr.Error == nil {
		t.Fatalf("failed test %+v", tr)
	}
	se = call[apigen.MetaSearchEngine](admin, http.MethodPut, "/api/v1/system/meta/search-engine", body, 200)
	if se.Settings.Backend != "typesense" || !se.Settings.HasApiKey || se.Settings.Origin != "admin" || fake.SearchKey() != "ts-secret" {
		t.Fatalf("saved %+v", se)
	}
	call[apigen.MetaSearchEngine](admin, http.MethodPut, "/api/v1/system/meta/search-engine", map[string]any{"backend": "typesense", "url": "http://typesense:8108"}, 200)
	if fake.SearchKey() != "ts-secret" {
		t.Fatal("an empty key replaced the stored one")
	}
	if strings.Contains(string(admin.do(http.MethodGet, "/api/v1/system/meta/search-engine", nil).body), "ts-secret") {
		t.Fatal("the key is shown")
	}
	admin.expectProblem(http.MethodPut, "/api/v1/system/meta/search-engine", map[string]any{"backend": "meilisearch"}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodPut, "/api/v1/system/meta/search-engine", map[string]any{"backend": "elastic"}, 400, CodeInvalidRequest)
	if r := call[apigen.MetaSearchEngine](admin, http.MethodPost, "/api/v1/system/meta/search-engine/reset", nil, 200); r.Settings.Origin != "config" || r.Settings.HasApiKey {
		t.Fatalf("reset %+v", r)
	}
}

func TestCronZone(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]string{
		"0 3 * * *":                        "",
		"CRON_TZ=Europe/Vilnius 0 3 * * *": "Europe/Vilnius",
		"TZ=America/New_York 30 4 * * *":   "America/New_York",
	} {
		if got := cronZone(expr); got != want {
			t.Errorf("cronZone(%q) = %q, want %q", expr, got, want)
		}
	}
}
