// SPDX-License-Identifier: GPL-3.0-or-later

// Package metatest runs a fake metadata service for tests: the real gRPC
// API over cleartext HTTP/2, with keys, providers and entries set by the
// test. All data is made up.
package metatest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	"github.com/panelarr/meta/gen/panelarr/meta/v1/metav1connect"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
)

// Fake is a running fake metadata service.
type Fake struct {
	URL string

	mu        sync.Mutex
	keys      map[string][]string // key → scopes
	providers []*metav1.Provider
	entries   []*metav1.Entry
	syncs     []*metav1.RunSyncRequest
	lastQuery *metav1.EntryFilter
	down      bool
	// providerDown: RunSync answers that the provider's plugin is down,
	// with the explicit reason when explicit is set.
	providerDown map[string]bool
	explicit     bool
	// failWith makes catalogue calls answer with this error code.
	failWith connect.Code
	// legacy: no jobs API (an older service).
	legacy bool
	// delay slows every call down.
	delay time.Duration
	// via records which API started each sync: "jobs" or "admin".
	via []string
	// pushed are network profiles pushed per provider; unsupported are
	// providers whose plugin takes no profiles.
	// pairs are suspected duplicates; judge the decision model settings
	// (judgeKey its API key, never returned).
	pairs []*metav1.DuplicatePair
	// search is the saved search engine settings (searchKey its key).
	search    *metav1.SearchEngineSettings
	searchKey string
	judge     *metav1.DecisionJudgeSettings
	judgeKey  string
	// syncRuns are the sync runs started, oldest first.
	syncRuns []*metav1.SyncRun
	// jobs and runs are meta's own jobs (not syncs), runs newest last.
	jobs        []*metav1.Job
	runs        []*metav1.JobRun
	pushed      map[string]*metadatav1.NetworkProfile
	pushedAt    map[string]time.Time
	unsupported map[string]bool
}

// New starts a fake with one admin key, AdminKey, and no providers.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{keys: map[string][]string{AdminKey: {"admin"}}}
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(f.auth())
	mux.Handle(metav1connect.NewMetaServiceHandler(&metaHandler{f: f}, opts))
	mux.Handle(metav1connect.NewMetaAdminServiceHandler(&adminHandler{f: f}, opts))
	mux.Handle(metav1connect.NewMetaClientsServiceHandler(&clientsHandler{f: f}, opts))
	mux.Handle(metav1connect.NewMetaJobsServiceHandler(&jobsHandler{f: f}, opts))
	mux.Handle(metav1connect.NewMetaIntegrationsServiceHandler(&integrationsHandler{f: f}, opts))
	mux.Handle(metav1connect.NewMetaDuplicatesServiceHandler(&duplicatesHandler{f: f}, opts))
	// Down means no answer at all: the connection is cut.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			panic(http.ErrAbortHandler)
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// AdminKey is the fake's admin key.
const AdminKey = "mtk_test_admin"

// AddKey adds a key with scopes.
func (f *Fake) AddKey(key string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = scopes
}

// SetProviders replaces the providers: id → record count.
func (f *Fake) SetProviders(records map[string]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers = nil
	for id, n := range records {
		f.providers = append(f.providers, &metav1.Provider{
			Id: id, RecordCount: n, Health: metadatav1.HealthStatus_HEALTH_STATUS_OK,
		})
	}
	slices.SortFunc(f.providers, func(a, b *metav1.Provider) int { return strings.Compare(a.Id, b.Id) })
}

// AddEntry adds an entry to the catalogue.
func (f *Fake) AddEntry(e *metav1.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
}

// SetProviderDown makes RunSync for a provider answer that its plugin
// can't be reached, as the metadata service does; explicit adds the
// structured reason (ErrorInfo PROVIDER_UNAVAILABLE with the provider).
func (f *Fake) SetProviderDown(provider string, explicit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.providerDown == nil {
		f.providerDown = map[string]bool{}
	}
	f.providerDown[provider] = true
	f.explicit = explicit
}

// FailWith makes catalogue calls (search, browse, entries) answer with an
// error of this code; 0 stops it.
func (f *Fake) FailWith(code connect.Code) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = code
}

