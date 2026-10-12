// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/network"
	"github.com/panelarr/server/internal/plugins"
)

// networkError maps the network service's errors.
func networkError(err error) error {
	var bad *network.InvalidError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, network.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, network.ErrIDTaken):
		return NewProblem(http.StatusConflict, "id_taken", "this ID is taken; choose another")
	case errors.Is(err, network.ErrInvalidID):
		return fieldProblem("id", err.Error())
	case errors.Is(err, network.ErrInUse):
		return NewProblem(http.StatusConflict, "in_use", err.Error())
	case errors.Is(err, network.ErrNotShared):
		return NewProblem(http.StatusConflict, "profile_not_shared", err.Error())
	case errors.As(err, &bad):
		return fieldProblem(bad.Field, bad.Reason)
	}
	return err
}

func secondsPtr(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	return ptr(int(d / time.Second))
}

func durationOf(seconds *int) time.Duration {
	if seconds == nil {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}

// ownerFilter narrows a network list to one owner's custom items.
func ownerFilter(kind *apigen.ProfileOwnerKind, id *string) *network.Owner {
	if kind == nil {
		return nil
	}
	return &network.Owner{Kind: string(*kind), ID: deref(id)}
}

func toOwner(o *network.Owner) *apigen.ProfileOwner {
	if o == nil {
		return nil
	}
	return &apigen.ProfileOwner{Kind: apigen.ProfileOwnerKind(o.Kind), Id: o.ID}
}

func ownerInput(o *apigen.ProfileOwner) *network.Owner {
	if o == nil {
		return nil
	}
	return &network.Owner{Kind: string(o.Kind), ID: o.Id}
}

func toSolver(s network.Solver) apigen.Solver {
	return apigen.Solver{
		Id: s.ID, Name: s.Name, Type: apigen.SolverType(s.Type), Url: s.URL,
		SessionTtlSeconds: secondsPtr(s.SessionTTL), MaxTimeoutSeconds: secondsPtr(s.MaxTimeout),
		AsResponseFallback: s.AsResponseFallback, Owner: toOwner(s.Owner), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

func solverInput(b apigen.NewSolver) network.Solver {
	return network.Solver{
		ID: b.Id, Name: b.Name, Type: string(b.Type), URL: b.Url,
		SessionTTL: durationOf(b.SessionTtlSeconds), MaxTimeout: durationOf(b.MaxTimeoutSeconds), AsResponseFallback: deref(b.AsResponseFallback),
		Owner: ownerInput(b.Owner),
	}
}

func toNetworkTest(r network.TestResult) apigen.NetworkTest {
	return apigen.NetworkTest{Ok: r.OK, Message: r.Message, Version: nonEmpty(r.Version), LatencyMs: int(r.Latency.Milliseconds())}
}

// ListSolvers lists the solvers.
func (s *Server) ListSolvers(ctx context.Context, req apigen.ListSolversRequestObject) (apigen.ListSolversResponseObject, error) {
	list, err := s.deps.Network.Solvers(ctx, ownerFilter(req.Params.OwnerKind, req.Params.OwnerId), deref(req.Params.IncludeCustom))
	if err != nil {
		return nil, err
	}
	out := apigen.ListSolvers200JSONResponse{Solvers: make([]apigen.Solver, len(list))}
	for i, v := range list {
		out.Solvers[i] = toSolver(v)
	}
	return out, nil
}

// CreateSolver adds a solver.
func (s *Server) CreateSolver(ctx context.Context, req apigen.CreateSolverRequestObject) (apigen.CreateSolverResponseObject, error) {
	v, err := s.deps.Network.CreateSolver(ctx, solverInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.CreateSolver201JSONResponse(toSolver(v)), nil
}

// TestSolverSettings checks an unsaved solver.
func (s *Server) TestSolverSettings(ctx context.Context, req apigen.TestSolverSettingsRequestObject) (apigen.TestSolverSettingsResponseObject, error) {
	r, err := s.deps.Network.TestSolver(ctx, solverInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestSolverSettings200JSONResponse(toNetworkTest(r)), nil
}

// GetSolver returns a solver.
func (s *Server) GetSolver(ctx context.Context, req apigen.GetSolverRequestObject) (apigen.GetSolverResponseObject, error) {
	v, err := s.deps.Network.Solver(ctx, req.SolverId)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.GetSolver200JSONResponse(toSolver(v)), nil
}

// UpdateSolver changes a solver.
func (s *Server) UpdateSolver(ctx context.Context, req apigen.UpdateSolverRequestObject) (apigen.UpdateSolverResponseObject, error) {
	b := req.Body
	p := network.SolverPatch{Name: b.Name, URL: b.Url, AsResponseFallback: b.AsResponseFallback}
	if b.Type != nil {
		p.Type = ptr(string(*b.Type))
	}
	if b.SessionTtlSeconds != nil {
		p.SessionTTL = ptr(durationOf(b.SessionTtlSeconds))
	}
	if b.MaxTimeoutSeconds != nil {
		p.MaxTimeout = ptr(durationOf(b.MaxTimeoutSeconds))
	}
	v, err := s.deps.Network.UpdateSolver(ctx, req.SolverId, p)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.UpdateSolver200JSONResponse(toSolver(v)), nil
}

// DeleteSolver removes a solver.
func (s *Server) DeleteSolver(ctx context.Context, req apigen.DeleteSolverRequestObject) (apigen.DeleteSolverResponseObject, error) {
	if err := s.deps.Network.DeleteSolver(ctx, req.SolverId); err != nil {
		return nil, networkError(err)
	}
	return apigen.DeleteSolver204Response{}, nil
}

// TestSolver checks a saved solver.
func (s *Server) TestSolver(ctx context.Context, req apigen.TestSolverRequestObject) (apigen.TestSolverResponseObject, error) {
	v, err := s.deps.Network.Solver(ctx, req.SolverId)
	if err != nil {
		return nil, networkError(err)
	}
	r, err := s.deps.Network.TestSolver(ctx, v)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestSolver200JSONResponse(toNetworkTest(r)), nil
}

func toProxy(p network.Proxy) apigen.Proxy {
	return apigen.Proxy{
		Id: p.ID, Name: p.Name, Url: p.URL, Username: nonEmpty(p.Username), HasPassword: p.HasPassword,
		Owner: toOwner(p.Owner), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func proxyInput(b apigen.NewProxy) network.ProxyInput {
	return network.ProxyInput{ID: b.Id, Name: b.Name, URL: b.Url, Username: deref(b.Username), Password: deref(b.Password), Owner: ownerInput(b.Owner)}
}

// ListProxies lists the proxies.
func (s *Server) ListProxies(ctx context.Context, req apigen.ListProxiesRequestObject) (apigen.ListProxiesResponseObject, error) {
	list, err := s.deps.Network.Proxies(ctx, ownerFilter(req.Params.OwnerKind, req.Params.OwnerId), deref(req.Params.IncludeCustom))
	if err != nil {
		return nil, err
	}
	out := apigen.ListProxies200JSONResponse{Proxies: make([]apigen.Proxy, len(list))}
	for i, v := range list {
		out.Proxies[i] = toProxy(v)
	}
	return out, nil
}

// CreateProxy adds a proxy.
func (s *Server) CreateProxy(ctx context.Context, req apigen.CreateProxyRequestObject) (apigen.CreateProxyResponseObject, error) {
	v, err := s.deps.Network.CreateProxy(ctx, proxyInput(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.CreateProxy201JSONResponse(toProxy(v)), nil
}

// TestProxySettings checks an unsaved proxy (or a saved one with changes).
func (s *Server) TestProxySettings(ctx context.Context, req apigen.TestProxySettingsRequestObject) (apigen.TestProxySettingsResponseObject, error) {
	r, err := s.deps.Network.TestProxy(ctx, proxyInput(req.Body.Proxy), deref(req.Body.Url))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestProxySettings200JSONResponse(toNetworkTest(r)), nil
}

// GetProxy returns a proxy.
func (s *Server) GetProxy(ctx context.Context, req apigen.GetProxyRequestObject) (apigen.GetProxyResponseObject, error) {
	v, err := s.deps.Network.Proxy(ctx, req.ProxyId)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.GetProxy200JSONResponse(toProxy(v)), nil
}

// UpdateProxy changes a proxy.
func (s *Server) UpdateProxy(ctx context.Context, req apigen.UpdateProxyRequestObject) (apigen.UpdateProxyResponseObject, error) {
	b := req.Body
	v, err := s.deps.Network.UpdateProxy(ctx, req.ProxyId, network.ProxyPatch{Name: b.Name, URL: b.Url, Username: b.Username, Password: b.Password})
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.UpdateProxy200JSONResponse(toProxy(v)), nil
}

// DeleteProxy removes a proxy.
func (s *Server) DeleteProxy(ctx context.Context, req apigen.DeleteProxyRequestObject) (apigen.DeleteProxyResponseObject, error) {
	if err := s.deps.Network.DeleteProxy(ctx, req.ProxyId); err != nil {
		return nil, networkError(err)
	}
	return apigen.DeleteProxy204Response{}, nil
}

// TestProxy checks a saved proxy.
func (s *Server) TestProxy(ctx context.Context, req apigen.TestProxyRequestObject) (apigen.TestProxyResponseObject, error) {
	p, err := s.deps.Network.Proxy(ctx, req.ProxyId)
	if err != nil {
		return nil, networkError(err)
	}
	target := ""
	if req.Body != nil {
		target = deref(req.Body.Url)
	}
	r, err := s.deps.Network.TestProxy(ctx, network.ProxyInput{ID: p.ID, Name: p.Name, URL: p.URL, Username: p.Username}, target)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.TestProxy200JSONResponse(toNetworkTest(r)), nil
}

func secs(f *float32) time.Duration {
	if f == nil {
		return 0
	}
	return time.Duration(float64(*f) * float64(time.Second))
}

func secsPtr(d time.Duration) *float32 {
	if d <= 0 {
		return nil
	}
	return ptr(float32(d.Seconds()))
}

// profileSettings turns the API's profile settings into the service's.
func profileSettings(b apigen.NetworkProfileSettings) network.Profile {
	p := network.Profile{
		Name: b.Name, ProxyID: deref(b.ProxyId), SolverID: deref(b.SolverId), Concurrency: deref(b.Concurrency),
		DoHURL: deref(b.DohUrl), SharedCookieJar: deref(b.SharedCookieJar), UserAgentMode: network.UAPluginDefault,
	}
	if b.Headers != nil {
		p.Headers = *b.Headers
	}
	if rl := b.RateLimit; rl != nil {
		p.RateLimit = &network.RateLimit{
			Requests: rl.Requests, Period: secs(&rl.PeriodSeconds), Burst: deref(rl.Burst), Scope: "source",
		}
		if rl.Scope != nil {
			p.RateLimit.Scope = string(*rl.Scope)
		}
	}
	if ua := b.UserAgent; ua != nil {
		p.UserAgentMode, p.UserAgent = string(ua.Mode), deref(ua.Value)
	}
	if t := b.Timeouts; t != nil {
		p.Timeouts = network.Timeouts{Connect: secs(t.ConnectSeconds), Read: secs(t.ReadSeconds), Call: secs(t.CallSeconds)}
	}
	return p
}

func toNetworkProfile(p network.Profile, assigned []apigen.NetworkTarget) apigen.NetworkProfile {
	out := apigen.NetworkProfile{
		Id: p.ID, Name: p.Name, ProxyId: nonEmpty(p.ProxyID), SolverId: nonEmpty(p.SolverID),
		DohUrl: nonEmpty(p.DoHURL), SharedCookieJar: nonEmpty(p.SharedCookieJar), Headers: p.Headers,
		UserAgent: apigen.NetworkUserAgent{Mode: apigen.NetworkUserAgentMode(p.UserAgentMode), Value: nonEmpty(p.UserAgent)},
		Timeouts: apigen.NetworkTimeouts{
			ConnectSeconds: secsPtr(p.Timeouts.Connect), ReadSeconds: secsPtr(p.Timeouts.Read), CallSeconds: secsPtr(p.Timeouts.Call),
		},
		AssignedTo: assigned, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if out.AssignedTo == nil {
		out.AssignedTo = []apigen.NetworkTarget{}
	}
	if p.Concurrency > 0 {
		out.Concurrency = ptr(p.Concurrency)
	}
	if rl := p.RateLimit; rl != nil {
		scope := apigen.NetworkRateLimitScope(rl.Scope)
		out.RateLimit = &apigen.NetworkRateLimit{Requests: rl.Requests, PeriodSeconds: float32(rl.Period.Seconds()), Scope: &scope}
		if rl.Burst > 0 {
			out.RateLimit.Burst = ptr(rl.Burst)
		}
	}
	if p.Owner != nil {
		out.Owner = &apigen.ProfileOwner{Kind: apigen.ProfileOwnerKind(p.Owner.Kind), Id: p.Owner.ID}
	}
	return out
}

// assignedTo maps profiles to where they are assigned.
func (s *Server) assignedTo(ctx context.Context) (map[string][]apigen.NetworkTarget, error) {
	list, err := s.deps.Network.Assignments(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]apigen.NetworkTarget{}
	for _, a := range list {
		if a.ProfileID != "" {
			out[a.ProfileID] = append(out[a.ProfileID], apigen.NetworkTarget{TargetKind: apigen.NetworkTargetKind(a.Kind), TargetId: a.TargetID})
		}
	}
	return out, nil
}

func (s *Server) profileResponse(ctx context.Context, p network.Profile) (apigen.NetworkProfile, error) {
	assigned, err := s.assignedTo(ctx)
	if err != nil {
		return apigen.NetworkProfile{}, err
	}
	return toNetworkProfile(p, assigned[p.ID]), nil
}

// ListNetworkProfiles lists profiles.
func (s *Server) ListNetworkProfiles(ctx context.Context, req apigen.ListNetworkProfilesRequestObject) (apigen.ListNetworkProfilesResponseObject, error) {
	list, err := s.deps.Network.Profiles(ctx, ownerFilter(req.Params.OwnerKind, req.Params.OwnerId), deref(req.Params.IncludeCustom))
	if err != nil {
		return nil, err
	}
	assigned, err := s.assignedTo(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListNetworkProfiles200JSONResponse{Profiles: make([]apigen.NetworkProfile, len(list))}
	for i, p := range list {
		out.Profiles[i] = toNetworkProfile(p, assigned[p.ID])
	}
	return out, nil
}

// CreateNetworkProfile adds a profile.
func (s *Server) CreateNetworkProfile(ctx context.Context, req apigen.CreateNetworkProfileRequestObject) (apigen.CreateNetworkProfileResponseObject, error) {
	b := req.Body
	p := profileSettings(apigen.NetworkProfileSettings{
		Name: b.Name, ProxyId: b.ProxyId, SolverId: b.SolverId, RateLimit: b.RateLimit, Concurrency: b.Concurrency,
		UserAgent: b.UserAgent, DohUrl: b.DohUrl, Headers: b.Headers, SharedCookieJar: b.SharedCookieJar, Timeouts: b.Timeouts,
	})
	p.ID = b.Id
	if b.Owner != nil {
		p.Owner = &network.Owner{Kind: string(b.Owner.Kind), ID: b.Owner.Id}
	}
	created, err := s.deps.Network.CreateProfile(ctx, p)
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.CreateNetworkProfile201JSONResponse(toNetworkProfile(created, nil)), nil
}

// GetNetworkProfile returns a profile.
func (s *Server) GetNetworkProfile(ctx context.Context, req apigen.GetNetworkProfileRequestObject) (apigen.GetNetworkProfileResponseObject, error) {
	p, err := s.deps.Network.Profile(ctx, req.ProfileId)
	if err != nil {
		return nil, networkError(err)
	}
	out, err := s.profileResponse(ctx, p)
	if err != nil {
		return nil, err
	}
	return apigen.GetNetworkProfile200JSONResponse(out), nil
}

// ReplaceNetworkProfile replaces a profile's settings.
func (s *Server) ReplaceNetworkProfile(ctx context.Context, req apigen.ReplaceNetworkProfileRequestObject) (apigen.ReplaceNetworkProfileResponseObject, error) {
	p, err := s.deps.Network.ReplaceProfile(ctx, req.ProfileId, profileSettings(*req.Body))
	if err != nil {
		return nil, networkError(err)
	}
	out, err := s.profileResponse(ctx, p)
	if err != nil {
		return nil, err
	}
	return apigen.ReplaceNetworkProfile200JSONResponse(out), nil
}

// DeleteNetworkProfile removes a profile.
func (s *Server) DeleteNetworkProfile(ctx context.Context, req apigen.DeleteNetworkProfileRequestObject) (apigen.DeleteNetworkProfileResponseObject, error) {
	if err := s.deps.Network.DeleteProfile(ctx, req.ProfileId); err != nil {
		return nil, networkError(err)
	}
	return apigen.DeleteNetworkProfile204Response{}, nil
}

func toAssignment(a network.Assignment) apigen.NetworkAssignment {
	return apigen.NetworkAssignment{
		TargetKind: apigen.NetworkTargetKind(a.Kind), TargetId: a.TargetID, ProfileId: nonEmpty(a.ProfileID),
		PluginDefault: a.PluginDefault(), UpdatedAt: a.UpdatedAt,
	}
}

// ListNetworkAssignments lists the assignments.
func (s *Server) ListNetworkAssignments(ctx context.Context, _ apigen.ListNetworkAssignmentsRequestObject) (apigen.ListNetworkAssignmentsResponseObject, error) {
	list, err := s.deps.Network.Assignments(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListNetworkAssignments200JSONResponse{Assignments: make([]apigen.NetworkAssignment, len(list))}
	for i, a := range list {
		out.Assignments[i] = toAssignment(a)
	}
	return out, nil
}

// AssignNetworkProfile assigns a profile at a level.
func (s *Server) AssignNetworkProfile(ctx context.Context, req apigen.AssignNetworkProfileRequestObject) (apigen.AssignNetworkProfileResponseObject, error) {
	b := req.Body
	if (b.ProfileId != nil) == deref(b.PluginDefault) {
		return nil, fieldProblem("profile_id", "give profile_id, or plugin_default: true")
	}
	a, err := s.deps.Network.Assign(ctx, string(b.TargetKind), deref(b.TargetId), deref(b.ProfileId))
	if err != nil {
		return nil, networkError(err)
	}
	return apigen.AssignNetworkProfile200JSONResponse(toAssignment(a)), nil
}

// UnassignNetworkProfile removes an assignment.
func (s *Server) UnassignNetworkProfile(ctx context.Context, req apigen.UnassignNetworkProfileRequestObject) (apigen.UnassignNetworkProfileResponseObject, error) {
	if err := s.deps.Network.Unassign(ctx, string(req.Params.TargetKind), deref(req.Params.TargetId)); err != nil {
		return nil, networkError(err)
	}
	return apigen.UnassignNetworkProfile204Response{}, nil
}

func toEffective(e network.Effective) apigen.NetworkEffective {
	return apigen.NetworkEffective{Level: apigen.NetworkEffectiveLevel(e.Level), ProfileId: nonEmpty(e.ProfileID), PluginDefault: e.ProfileID == ""}
}

// networkView computes sources' network rows: the profile in force and
// whether the plugin has it.
type networkView struct {
	s      *Server
	res    *network.Resolver
	pushes map[string]network.Push
	hashes map[string]string
}

func (s *Server) newNetworkView(ctx context.Context) (*networkView, error) {
	res, err := s.deps.Network.Resolver(ctx)
	if err != nil {
		return nil, err
	}
	pushes, err := s.deps.Network.Pushes(ctx, network.KindSource)
	if err != nil {
		return nil, err
	}
	return &networkView{s: s, res: res, pushes: pushes, hashes: map[string]string{}}, nil
}

// hash is the hash of a profile as the plugin would get it.
func (v *networkView) hash(ctx context.Context, profileID string, solver bool) (string, error) {
	if profileID == "" {
		return network.UnsetHash, nil
	}
	key := fmt.Sprint(profileID, solver)
	if h, ok := v.hashes[key]; ok {
		return h, nil
	}
	prof, err := v.s.deps.Network.SourceProfile(ctx, profileID, solver)
	if err != nil {
		return "", err
	}
	v.hashes[key] = network.Hash(prof)
	return v.hashes[key], nil
}

func (v *networkView) source(ctx context.Context, pl plugins.Plugin, src plugins.Source) (apigen.SourceNetwork, error) {
	eff := v.res.Source(src.ID, pl.ID, src.PackageID)
	out := apigen.SourceNetwork{
		SourceId: src.ID, Name: src.Name, Lang: nonEmpty(src.Lang), PackageId: nonEmpty(src.PackageID), Present: ptr(src.Present),
		Effective: toEffective(eff),
	}
	if a, ok := v.res.Assigned(network.KindSource, src.ID); ok {
		out.Assignment = ptr(toAssignment(a))
	}
	push := apigen.NetworkPush{State: apigen.NetworkPushStateNotAccepted}
	if plugins.AcceptsProfiles(pl.Info) {
		want, err := v.hash(ctx, eff.ProfileID, plugins.AcceptsSolver(pl.Info))
		if err != nil {
			return apigen.SourceNetwork{}, err
		}
		last, seen := v.pushes[src.ID]
		switch {
		case seen && last.Error != "":
			push = apigen.NetworkPush{State: apigen.NetworkPushStateFailed, Error: ptr(last.Error), PushedAt: &last.PushedAt}
		case seen && last.Hash == want:
			push = apigen.NetworkPush{State: apigen.NetworkPushStatePushed, PushedAt: &last.PushedAt}
		case !seen && want == network.UnsetHash:
			push = apigen.NetworkPush{State: apigen.NetworkPushStateNotNeeded}
		default:
			push = apigen.NetworkPush{State: apigen.NetworkPushStatePending}
		}
	}
	out.Push = &push
	return out, nil
}

// GetPluginNetwork shows the profile in force for each of a plugin's
// sources.
func (s *Server) GetPluginNetwork(ctx context.Context, req apigen.GetPluginNetworkRequestObject) (apigen.GetPluginNetworkResponseObject, error) {
	pl, err := s.deps.Plugins.Get(ctx, req.PluginId)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	view, err := s.newNetworkView(ctx)
	if err != nil {
		return nil, err
	}
	srcs, err := s.deps.Plugins.Sources(ctx, s.sourceViewer(ctx), true)
	if err != nil {
		return nil, err
	}
	out := apigen.GetPluginNetwork200JSONResponse{
		PluginId: pl.ID, AcceptsProfiles: plugins.AcceptsProfiles(pl.Info), AcceptsSolver: plugins.AcceptsSolver(pl.Info),
		Packages: []apigen.NetworkAssignment{}, Sources: []apigen.SourceNetwork{},
	}
	if a, ok := view.res.Assigned(network.KindPlugin, pl.ID); ok {
		out.Assignment = ptr(toAssignment(a))
	}
	all, err := s.deps.Network.Assignments(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.Kind == network.KindPackage && strings.HasPrefix(a.TargetID, pl.ID+":") {
			out.Packages = append(out.Packages, toAssignment(a))
		}
	}
	for _, src := range srcs {
		if src.PluginID != pl.ID {
			continue
		}
		sn, err := view.source(ctx, pl, src)
		if err != nil {
			return nil, err
		}
		out.Sources = append(out.Sources, sn)
	}
	return out, nil
}

// GetSourceNetwork shows one source's profile and what its plugin applies.
func (s *Server) GetSourceNetwork(ctx context.Context, req apigen.GetSourceNetworkRequestObject) (apigen.GetSourceNetworkResponseObject, error) {
	src, err := s.deps.Plugins.Source(ctx, req.SourceId, plugins.Viewer{Admin: true, AnyKnown: true})
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	pl, err := s.deps.Plugins.Get(ctx, src.PluginID)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	view, err := s.newNetworkView(ctx)
	if err != nil {
		return nil, err
	}
	out, err := view.source(ctx, pl, src)
	if err != nil {
		return nil, err
	}
	origin, name, err := s.deps.Plugins.Applied(ctx, src.ID)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	out.Applied = &apigen.NetworkApplied{Origin: apigen.NetworkAppliedOrigin(origin), ProfileName: nonEmpty(name)}
	return apigen.GetSourceNetwork200JSONResponse(out), nil
}

// GetMetaNetwork shows each metadata provider's profile in force and what
// meta applies.
func (s *Server) GetMetaNetwork(ctx context.Context, _ apigen.GetMetaNetworkRequestObject) (apigen.GetMetaNetworkResponseObject, error) {
	list, err := s.deps.Meta.Assignments(ctx)
	if err != nil {
		return nil, metaError(err)
	}
	res, err := s.deps.Network.Resolver(ctx)
	if err != nil {
		return nil, err
	}
	pushes, err := s.deps.Network.Pushes(ctx, network.KindProvider)
	if err != nil {
		return nil, err
	}
	out := apigen.GetMetaNetwork200JSONResponse{Providers: []apigen.ProviderNetwork{}}
	if a, ok := res.Assigned(network.KindMeta, ""); ok {
		out.Assignment = ptr(toAssignment(a))
	}
	hashes := map[string]string{}
	for _, a := range list {
		id := a.GetProviderId()
		eff := res.Provider(id)
		pn := apigen.ProviderNetwork{ProviderId: id, AcceptsProfiles: a.GetSupported(), Effective: toEffective(eff)}
		if as, ok := res.Assigned(network.KindProvider, id); ok {
			pn.Assignment = ptr(toAssignment(as))
		}
		want := network.UnsetHash
		if eff.ProfileID != "" {
			if want = hashes[eff.ProfileID]; want == "" {
				prof, err := s.deps.Network.MetaProfile(ctx, eff.ProfileID)
				if err != nil {
					return nil, err
				}
				want = network.Hash(prof)
				hashes[eff.ProfileID] = want
			}
		}
		has := a.GetPushedProfile() != nil || a.GetPushedProfileName() != ""
		last, seen := pushes[id]
		switch {
		case seen && last.Error != "":
			pn.Push = apigen.NetworkPush{State: apigen.NetworkPushStateFailed, Error: ptr(last.Error), PushedAt: &last.PushedAt}
		case want == network.UnsetHash && !has:
			pn.Push = apigen.NetworkPush{State: apigen.NetworkPushStateNotNeeded}
		case seen && last.Hash == want && (want != network.UnsetHash) == has:
			pn.Push = apigen.NetworkPush{State: apigen.NetworkPushStatePushed, PushedAt: &last.PushedAt}
		default:
			pn.Push = apigen.NetworkPush{State: apigen.NetworkPushStatePending}
		}
		switch a.GetEffective() {
		case metav1.ProfileOrigin_PROFILE_ORIGIN_PUSHED:
			pn.Applied.Origin, pn.Applied.ProfileName = apigen.ProviderNetworkAppliedOriginPushed, nonEmpty(a.GetPushedProfileName())
		case metav1.ProfileOrigin_PROFILE_ORIGIN_ASSIGNED:
			pn.Applied.Origin, pn.Applied.ProfileName = apigen.ProviderNetworkAppliedOriginMeta, nonEmpty(a.GetEffectiveProfileId())
		case metav1.ProfileOrigin_PROFILE_ORIGIN_DEFAULT:
			pn.Applied.Origin, pn.Applied.ProfileName = apigen.ProviderNetworkAppliedOriginMetaDefault, nonEmpty(a.GetEffectiveProfileId())
		default:
			pn.Applied.Origin = apigen.ProviderNetworkAppliedOriginNone
		}
		if a.GetPushedAt() != nil {
			at := a.GetPushedAt().AsTime()
			pn.Applied.PushedAt = &at
		}
		out.Providers = append(out.Providers, pn)
	}
	return out, nil
}
