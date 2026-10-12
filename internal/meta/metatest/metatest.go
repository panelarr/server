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
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

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
			f.mu.Unlock()
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
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	found := slices.ContainsFunc(h.f.providers, func(p *metav1.Provider) bool { return p.Id == req.Msg.GetProvider() })
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such provider"))
	}
	if h.f.providerDown[req.Msg.GetProvider()] {
		cerr := connect.NewError(connect.CodeUnavailable, errors.New("plugin is not connected"))
		if h.f.explicit {
			if d, err := connect.NewErrorDetail(&errdetails.ErrorInfo{
				Reason: "PROVIDER_UNAVAILABLE", Domain: "panelarr.meta", Metadata: map[string]string{"provider": req.Msg.GetProvider()},
			}); err == nil {
				cerr.AddDetail(d)
			}
		}
		return nil, cerr
	}
	h.f.syncs = append(h.f.syncs, req.Msg)
	return connect.NewResponse(&metav1.RunSyncResponse{Run: &metav1.SyncRun{Id: "run-1", Provider: req.Msg.GetProvider(), Kind: req.Msg.GetKind()}}), nil
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
