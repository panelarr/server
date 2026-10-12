// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"

	"github.com/panelarr/server/internal/network"
	"github.com/panelarr/server/internal/queue"
)

// Network profiles reach plugins here: each source's profile in force
// (see network.Resolver) is pushed with SetNetworkProfile to plugins that
// declare the network profile service, without the solver unless they
// declare the solver service. Only changes are pushed (by hash); after a
// plugin restarts (its instance ID changes, it comes back from down, or
// the server starts) everything is pushed again.

// KindNetworkPush is the task kind that pushes profiles after a change.
const KindNetworkPush = "network.push"

// pushTimeout bounds pushing one plugin's profiles.
const pushTimeout = 2 * time.Minute

// SetNetwork makes the host push network profiles from n, and queue a push
// whenever a profile or assignment changes.
func (h *Host) SetNetwork(n *network.Service, q *queue.Queue) {
	h.net = n
	q.Register(KindNetworkPush, queue.Kind{Handler: h.handleNetworkPush, Concurrency: 1, MaxAttempts: 3})
	n.SetChanged(func(ctx context.Context) {
		if _, _, err := q.Enqueue(ctx, queue.NewTask{Kind: KindNetworkPush, DedupeKey: KindNetworkPush, Priority: 10}); err != nil {
			h.log.WarnContext(ctx, "queue network push", "err", err)
		}
	})
}

func (h *Host) handleNetworkPush(ctx context.Context, _ *queue.Task) (any, error) {
	rows, err := h.st.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	pushed, failed := 0, 0
	for _, p := range rows {
		if !p.Enabled {
			continue
		}
		n, f, err := h.PushProfiles(ctx, p.ID, false)
		if err != nil {
			return nil, err
		}
		pushed, failed = pushed+n, failed+f
	}
	return map[string]int{"pushed": pushed, "failed": failed}, nil
}

// PushProfiles pushes a plugin's sources' profiles that changed (all of
// them with force) and keeps the server's own source limits in line with
// them. It returns how many pushes went and failed; failures are retried
// at the next sync.
func (h *Host) PushProfiles(ctx context.Context, id string, force bool) (pushed, failed int, err error) {
	if h.net == nil {
		return 0, 0, nil
	}
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	p, err := h.Get(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	if !p.Enabled {
		return 0, 0, nil
	}
	res, err := h.net.Resolver(ctx)
	if err != nil {
		return 0, 0, err
	}
	srcs, err := h.Sources(ctx, Viewer{Admin: true}, true)
	if err != nil {
		return 0, 0, err
	}
	accepts, solver := AcceptsProfiles(p.Info), AcceptsSolver(p.Info)
	var prev map[string]network.Push
	if accepts {
		if prev, err = h.net.Pushes(ctx, network.KindSource); err != nil {
			return 0, 0, err
		}
	}
	var c client.Client
	built := map[string]*sourcev1.NetworkProfile{}
	for _, s := range srcs {
		if s.PluginID != id || !s.Present {
			continue
		}
		eff := res.Source(s.ID, id, s.PackageID)
		var prof *sourcev1.NetworkProfile
		if eff.ProfileID != "" {
			if prof = built[eff.ProfileID]; prof == nil {
				if prof, err = h.net.SourceProfile(ctx, eff.ProfileID, solver); err != nil {
					return pushed, failed, err
				}
				built[eff.ProfileID] = prof
			}
		}
		h.limitFromProfile(p, s, prof)
		if !accepts {
			continue
		}
		hash := network.Hash(prof)
		last, seen := prev[s.ID]
		switch {
		case !seen && hash == network.UnsetHash:
			continue // never pushed, nothing to push
		case seen && last.Hash == hash && !force:
			continue
		}
		if c == nil {
			if c, err = h.Client(ctx, id); err != nil {
				return pushed, failed, err
			}
		}
		pctx, cancel := context.WithTimeout(ctx, callTimeout)
		perr := c.SetNetworkProfile(pctx, s.SourceID, prof)
		cancel()
		if perr != nil {
			failed++
			h.log.WarnContext(ctx, "network profile push failed", "plugin", id, "source", s.ID, "err", errorMessage(perr))
		} else {
			pushed++
		}
		if err := h.net.RecordPush(ctx, network.KindSource, s.ID, id, hash, wrapPush(perr)); err != nil {
			return pushed, failed, err
		}
	}
	if pushed+failed > 0 {
		h.log.InfoContext(ctx, "network profiles pushed", "plugin", id, "pushed", pushed, "failed", failed, "all", force)
	}
	return pushed, failed, nil
}

func wrapPush(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(errorMessage(err))
}

// limitFromProfile sets the server's own cap for a source: the profile's
// rate limit and concurrency where it sets them, else the plugin's
// declared limits.
func (h *Host) limitFromProfile(p Plugin, s Source, prof *sourcev1.NetworkProfile) {
	if h.limits == nil {
		return
	}
	l := sourceLimit(p.Info)
	if c := prof.GetConcurrency(); c > 0 {
		l.Concurrency = int(c)
	}
	if rl := prof.GetRateLimit(); rl.GetRequests() > 0 && rl.GetPeriod().AsDuration() > 0 &&
		rl.GetScope() != sourcev1.RateLimitScope_RATE_LIMIT_SCOPE_HOST {
		l.Every = rl.GetPeriod().AsDuration() / time.Duration(rl.GetRequests())
		l.Burst = max(int(rl.GetBurst()), 1)
	}
	h.limits.Set("source:"+s.ID, l)
}

// Applied is what a plugin says it applies to a source: "pushed" (the
// server's profile), "plugin" (its own settings) or "none" (direct), or
// "unknown" when the plugin can't say.
func (h *Host) Applied(ctx context.Context, sourceID string) (string, string, error) {
	s, err := h.Source(ctx, sourceID, Viewer{Admin: true, AnyKnown: true})
	if err != nil {
		return "", "", err
	}
	c, err := h.Client(ctx, s.PluginID)
	if err != nil {
		return "unknown", "", nil //nolint:nilerr // a plugin turned off can't say
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.GetNetworkProfile(ctx, s.SourceID)
	if err != nil {
		return "unknown", "", nil //nolint:nilerr // optional for plugins
	}
	switch res.GetOrigin() {
	case sourcev1.NetworkProfileOrigin_NETWORK_PROFILE_ORIGIN_PUSHED:
		return "pushed", res.GetProfile().GetName(), nil
	case sourcev1.NetworkProfileOrigin_NETWORK_PROFILE_ORIGIN_PLUGIN:
		return "plugin", res.GetProfile().GetName(), nil
	case sourcev1.NetworkProfileOrigin_NETWORK_PROFILE_ORIGIN_NONE:
		return "none", "", nil
	}
	return "unknown", "", nil
}
