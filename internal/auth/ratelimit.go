// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"sync"
	"time"
)

// FailureLimiter counts failures per key in a fixed window and blocks a key
// once it reaches the limit, until its window ends. It lives in memory: a
// restart forgets it, which is fine for slowing down password guessing.
type FailureLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*failures
}

type failures struct {
	count int
	reset time.Time
}

// NewFailureLimiter allows max failures per key per window.
func NewFailureLimiter(max int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{max: max, window: window, now: time.Now, entries: map[string]*failures{}}
}

// Blocked reports whether key is blocked, and for how long.
func (l *FailureLimiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	now := l.now()
	if !now.Before(e.reset) {
		delete(l.entries, key)
		return false, 0
	}
	if e.count >= l.max {
		return true, e.reset.Sub(now)
	}
	return false, 0
}

// Fail records a failure for key.
func (l *FailureLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.entries) > 10000 {
		l.sweep(now)
	}
	e, ok := l.entries[key]
	if !ok || !now.Before(e.reset) {
		e = &failures{reset: now.Add(l.window)}
		l.entries[key] = e
	}
	e.count++
}

// Reset forgets key's failures (after a successful login).
func (l *FailureLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *FailureLimiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.reset) {
			delete(l.entries, k)
		}
	}
}