// SetLegacy makes the service lack the jobs API, like an older version.
func (f *Fake) SetLegacy(legacy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.legacy = legacy
}

// SetDelay slows every call down.
func (f *Fake) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// SyncVia returns which API started each sync, in order.
func (f *Fake) SyncVia() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.via)
}

// SetDown makes the service unreachable: connections are cut without an
// answer.
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// Syncs returns the syncs started so far.
func (f *Fake) Syncs() []*metav1.RunSyncRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

// LastFilter returns the filter of the last search or browse.
func (f *Fake) LastFilter() *metav1.EntryFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery
}

func (f *Fake) auth() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			f.mu.Lock()
			scopes, ok := f.keys[strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ")]
			delay := f.delay
			f.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if !ok {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown key"))
			}
			return next(context.WithValue(ctx, scopesKey{}, scopes), req)
		}
	}
}

type scopesKey struct{}

type clientsHandler struct {
	metav1connect.UnimplementedMetaClientsServiceHandler
	f *Fake
}

func (h *clientsHandler) WhoAmI(ctx context.Context, _ *connect.Request[metav1.WhoAmIRequest]) (*connect.Response[metav1.WhoAmIResponse], error) {
	scopes, _ := ctx.Value(scopesKey{}).([]string)
	return connect.NewResponse(&metav1.WhoAmIResponse{Name: "test client", Scopes: scopes}), nil
}

type adminHandler struct {
	metav1connect.UnimplementedMetaAdminServiceHandler
	f *Fake
}

func requireAdmin(ctx context.Context) error {
	scopes, _ := ctx.Value(scopesKey{}).([]string)
	if !slices.Contains(scopes, "admin") {
		return connect.NewError(connect.CodePermissionDenied, errors.New("needs admin"))
	}
	return nil
}

