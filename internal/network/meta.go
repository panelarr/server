// SPDX-License-Identifier: GPL-3.0-or-later

package network

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/durationpb"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/meta"
)

// Metadata providers get their profile through meta: the server pushes
// each provider's profile in force (provider, then all of meta, then
// global) with SetPushedProfile. meta keeps it, hands it only to plugins
// that take network profiles, and re-sends it when a plugin reconnects, so
// the server only pushes changes, and reconciles against what meta says
// it has (meta's database may have been reset).

// MetaPusher is the part of the meta service pushes use.
type MetaPusher interface {
	Assignments(ctx context.Context) ([]*metav1.Assignment, error)
	PushProfile(ctx context.Context, provider string, p *metadatav1.NetworkProfile) error
}

// MetaProfile builds a profile as metadata plugins receive it, solver
// inline (meta has no separate solver service) and the proxy's password.
func (s *Service) MetaProfile(ctx context.Context, id string) (*metadatav1.NetworkProfile, error) {
	p, err := s.Profile(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &metadatav1.NetworkProfile{
		Name: p.Name, Concurrency: uint32(p.Concurrency), Headers: p.Headers, DohUrl: p.DoHURL, //nolint:gosec // checked on save
	}
	if p.ProxyID != "" {
		px, err := s.proxyInput(ctx, p.ProxyID)
		if err != nil {
			return nil, err
		}
		out.Proxy = &metadatav1.Proxy{Url: px.URL, Username: px.Username, Password: px.Password}
	}
	if p.SolverID != "" {
		sv, err := s.Solver(ctx, p.SolverID)
		if err != nil {
			return nil, err
		}
		out.Solver = &metadatav1.Solver{Type: metadatav1.SolverType_SOLVER_TYPE_FLARESOLVERR, Url: sv.URL, AsResponseFallback: sv.AsResponseFallback}
		if sv.Type == SolverByparr {
			out.Solver.Type = metadatav1.SolverType_SOLVER_TYPE_BYPARR
		}
		if sv.SessionTTL > 0 {
			out.Solver.SessionTtl = durationpb.New(sv.SessionTTL)
		}
		if sv.MaxTimeout > 0 {
			out.Solver.MaxTimeout = durationpb.New(sv.MaxTimeout)
		}
	}
	if rl := p.RateLimit; rl != nil {
		out.RateLimit = &metadatav1.NetworkRateLimit{
			Requests: uint32(rl.Requests), Period: durationpb.New(rl.Period), Burst: uint32(rl.Burst), PerHost: rl.Scope == "host", //nolint:gosec // checked on save
		}
	}
	switch p.UserAgentMode {
	case UAFixed:
		out.UserAgent = &metadatav1.UserAgent{Mode: &metadatav1.UserAgent_Fixed{Fixed: p.UserAgent}}
	case UARandomDesktop:
		out.UserAgent = &metadatav1.UserAgent{Mode: &metadatav1.UserAgent_RandomDesktop{RandomDesktop: true}}
	case UAFromSolver:
		out.UserAgent = &metadatav1.UserAgent{Mode: &metadatav1.UserAgent_FromSolver{FromSolver: true}}
	}
	if t := p.Timeouts; t.Connect > 0 || t.Read > 0 || t.Call > 0 {
		out.Timeouts = &metadatav1.Timeouts{}
		if t.Connect > 0 {
			out.Timeouts.Connect = durationpb.New(t.Connect)
		}
		if t.Read > 0 {
			out.Timeouts.Read = durationpb.New(t.Read)
		}
		if t.Call > 0 {
			out.Timeouts.Call = durationpb.New(t.Call)
		}
	}
	return out, nil
}

// PushMeta pushes each metadata provider's profile in force when it
// changed, failed before, or meta doesn't have what it should (all of them
// with force). Without a metadata service it does nothing.
func (s *Service) PushMeta(ctx context.Context, m MetaPusher, force bool) (pushed, failed int, err error) {
	list, err := m.Assignments(ctx)
	if errors.Is(err, meta.ErrNotConfigured) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	res, err := s.Resolver(ctx)
	if err != nil {
		return 0, 0, err
	}
	prev, err := s.Pushes(ctx, KindProvider)
	if err != nil {
		return 0, 0, err
	}
	built := map[string]*metadatav1.NetworkProfile{}
	for _, a := range list {
		id := a.GetProviderId()
		eff := res.Provider(id)
		var prof *metadatav1.NetworkProfile
		if eff.ProfileID != "" {
			if prof = built[eff.ProfileID]; prof == nil {
				if prof, err = s.MetaProfile(ctx, eff.ProfileID); err != nil {
					return pushed, failed, err
				}
				built[eff.ProfileID] = prof
			}
		}
		hash := Hash(prof)
		has := a.GetPushedProfile() != nil || a.GetPushedProfileName() != ""
		last, seen := prev[id]
		switch {
		case prof == nil && !has:
			continue // nothing pushed, nothing to push
		case !force && seen && last.Hash == hash && (prof != nil) == has:
			continue
		}
		perr := m.PushProfile(ctx, id, prof)
		if perr != nil {
			failed++
			s.log.WarnContext(ctx, "network profile push to meta failed", "provider", id, "err", perr)
		} else {
			pushed++
		}
		if err := s.RecordPush(ctx, KindProvider, id, "", hash, perr); err != nil {
			return pushed, failed, err
		}
	}
	if pushed+failed > 0 {
		s.log.InfoContext(ctx, "network profiles pushed to meta", "pushed", pushed, "failed", failed)
	}
	return pushed, failed, nil
}

// Touch runs what follows a change (a push), e.g. after meta was connected.
func (s *Service) Touch(ctx context.Context) { s.changed(ctx) }
