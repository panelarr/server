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

// DefaultConfigFollowUp are the waits before each sync of a plugin after
// its settings were saved: plugins apply new settings in the background
// (adding stores, installing extensions) and report problems in their
// health, which these syncs pick up.
var DefaultConfigFollowUp = []time.Duration{5 * time.Second, 30 * time.Second}

// SetConfigFollowUp changes the waits of the syncs after a settings save.
func (h *Host) SetConfigFollowUp(delays ...time.Duration) {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	h.followUp = slices.Clone(delays)
}

// chain is the queue of single-plugin syncs pending for one plugin.
type chain struct {
	waits []time.Duration
	// untilUp stops the chain once the plugin answers (rechecks of a
	// plugin found down); a settings follow-up runs all its syncs.
	untilUp bool
}

// scheduleRecheck rechecks a plugin found down after each recheck wait,
// until it answers again or the waits run out (the regular sync carries on
// from there).
func (h *Host) scheduleRecheck(id string) {
	h.rmu.Lock()
	delays := slices.Clone(h.recheck)
	h.rmu.Unlock()
	h.schedule(id, delays, true)
}

// scheduleFollowUp syncs a plugin a few times after its settings changed.
func (h *Host) scheduleFollowUp(id string) {
	h.rmu.Lock()
	delays := slices.Clone(h.followUp)
	h.rmu.Unlock()
	h.schedule(id, delays, false)
}

// schedule runs single-plugin syncs after the given waits. One chain runs
// per plugin: asking while one runs adds to it (a settings follow-up then
// runs to its end even if the plugin is up). The syncs log at debug level
// and aren't job runs.
func (h *Host) schedule(id string, delays []time.Duration, untilUp bool) {
	h.rmu.Lock()
	ctx := h.bg
	if ctx == nil || len(delays) == 0 {
		h.rmu.Unlock()
		return
	}
	if c := h.rechecking[id]; c != nil {
		if !untilUp {
			c.waits = append(c.waits, delays...)
			c.untilUp = false
		}
		h.rmu.Unlock()
		return
	}
	c := &chain{waits: delays, untilUp: untilUp}
	h.rechecking[id] = c
	h.rmu.Unlock()

	go func() {
		defer func() {
			h.rmu.Lock()
			delete(h.rechecking, id)
			h.rmu.Unlock()
		}()
		down := false
		for i := 1; ; i++ {
			h.rmu.Lock()
			if len(c.waits) == 0 && down && !c.untilUp {
				// A settings follow-up left the plugin down: recheck it
				// like any plugin found down.
				c.waits, c.untilUp = slices.Clone(h.recheck), true
			}
			if len(c.waits) == 0 {
				h.rmu.Unlock()
				return
			}
			d := c.waits[0]
			c.waits = c.waits[1:]
			h.rmu.Unlock()

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
				h.log.DebugContext(ctx, "plugin recheck failed", "plugin", id, "attempt", i, "err", err)
				continue
			}
			h.log.DebugContext(ctx, "plugin recheck", "plugin", id, "attempt", i, "health", p.Health)
			down = p.Health == "down"
			h.rmu.Lock()
			done := c.untilUp && !down
			h.rmu.Unlock()
			if done {
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