func (h *adminHandler) ListProviders(ctx context.Context, _ *connect.Request[metav1.ListProvidersRequest]) (*connect.Response[metav1.ListProvidersResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	res := &metav1.ListProvidersResponse{}
	for _, p := range h.f.providers {
		p = proto.Clone(p).(*metav1.Provider)
		for i := len(h.f.syncRuns) - 1; i >= 0 && len(p.LastRuns) < 3; i-- {
			if r := h.f.syncRuns[i]; r.GetProvider() == p.GetId() {
				p.LastRuns = append(p.LastRuns, r)
			}
		}
		res.Providers = append(res.Providers, p)
	}
	return connect.NewResponse(res), nil
}

func (h *adminHandler) ListSyncRuns(ctx context.Context, req *connect.Request[metav1.ListSyncRunsRequest]) (*connect.Response[metav1.ListSyncRunsResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	res := &metav1.ListSyncRunsResponse{}
	for i := len(h.f.syncRuns) - 1; i >= 0; i-- {
		if r := h.f.syncRuns[i]; req.Msg.GetProvider() == "" || r.GetProvider() == req.Msg.GetProvider() {
			res.Runs = append(res.Runs, r)
		}
	}
	return connect.NewResponse(res), nil
}

func (h *adminHandler) GetSyncRun(ctx context.Context, req *connect.Request[metav1.GetSyncRunRequest]) (*connect.Response[metav1.GetSyncRunResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for _, r := range h.f.syncRuns {
		if r.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&metav1.GetSyncRunResponse{Run: r}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such run"))
}

func (h *adminHandler) GetHealth(ctx context.Context, _ *connect.Request[metav1.GetHealthRequest]) (*connect.Response[metav1.GetHealthResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	res := &metav1.GetHealthResponse{Status: metadatav1.HealthStatus_HEALTH_STATUS_OK, Version: "test", Components: []*metav1.ComponentHealth{
		{Name: "database", Kind: "database", Status: metadatav1.HealthStatus_HEALTH_STATUS_OK, Details: map[string]string{"engine": "postgres"}},
	}}
	for _, p := range h.f.providers {
		st := metadatav1.HealthStatus_HEALTH_STATUS_OK
		if h.f.providerDown[p.GetId()] {
			st, res.Status = metadatav1.HealthStatus_HEALTH_STATUS_DOWN, metadatav1.HealthStatus_HEALTH_STATUS_DEGRADED
		}
		res.Components = append(res.Components, &metav1.ComponentHealth{Name: "provider:" + p.GetId(), Kind: "provider", Status: st})
	}
	return connect.NewResponse(res), nil
}

func (h *adminHandler) RefreshEntry(ctx context.Context, req *connect.Request[metav1.RefreshEntryRequest]) (*connect.Response[metav1.RefreshEntryResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for _, e := range h.f.entries {
		if e.GetId() == req.Msg.GetId() {
			res := &metav1.RefreshEntryResponse{Failed: map[string]string{}}
			for _, p := range h.f.providers {
				if h.f.providerDown[p.GetId()] {
					res.Failed[p.GetId()] = "plugin is not connected"
				} else {
					res.Refreshed = append(res.Refreshed, p.GetId())
				}
			}
			return connect.NewResponse(res), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such entry"))
}

func (h *adminHandler) RunSync(ctx context.Context, req *connect.Request[metav1.RunSyncRequest]) (*connect.Response[metav1.RunSyncResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	run, err := h.f.startSync(req.Msg.GetProvider(), req.Msg.GetKind(), "admin")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&metav1.RunSyncResponse{Run: run}), nil
}

// startSync starts a sync as the service does, or answers why it can't.
func (f *Fake) startSync(provider, kind, via string) (*metav1.SyncRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := slices.ContainsFunc(f.providers, func(p *metav1.Provider) bool { return p.Id == provider })
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such provider"))
	}
	if f.providerDown[provider] {
		cerr := connect.NewError(connect.CodeUnavailable, errors.New("plugin is not connected"))
		if f.explicit {
			if d, err := connect.NewErrorDetail(&errdetails.ErrorInfo{
				Reason: "PROVIDER_UNAVAILABLE", Domain: "panelarr.meta", Metadata: map[string]string{"provider": provider},
			}); err == nil {
				cerr.AddDetail(d)
			}
		}
		return nil, cerr
	}
	f.syncs = append(f.syncs, &metav1.RunSyncRequest{Provider: provider, Kind: kind})
	f.via = append(f.via, via)
	run := &metav1.SyncRun{
		Id: "run-" + strconv.Itoa(len(f.syncs)), Provider: provider, Kind: kind, Status: metav1.SyncRunStatus_SYNC_RUN_STATUS_RUNNING,
		Trigger: "manual", StartedAt: timestamppb.Now(), Upserts: 12, Changed: 3,
		Notices: []*metav1.SyncRunNotice{{Level: metadatav1.NoticeLevel_NOTICE_LEVEL_WARNING, Message: "one record without a title"}},
	}
	f.syncRuns = append(f.syncRuns, run)
	return run, nil
}

type jobsHandler struct {
	metav1connect.UnimplementedMetaJobsServiceHandler
	f *Fake
}

func (h *jobsHandler) RunJob(ctx context.Context, req *connect.Request[metav1.RunJobRequest]) (*connect.Response[metav1.RunJobResponse], error) {
	h.f.mu.Lock()
	legacy := h.f.legacy
	h.f.mu.Unlock()
	if legacy {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("no jobs API"))
	}
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if run, ok, err := h.f.runJob(req.Msg.GetJobId(), req.Msg.GetParams()); ok {
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&metav1.RunJobResponse{Run: run}), nil
	}
	parts := strings.Split(req.Msg.GetJobId(), ".")
	if len(parts) != 3 || parts[0] != "sync" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no job "+req.Msg.GetJobId()))
	}
	run, err := h.f.startSync(parts[1], parts[2], "jobs")
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no job "+req.Msg.GetJobId()))
		}
		return nil, err
	}
	return connect.NewResponse(&metav1.RunJobResponse{Run: &metav1.JobRunSummary{Id: run.GetId(), JobId: req.Msg.GetJobId(), TriggeredBy: "test client"}}), nil
}

type metaHandler struct {
	metav1connect.UnimplementedMetaServiceHandler
	f *Fake
}

func summary(e *metav1.Entry) *metav1.EntrySummary {
	var cover *metadatav1.Image
	for _, im := range e.GetImages() {
		if im.GetKind() == metadatav1.ImageKind_IMAGE_KIND_COVER {
			cover = im
			break
		}
	}
	return &metav1.EntrySummary{
		Id: e.GetId(), Title: e.GetTitle(), Type: e.GetType(), Status: e.GetStatus(),
		ContentRating: e.GetContentRating(), Year: e.GetDates().GetStart().GetYear(), Cover: cover,
	}
}

func (h *metaHandler) failure() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if h.f.failWith != 0 {
		return connect.NewError(h.f.failWith, errors.New("failing for the test"))
	}
	return nil
}

func (h *metaHandler) match(f *metav1.EntryFilter, text string) ([]*metav1.Entry, bool) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.lastQuery = f
	noProviders := len(h.f.providers) == 0
	var out []*metav1.Entry
	for _, e := range h.f.entries {
		if len(f.GetContentRatings()) > 0 && !slices.Contains(f.GetContentRatings(), e.GetContentRating()) {
			continue
		}
		if len(f.GetTypes()) > 0 && !slices.Contains(f.GetTypes(), e.GetType()) {
			continue
		}
		if text != "" && !slices.ContainsFunc(e.GetTitles(), func(t *metadatav1.Title) bool {
			return strings.Contains(strings.ToLower(t.GetText()), strings.ToLower(text))
		}) && !strings.Contains(strings.ToLower(e.GetTitle()), strings.ToLower(text)) {
			continue
		}
		out = append(out, e)
	}
	return out, noProviders
}

