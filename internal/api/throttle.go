// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"sync"
	"time"
)

// logThrottle lets a repeating warning through once per key and period,
// so a page full of failing images doesn't flood the log.
type logThrottle struct {
	every time.Duration
	mu    sync.Mutex
	last  map[string]time.Time
}

func newLogThrottle(every time.Duration) *logThrottle {
	return &logThrottle{every: every, last: map[string]time.Time{}}
}

// allow reports whether to log for key now.
func (t *logThrottle) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if at, ok := t.last[key]; ok && now.Sub(at) < t.every {
		return false
	}
	if len(t.last) > 10000 {
		clear(t.last)
	}
	t.last[key] = now
	return true
}
