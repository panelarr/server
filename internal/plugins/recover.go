// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"slices"
	"time"
)

// DefaultRecheck are the waits before each recheck of a plugin found down,
// so one that was only briefly away (restarting next to the server) shows
// as ok again within seconds instead of at the next full sync.
var DefaultRecheck = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute}

// DefaultStartupGrace are the waits for a plugin that doesn't answer yet
// when the server starts, before its first sync marks it down.
var DefaultStartupGrace = []time.Duration{time.Second, 2 * time.Second}

// SetRecheck changes the waits between rechecks of a plugin found down
// (none: no rechecks).
func (h *Host) SetRecheck(delays ...time.Duration) {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	h.recheck = slices.Clone(delays)
}

// SetStartupGrace changes the waits for plugins not answering at startup.
func (h *Host) SetStartupGrace(delays ...time.Duration) {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	h.grace = slices.Clone(delays)
}

// scheduleRecheck rechecks a plugin found down after each recheck wait,
// until it answers again or the waits run out (the regular sync carries on
// from there). One chain runs per plugin; the checks log at debug level
// and aren't job runs.
func (h *Host) scheduleRecheck(id string) {
	h.rmu.Lock()
	ctx, delays := h.bg, slices.Clone(h.recheck)
	if ctx == nil || len(delays) == 0 || h.rechecking[id] {
		h.rmu.Unlock()
		return
	}
	h.rechecking[id] = true
	h.rmu.Unlock()

	go func() {
		defer func() {
			h.rmu.Lock()
			delete(h.rechecking, id)
			h.rmu.Unlock()
		}()
		for i, d := range delays {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			p, err := h.sync(ctx, id)
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDisabled) {
				return // removed or turned off meanwhile
			}
			if err != nil {
				h.log.DebugContext(ctx, "plugin recheck failed", "plugin", id, "attempt", i+1, "err", err)
				continue
			}
			h.log.DebugContext(ctx, "plugin recheck", "plugin", id, "attempt", i+1, "health", p.Health)
			if p.Health != "down" {
				return
			}
		}
	}()
}

// awaitReachable gives a plugin that doesn't answer a few short waits to
// come up (at startup, next to the server), so it isn't marked down for
// being a second late.
func (h *Host) awaitReachable(ctx context.Context, id string) {
	h.rmu.Lock()
	grace := slices.Clone(h.grace)
	h.rmu.Unlock()
	c, err := h.Client(ctx, id)
	if err != nil {
		return
	}
	for _, d := range grace {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := c.GetInfo(pctx)
		cancel()
		if err == nil {
			return
		}
		h.log.DebugContext(ctx, "plugin not answering yet", "plugin", id, "err", errorMessage(err))
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