func (h *metaHandler) Search(_ context.Context, req *connect.Request[metav1.SearchRequest]) (*connect.Response[metav1.SearchResponse], error) {
	if err := h.failure(); err != nil {
		return nil, err
	}
	entries, noProviders := h.match(req.Msg.GetFilter(), req.Msg.GetQuery())
	res := &metav1.SearchResponse{NoProviders: noProviders}
	for _, e := range entries {
		res.Hits = append(res.Hits, &metav1.SearchHit{Entry: summary(e), Score: 1, Matched: req.Msg.GetQuery()})
	}
	res.TotalSize = int64(len(res.Hits))
	return connect.NewResponse(res), nil
}

func (h *metaHandler) Browse(_ context.Context, req *connect.Request[metav1.BrowseRequest]) (*connect.Response[metav1.BrowseResponse], error) {
	if err := h.failure(); err != nil {
		return nil, err
	}
	entries, noProviders := h.match(req.Msg.GetFilter(), "")
	res := &metav1.BrowseResponse{NoProviders: noProviders}
	for _, e := range entries {
		res.Entries = append(res.Entries, summary(e))
	}
	res.TotalSize = int64(len(res.Entries))
	return connect.NewResponse(res), nil
}

func (h *metaHandler) GetEntry(_ context.Context, req *connect.Request[metav1.GetEntryRequest]) (*connect.Response[metav1.GetEntryResponse], error) {
	if err := h.failure(); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for _, e := range h.f.entries {
		if e.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&metav1.GetEntryResponse{Entry: e}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such entry"))
}

// SetUnsupported marks a provider whose plugin takes no network profiles.
func (f *Fake) SetUnsupported(provider string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unsupported == nil {
		f.unsupported = map[string]bool{}
	}
	f.unsupported[provider] = true
}

// Pushed returns the profile pushed to a provider (password included), or
// nil.
func (f *Fake) Pushed(provider string) *metadatav1.NetworkProfile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushed[provider]
}

// ForgetPushed drops every pushed profile, like a reset meta.
func (f *Fake) ForgetPushed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed, f.pushedAt = nil, nil
}

type integrationsHandler struct {
	metav1connect.UnimplementedMetaIntegrationsServiceHandler
	f *Fake
}

