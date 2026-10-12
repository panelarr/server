// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"testing"
	"time"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/network"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
	"github.com/panelarr/server/internal/queue"
)

func TestSourceLimitsFollowTheProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, e := newHost(t)
	reg := limits.NewRegistry()
	h.SetLimits(reg)
	n := network.New(e.st, e.box, nil)
	h.SetNetwork(n, queue.New(e.st, nil, reg, nil, nil))
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: pluginstest.Serve(t, plugintest.New(), key), APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.CreateProfile(ctx, network.Profile{
		ID: "slow", Name: "Slow", Concurrency: 1,
		RateLimit: &network.RateLimit{Requests: 1, Period: 3 * time.Second, Scope: "source"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Assign(ctx, network.KindSource, "fake:1001", "slow"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.PushProfiles(ctx, "fake", false); err != nil {
		t.Fatal(err)
	}
	if l, _ := reg.Limit("source:fake:1001"); l.Concurrency != 1 || l.Every != 3*time.Second {
		t.Fatalf("profile's limit %+v", l)
	}
	// Without a profile: what the plugin declares (2 at once, 10 a second).
	if l, _ := reg.Limit("source:fake:2001"); l.Concurrency != 2 || l.Every != 100*time.Millisecond {
		t.Fatalf("plugin's limit %+v", l)
	}
}
