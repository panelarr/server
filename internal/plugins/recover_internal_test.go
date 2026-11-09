// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"
)

// fakeClock is a clock whose sleeps only move its time forward.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) bool {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return true
}

// lateClient fails GetInfo the way a plugin whose name doesn't resolve yet
// does, until the clock reaches up.
type lateClient struct {
	client.Client
	clock *fakeClock
	up    time.Time
	tries int
}

func (c *lateClient) GetInfo(context.Context) (*sourcev1.PluginInfo, error) {
	c.tries++
	if c.clock.now().Before(c.up) {
		return nil, errors.New("dial tcp: lookup plugin: server misbehaving")
	}
	return &sourcev1.PluginInfo{}, nil
}

func startingHost(clock *fakeClock) *Host {
	return &Host{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: clock.now, sleep: clock.sleep,
		grace: DefaultStartupGrace, window: DefaultStartupWindow, started: clock.now(),
	}
}

// A plugin that only resolves some seconds after the server started (it
// starts next to it) is waited for instead of being marked down.
func TestAwaitReachableWaitsOutTheStartupWindow(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := startingHost(clock)
	c := &lateClient{clock: clock, up: clock.t.Add(12 * time.Second)}
	if err := h.awaitReachable(context.Background(), "late", c); err != nil {
		t.Fatalf("gave up on a plugin up after 12s: %v (after %v)", err, clock.sleeps)
	}
	if waited := clock.t.Sub(h.started); waited < 12*time.Second || waited > 16*time.Second {
		t.Fatalf("waited %v (%v), want just past 12s", waited, clock.sleeps)
	}
}

// A plugin that never answers is given up on once the window ends, so its
// first sync marks it down.
func TestAwaitReachableGivesUpAfterTheWindow(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := startingHost(clock)
	c := &lateClient{clock: clock, up: clock.t.Add(time.Hour)}
	if err := h.awaitReachable(context.Background(), "gone", c); err == nil {
		t.Fatal("reachable?")
	}
	if waited := clock.t.Sub(h.started); waited != DefaultStartupWindow {
		t.Fatalf("waited %v (%v), want the window %v", waited, clock.sleeps, DefaultStartupWindow)
	}
	// One more try right at the end, not a flood of them.
	if c.tries > 12 {
		t.Fatalf("%d tries", c.tries)
	}

	// The window counts from startup: later, a plugin gets a single try.
	c2 := &lateClient{clock: clock, up: clock.t.Add(time.Second)}
	if err := h.awaitReachable(context.Background(), "later", c2); err == nil || c2.tries != 1 {
		t.Fatalf("after the window: %v, %d tries", err, c2.tries)
	}
}

func TestAwaitReachableStopsWithTheServer(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := startingHost(clock)
	h.sleep = sleepCtx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &lateClient{clock: clock, up: clock.t.Add(time.Hour)}
	if err := h.awaitReachable(ctx, "gone", c); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