func (h *integrationsHandler) ListAssignments(context.Context, *connect.Request[metav1.ListAssignmentsRequest]) (*connect.Response[metav1.ListAssignmentsResponse], error) {
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &metav1.ListAssignmentsResponse{}
	for _, p := range f.providers {
		a := &metav1.Assignment{ProviderId: p.GetId(), Supported: !f.unsupported[p.GetId()], Effective: metav1.ProfileOrigin_PROFILE_ORIGIN_NONE}
		if pp := f.pushed[p.GetId()]; pp != nil {
			shown := proto.Clone(pp).(*metadatav1.NetworkProfile)
			if shown.GetProxy() != nil {
				a.PushedProxyPasswordSet = shown.GetProxy().GetPassword() != ""
				shown.Proxy.Password = ""
			}
			a.PushedProfile, a.PushedProfileName = shown, pp.GetName()
			a.PushedAt = timestamppb.New(f.pushedAt[p.GetId()])
			a.Effective = metav1.ProfileOrigin_PROFILE_ORIGIN_PUSHED
		}
		res.Assignments = append(res.Assignments, a)
	}
	return connect.NewResponse(res), nil
}

func (h *integrationsHandler) SetPushedProfile(_ context.Context, req *connect.Request[metav1.SetPushedProfileRequest]) (*connect.Response[metav1.SetPushedProfileResponse], error) {
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	id := req.Msg.GetProviderId()
	if !slices.ContainsFunc(f.providers, func(p *metav1.Provider) bool { return p.GetId() == id }) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such provider"))
	}
	if f.pushed == nil {
		f.pushed, f.pushedAt = map[string]*metadatav1.NetworkProfile{}, map[string]time.Time{}
	}
	if p := req.Msg.GetProfile(); p != nil {
		f.pushed[id], f.pushedAt[id] = p, time.Now()
	} else {
		delete(f.pushed, id)
		delete(f.pushedAt, id)
	}
	return connect.NewResponse(&metav1.SetPushedProfileResponse{}), nil
}

// AddJob adds one of meta's jobs (not a sync): RunJob starts a run that
// stays running until cancelled or finished with FinishRun.
func (f *Fake) AddJob(j *metav1.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, j)
}

// FinishRun ends a run as succeeded.
func (f *Fake) FinishRun(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.run(id); r != nil {
		r.Summary.Status = metav1.JobRunStatus_JOB_RUN_STATUS_SUCCEEDED
		r.Summary.FinishedAt = timestamppb.Now()
	}
}

// run finds a run; call with f.mu held.
func (f *Fake) run(id string) *metav1.JobRun {
	for _, r := range f.runs {
		if r.GetSummary().GetId() == id {
			return r
		}
	}
	return nil
}

// job is a job as listed now; call with f.mu held.
func (f *Fake) job(id string) *metav1.Job {
	for _, j := range f.jobs {
		if j.GetId() != id {
			continue
		}
		out := proto.Clone(j).(*metav1.Job)
		for i := len(f.runs) - 1; i >= 0; i-- {
			if r := f.runs[i].GetSummary(); r.GetJobId() == id {
				out.LastRun = r
				if r.GetStatus() == metav1.JobRunStatus_JOB_RUN_STATUS_RUNNING {
					out.Running, out.CurrentRunId = true, r.GetId()
				}
				break
			}
		}
		return out
	}
	return nil
}

func (f *Fake) runJob(id string, params map[string]string) (*metav1.JobRunSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.job(id)
	if j == nil {
		return nil, false, nil
	}
	if j.GetRunning() {
		cerr := connect.NewError(connect.CodeAlreadyExists, errors.New("the job is running"))
		if d, err := connect.NewErrorDetail(&metav1.JobAlreadyRunning{CurrentRunId: j.GetCurrentRunId()}); err == nil {
			cerr.AddDetail(d)
		}
		return nil, true, cerr
	}
	sum := &metav1.JobRunSummary{
		Id: uuid.NewString(), JobId: id, Status: metav1.JobRunStatus_JOB_RUN_STATUS_RUNNING,
		Trigger: metav1.JobTrigger_JOB_TRIGGER_MANUAL, StartedAt: timestamppb.Now(), TriggeredBy: "test client",
	}
	f.runs = append(f.runs, &metav1.JobRun{Summary: sum, Params: params, Progress: &metav1.JobProgress{Phase: "working", Done: 1, Total: 10}})
	return sum, true, nil
}

