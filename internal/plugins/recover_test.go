// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"testing"
	"time"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

func waitHealth(t *testing.T, h *plugins.Host, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		p, err := h.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Health == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health %s (%s), want %s", p.Health, p.HealthMessage, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDownPluginRecoversQuickly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := newHost(t)
	h.SetRecheck(50*time.Millisecond, 100*time.Millisecond, 150*time.Millisecond)
	svc := pluginstest.ServeSwitchable(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: svc.URL, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.Load(ctx); err != nil {
		t.Fatal(err)
	}

	svc.Down.Store(true)
	if p, err := h.Sync(ctx, "fake"); err != nil || p.Health != "down" {
		t.Fatalf("down: %v %+v", err, p)
	}
	// Several syncs while it's down start only one chain of rechecks.
	for range 3 {
		_, _ = h.Sync(ctx, "fake")
	}
	svc.Down.Store(false)
	// Back without waiting for the next full sync.
	waitHealth(t, h, "fake", "ok", 2*time.Second)
}

func TestRechecksStopAfterTheirWaits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := newHost(t)
	h.SetRecheck(20*time.Millisecond, 20*time.Millisecond)
	svc := pluginstest.ServeSwitchable(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: svc.URL, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.Load(ctx); err != nil {
		t.Fatal(err)
	}
	svc.Down.Store(true)
	_, _ = h.Sync(ctx, "fake")
	time.Sleep(300 * time.Millisecond)
	settled := svc.Requests.Load()
	time.Sleep(300 * time.Millisecond)
	if n := svc.Requests.Load(); n != settled {
		t.Fatalf("rechecks went on: %d → %d requests", settled, n)
	}
	waitHealth(t, h, "fake", "down", time.Second)
}

func TestStartupGrace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, e := newHost(t)
	svc := pluginstest.ServeSwitchable(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: svc.URL, APIKey: key}); err != nil {
		t.Fatal(err)
	}

	// A restarted server finds the plugin a moment late.
	restarted := plugins.NewHost(e.st, e.box, plugins.RemoteDialer, nil)
	restarted.SetStartupGrace(100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)
	restarted.SetRecheck() // only the grace may save it here
	svc.Down.Store(true)
	go func() {
		time.Sleep(150 * time.Millisecond)
		svc.Down.Store(false)
	}()
	if err := restarted.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := restarted.Get(ctx, "fake"); p.Health != "ok" {
		t.Fatalf("marked %s for being late", p.Health)
	}
}
