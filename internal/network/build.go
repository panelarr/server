// SPDX-License-Identifier: GPL-3.0-or-later

package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/store/db"
)

// UnsetHash is the hash of "no profile": the plugin's own settings.
const UnsetHash = "unset"

// SourceProfile builds a profile as source plugins receive it, with the
// proxy's password and the solver inline. withSolver leaves the solver out
// for plugins that don't take one.
func (s *Service) SourceProfile(ctx context.Context, id string, withSolver bool) (*sourcev1.NetworkProfile, error) {
	p, err := s.Profile(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &sourcev1.NetworkProfile{
		Name: p.Name, Concurrency: uint32(p.Concurrency), DohUrl: p.DoHURL, //nolint:gosec // checked on save
		Headers: p.Headers, SharedCookieJar: p.SharedCookieJar,
	}
	if p.ProxyID != "" {
		px, err := s.proxyInput(ctx, p.ProxyID)
		if err != nil {
			return nil, err
		}
		out.Proxy = &sourcev1.Proxy{Url: px.URL, Username: px.Username, Password: px.Password}
	}
	if p.SolverID != "" && withSolver {
		sv, err := s.Solver(ctx, p.SolverID)
		if err != nil {
			return nil, err
		}
		out.Solver = &sourcev1.Solver{Type: solverType(sv.Type), Url: sv.URL, AsResponseFallback: sv.AsResponseFallback}
		if sv.SessionTTL > 0 {
			out.Solver.SessionTtl = durationpb.New(sv.SessionTTL)
		}
		if sv.MaxTimeout > 0 {
			out.Solver.MaxTimeout = durationpb.New(sv.MaxTimeout)
		}
	}
	if rl := p.RateLimit; rl != nil {
		scope := sourcev1.RateLimitScope_RATE_LIMIT_SCOPE_SOURCE
		if rl.Scope == "host" {
			scope = sourcev1.RateLimitScope_RATE_LIMIT_SCOPE_HOST
		}
		out.RateLimit = &sourcev1.RateLimit{Requests: uint32(rl.Requests), Period: durationpb.New(rl.Period), Burst: uint32(rl.Burst), Scope: scope} //nolint:gosec // checked on save
	}
	switch p.UserAgentMode {
	case UAFixed:
		out.UserAgent = &sourcev1.UserAgent{Mode: &sourcev1.UserAgent_Fixed{Fixed: p.UserAgent}}
	case UARandomDesktop:
		out.UserAgent = &sourcev1.UserAgent{Mode: &sourcev1.UserAgent_RandomDesktop{RandomDesktop: true}}
	case UAFromSolver:
		if out.Solver != nil {
			out.UserAgent = &sourcev1.UserAgent{Mode: &sourcev1.UserAgent_FromSolver{FromSolver: true}}
		}
	}
	if t := p.Timeouts; t.Connect > 0 || t.Read > 0 || t.Call > 0 {
		out.Timeouts = &sourcev1.Timeouts{}
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

func solverType(t string) sourcev1.SolverType {
	if t == SolverByparr {
		return sourcev1.SolverType_SOLVER_TYPE_BYPARR
	}
	return sourcev1.SolverType_SOLVER_TYPE_FLARESOLVERR
}

// Hash identifies a profile as pushed (secrets included, so a changed
// password is pushed again); nil is [UnsetHash].
func Hash(m proto.Message) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return UnsetHash
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Push is what was last pushed to a source or provider.
type Push = db.NetworkPush

// Pushes maps a kind's targets to what was last pushed.
func (s *Service) Pushes(ctx context.Context, kind string) (map[string]Push, error) {
	rows, err := s.st.ListPushes(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Push, len(rows))
	for _, r := range rows {
		out[r.TargetID] = r
	}
	return out, nil
}

// RecordPush keeps the outcome of a push: the hash when it worked, the
// error (and no hash, so it's tried again) when not.
func (s *Service) RecordPush(ctx context.Context, kind, target, plugin, hash string, pushErr error) error {
	msg := ""
	if pushErr != nil {
		hash, msg = "", pushErr.Error()
	}
	return s.st.UpsertPush(ctx, db.UpsertPushParams{TargetKind: kind, TargetID: target, PluginID: text(plugin), Hash: hash, Error: msg})
}