func (h *jobsHandler) ListJobs(ctx context.Context, _ *connect.Request[metav1.ListJobsRequest]) (*connect.Response[metav1.ListJobsResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &metav1.ListJobsResponse{}
	for _, j := range f.jobs {
		res.Jobs = append(res.Jobs, f.job(j.GetId()))
	}
	return connect.NewResponse(res), nil
}

func (h *jobsHandler) GetJob(ctx context.Context, req *connect.Request[metav1.GetJobRequest]) (*connect.Response[metav1.GetJobResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	j := h.f.job(req.Msg.GetJobId())
	if j == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such job"))
	}
	return connect.NewResponse(&metav1.GetJobResponse{Job: j}), nil
}

func (h *jobsHandler) ListJobRuns(ctx context.Context, req *connect.Request[metav1.ListJobRunsRequest]) (*connect.Response[metav1.ListJobRunsResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.job(req.Msg.GetJobId()) == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such job"))
	}
	res := &metav1.ListJobRunsResponse{}
	for i := len(f.runs) - 1; i >= 0; i-- {
		r := f.runs[i].GetSummary()
		if r.GetJobId() == req.Msg.GetJobId() && (req.Msg.GetStatus() == metav1.JobRunStatus_JOB_RUN_STATUS_UNSPECIFIED || r.GetStatus() == req.Msg.GetStatus()) {
			res.Runs = append(res.Runs, r)
		}
	}
	return connect.NewResponse(res), nil
}

func (h *jobsHandler) GetJobRun(ctx context.Context, req *connect.Request[metav1.GetJobRunRequest]) (*connect.Response[metav1.GetJobRunResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r := h.f.run(req.Msg.GetRunId())
	if r == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such run"))
	}
	return connect.NewResponse(&metav1.GetJobRunResponse{Run: proto.Clone(r).(*metav1.JobRun)}), nil
}

func (h *jobsHandler) CancelJobRun(ctx context.Context, req *connect.Request[metav1.CancelJobRunRequest]) (*connect.Response[metav1.CancelJobRunResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r := h.f.run(req.Msg.GetRunId())
	if r == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such run"))
	}
	r.Summary.Status, r.Summary.FinishedAt, r.Summary.Message = metav1.JobRunStatus_JOB_RUN_STATUS_CANCELLED, timestamppb.Now(), "cancelled"
	return connect.NewResponse(&metav1.CancelJobRunResponse{Run: r.GetSummary()}), nil
}

// AddPair adds a suspected duplicate pair of two entries (open).
func (f *Fake) AddPair(id int64, a, b string, score float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs = append(f.pairs, &metav1.DuplicatePair{
		Id: id, EntryA: &metav1.EntrySummary{Id: a, Title: a}, EntryB: &metav1.EntrySummary{Id: b, Title: b},
		Status: metav1.DuplicateStatus_DUPLICATE_STATUS_OPEN, Score: score, CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
		Signals: &metav1.DuplicateSignals{Title: 1, TitleA: a, TitleB: b, People: 0.5, SharedPeople: []string{"A. Author"}, YearA: 2020, YearB: 2020},
	})
}

// JudgeKey is the decision model's stored API key.
func (f *Fake) JudgeKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.judgeKey
}

type duplicatesHandler struct {
	metav1connect.UnimplementedMetaDuplicatesServiceHandler
	f *Fake
}

func (h *duplicatesHandler) pair(id int64) *metav1.DuplicatePair {
	for _, p := range h.f.pairs {
		if p.GetId() == id {
			return p
		}
	}
	return nil
}

