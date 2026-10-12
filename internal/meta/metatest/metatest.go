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
	return connect.NewResponse(&metav1.ListProvidersResponse{Providers: h.f.providers}), nil
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
	return &metav1.SyncRun{Id: "run-" + strconv.Itoa(len(f.syncs)), Provider: provider, Kind: kind}, nil
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
