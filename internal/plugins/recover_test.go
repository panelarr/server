// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"testing"
	"time"

	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"
	"github.com/panelarr/source-api/sdk/settings"

	"github.com/panelarr/server/internal/events"
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

// A plugin still not answering when the startup window ends is marked down.
func TestStartupGraceEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, e := newHost(t)
	svc := pluginstest.ServeSwitchable(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: svc.URL, APIKey: key}); err != nil {
		t.Fatal(err)
	}

	restarted := plugins.NewHost(e.st, e.box, plugins.RemoteDialer, nil)
	restarted.SetStartupWindow(200 * time.Millisecond)
	restarted.SetStartupGrace(50 * time.Millisecond)
	restarted.SetRecheck()
	svc.Down.Store(true)
	if err := restarted.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := restarted.Get(ctx, "fake"); p.Health != "down" {
		t.Fatalf("unreachable plugin is %s after the window", p.Health)
	}
}

// configurable returns a fake plugin that takes settings.
func configurable(t *testing.T) *plugintest.Fake {
	t.Helper()
	schema, err := settings.Parse(`{"type":"object","properties":{"stores":{"type":"string"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	fake := plugintest.New()
	fake.Config = plugin.NewPluginConfig(schema, func(context.Context, map[string]any) error { return nil })
	return fake
}

func TestConfigSaveFollowUp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := newHost(t)
	h.SetConfigFollowUp(50*time.Millisecond, 100*time.Millisecond)
	hub := events.NewHub()
	h.SetHub(hub)
	fake := configurable(t)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: pluginstest.Serve(t, fake, key), APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.Load(ctx); err != nil {
		t.Fatal(err)
	}
	sub := hub.Subscribe(events.Viewer{Admin: true})
	defer sub.Close()
	users := hub.Subscribe(events.Viewer{})
	defer users.Close()

	before := fake.Calls()["ListSources"]
	if _, err := h.SetConfig(ctx, "fake", map[string]any{"stores": "a"}); err != nil {
		t.Fatal(err)
	}
	// Both follow-up syncs run, and tell admins.
	for i := range 2 {
		select {
		case e := <-sub.C:
			if e.Type != "plugin.health" {
				t.Fatalf("event %s", e.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no plugin.health event for sync %d", i+1)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := fake.Calls()["ListSources"] - before; n != 2 {
		t.Fatalf("%d syncs after the save, want 2", n)
	}
	if len(users.C) != 0 {
		t.Fatal("a non-admin saw plugin health")
	}
}

func TestConfigSaveFollowUpFindingItDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := newHost(t)
	// One follow-up, late enough to find the plugin down even on a busy
	// machine; only the rechecks it hands over to bring it back.
	h.SetConfigFollowUp(500 * time.Millisecond)
	h.SetRecheck(100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)
	svc := pluginstest.ServeSwitchable(t, configurable(t), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: svc.URL, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetConfig(ctx, "fake", map[string]any{"stores": "a"}); err != nil {
		t.Fatal(err)
	}
	// Applying the settings takes the plugin down for a moment.
	svc.Down.Store(true)
	waitHealth(t, h, "fake", "down", 3*time.Second)
	svc.Down.Store(false)
	// The follow-up carries on as rechecks.
	waitHealth(t, h, "fake", "ok", 3*time.Second)
}