func (h *duplicatesHandler) ListDuplicates(ctx context.Context, req *connect.Request[metav1.ListDuplicatesRequest]) (*connect.Response[metav1.ListDuplicatesResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if n := req.Msg.GetPageSize(); n > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size is at most 100"))
	}
	status := req.Msg.GetStatus()
	if status == metav1.DuplicateStatus_DUPLICATE_STATUS_UNSPECIFIED {
		status = metav1.DuplicateStatus_DUPLICATE_STATUS_OPEN
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	res := &metav1.ListDuplicatesResponse{}
	for _, p := range h.f.pairs {
		if p.GetStatus() == status && p.GetScore() >= req.Msg.GetMinScore() && (!req.Msg.GetJudgedOnly() || p.GetVerdict() != nil) {
			res.Pairs = append(res.Pairs, p)
		}
	}
	res.TotalSize = int64(len(res.Pairs))
	return connect.NewResponse(res), nil
}

func (h *duplicatesHandler) GetDuplicate(ctx context.Context, req *connect.Request[metav1.GetDuplicateRequest]) (*connect.Response[metav1.GetDuplicateResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	p := h.pair(req.Msg.GetId())
	if p == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such pair"))
	}
	return connect.NewResponse(&metav1.GetDuplicateResponse{Pair: p}), nil
}

func (h *duplicatesHandler) decide(ctx context.Context, id int64, from, to metav1.DuplicateStatus, note string) (*metav1.DuplicatePair, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	p := h.pair(id)
	if p == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such pair"))
	}
	if p.GetStatus() != from {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the pair is "+p.GetStatus().String()))
	}
	p.Status, p.Note, p.DecidedBy, p.DecidedAt = to, note, "test client", timestamppb.Now()
	return p, nil
}

func (h *duplicatesHandler) ConfirmDuplicate(ctx context.Context, req *connect.Request[metav1.ConfirmDuplicateRequest]) (*connect.Response[metav1.ConfirmDuplicateResponse], error) {
	p, err := h.decide(ctx, req.Msg.GetId(), metav1.DuplicateStatus_DUPLICATE_STATUS_OPEN, metav1.DuplicateStatus_DUPLICATE_STATUS_CONFIRMED, req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	keep := p.GetEntryA().GetId()
	if req.Msg.GetKeep() == "b" {
		keep = p.GetEntryB().GetId()
	}
	return connect.NewResponse(&metav1.ConfirmDuplicateResponse{Pair: p, EntryId: keep}), nil
}

func (h *duplicatesHandler) DismissDuplicate(ctx context.Context, req *connect.Request[metav1.DismissDuplicateRequest]) (*connect.Response[metav1.DismissDuplicateResponse], error) {
	p, err := h.decide(ctx, req.Msg.GetId(), metav1.DuplicateStatus_DUPLICATE_STATUS_OPEN, metav1.DuplicateStatus_DUPLICATE_STATUS_DISMISSED, req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&metav1.DismissDuplicateResponse{Pair: p}), nil
}

func (h *duplicatesHandler) ReopenDuplicate(ctx context.Context, req *connect.Request[metav1.ReopenDuplicateRequest]) (*connect.Response[metav1.ReopenDuplicateResponse], error) {
	p, err := h.decide(ctx, req.Msg.GetId(), metav1.DuplicateStatus_DUPLICATE_STATUS_DISMISSED, metav1.DuplicateStatus_DUPLICATE_STATUS_OPEN, "")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&metav1.ReopenDuplicateResponse{Pair: p}), nil
}

// judgeShown is the judge as meta returns it; call with f.mu held.
func (h *duplicatesHandler) judgeShown() *metav1.DecisionJudgeSettings {
	if h.f.judge == nil {
		return &metav1.DecisionJudgeSettings{Kind: "local", Origin: "config"}
	}
	out := proto.Clone(h.f.judge).(*metav1.DecisionJudgeSettings)
	out.ApiKey, out.HasApiKey, out.Origin = "", h.f.judgeKey != "", "admin"
	return out
}

func (h *duplicatesHandler) GetDecisionJudge(ctx context.Context, _ *connect.Request[metav1.GetDecisionJudgeRequest]) (*connect.Response[metav1.GetDecisionJudgeResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return connect.NewResponse(&metav1.GetDecisionJudgeResponse{Settings: h.judgeShown()}), nil
}

func (h *duplicatesHandler) PutDecisionJudge(ctx context.Context, req *connect.Request[metav1.PutDecisionJudgeRequest]) (*connect.Response[metav1.PutDecisionJudgeResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.judge = proto.Clone(req.Msg.GetSettings()).(*metav1.DecisionJudgeSettings)
	if k := req.Msg.GetSettings().GetApiKey(); k != "" {
		h.f.judgeKey = k
	}
	return connect.NewResponse(&metav1.PutDecisionJudgeResponse{Settings: h.judgeShown()}), nil
}

func (h *duplicatesHandler) TestDecisionJudge(ctx context.Context, req *connect.Request[metav1.TestDecisionJudgeRequest]) (*connect.Response[metav1.TestDecisionJudgeResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.Msg.GetSettings().GetKind() == "broken" {
		return connect.NewResponse(&metav1.TestDecisionJudgeResponse{Error: "the model didn't answer"}), nil
	}
	return connect.NewResponse(&metav1.TestDecisionJudgeResponse{Ok: true, Verdict: &metav1.DuplicateVerdict{
		Judge: req.Msg.GetSettings().GetKind(), Same: 0.9, Choice: "same_work", Probabilities: map[string]float64{"same_work": 0.9, "different": 0.1},
	}}), nil
}

func (h *duplicatesHandler) ResetDecisionJudge(ctx context.Context, _ *connect.Request[metav1.ResetDecisionJudgeRequest]) (*connect.Response[metav1.ResetDecisionJudgeResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.judge, h.f.judgeKey = nil, ""
	return connect.NewResponse(&metav1.ResetDecisionJudgeResponse{Settings: h.judgeShown()}), nil
}

// SearchKey is the search engine's stored API key.
func (f *Fake) SearchKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchKey
}

// searchShown is the search engine as meta returns it; call with f.mu held.
func (f *Fake) searchShown() (*metav1.SearchEngineSettings, *metav1.SearchEngineStatus) {
	st := &metav1.SearchEngineSettings{Backend: "postgres", Origin: "config"}
	if f.search != nil {
		st = proto.Clone(f.search).(*metav1.SearchEngineSettings)
		st.ApiKey, st.HasApiKey, st.Origin = "", f.searchKey != "", "admin"
	}
	status := &metav1.SearchEngineStatus{Backend: st.GetBackend(), Substring: true, Reachable: true, Ready: true, Documents: 42, Lag: durationpb.New(2 * time.Second)}
	return st, status
}

func (h *integrationsHandler) GetSearchEngine(ctx context.Context, _ *connect.Request[metav1.GetSearchEngineRequest]) (*connect.Response[metav1.GetSearchEngineResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	st, status := h.f.searchShown()
	return connect.NewResponse(&metav1.GetSearchEngineResponse{Settings: st, Status: status}), nil
}

func (h *integrationsHandler) PutSearchEngine(ctx context.Context, req *connect.Request[metav1.PutSearchEngineRequest]) (*connect.Response[metav1.PutSearchEngineResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if b := req.Msg.GetSettings().GetBackend(); b != "postgres" && req.Msg.GetSettings().GetUrl() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(b+" needs a url"))
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.search = proto.Clone(req.Msg.GetSettings()).(*metav1.SearchEngineSettings)
	if k := req.Msg.GetSettings().GetApiKey(); k != "" {
		h.f.searchKey = k
	}
	st, status := h.f.searchShown()
	return connect.NewResponse(&metav1.PutSearchEngineResponse{Settings: st, Status: status}), nil
}

func (h *integrationsHandler) TestSearchEngine(ctx context.Context, req *connect.Request[metav1.TestSearchEngineRequest]) (*connect.Response[metav1.TestSearchEngineResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if strings.Contains(req.Msg.GetSettings().GetUrl(), "nowhere") {
		return connect.NewResponse(&metav1.TestSearchEngineResponse{Error: "can't reach the engine"}), nil
	}
	return connect.NewResponse(&metav1.TestSearchEngineResponse{Ok: true}), nil
}

func (h *integrationsHandler) ResetSearchEngine(ctx context.Context, _ *connect.Request[metav1.ResetSearchEngineRequest]) (*connect.Response[metav1.ResetSearchEngineResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.search, h.f.searchKey = nil, ""
	st, status := h.f.searchShown()
	return connect.NewResponse(&metav1.ResetSearchEngineResponse{Settings: st, Status: status}), nil
}
