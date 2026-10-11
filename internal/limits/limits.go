// SPDX-License-Identifier: GPL-3.0-or-later

// Package limits caps work per key (a source, later a host): how many
// tasks run at once, and how fast requests go. The caps hold across all
// users' work. They live in memory, which fits a single server instance.
package limits

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limit is the cap of one key.
type Limit struct {
	// Concurrency is how many tasks may hold the key at once (at least 1).
	Concurrency int
	// Every is the time between two requests; 0 means no rate limit.
	Every time.Duration
	// Burst is how many requests may go at once after a quiet spell.
	Burst int
}

// Default is the cap of keys nobody set: polite to sites that said
// nothing about their limits.
var Default = Limit{Concurrency: 2, Every: 500 * time.Millisecond, Burst: 4}

// Key is the live cap of one key.
type Key struct {
	sem     chan struct{}
	limiter *rate.Limiter
}

// Registry holds the caps of every key.
type Registry struct {
	mu   sync.Mutex
	keys map[string]*Key
	set  map[string]Limit
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{keys: map[string]*Key{}, set: map[string]Limit{}}
}

func newKey(l Limit) *Key {
	if l.Concurrency < 1 {
		l.Concurrency = 1
	}
	lim := rate.NewLimiter(rate.Inf, 1)
	if l.Every > 0 {
		burst := max(l.Burst, 1)
		lim = rate.NewLimiter(rate.Every(l.Every), burst)
	}
	return &Key{sem: make(chan struct{}, l.Concurrency), limiter: lim}
}

// Set changes a key's cap. Work holding the old cap finishes under it.
func (r *Registry) Set(key string, l Limit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.set[key]; ok && old == l {
		return
	}
	r.set[key] = l
	r.keys[key] = newKey(l)
}

// For returns the live cap of key.
func (r *Registry) For(key string) *Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.keys[key]
	if !ok {
		k = newKey(Default)
		r.keys[key] = k
	}
	return k
}

// TryAcquire takes a concurrency slot if one is free.
func (k *Key) TryAcquire() bool {
	select {
	case k.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot taken with TryAcquire.
func (k *Key) Release() { <-k.sem }

// Wait blocks until the next request may go.
func (k *Key) Wait(ctx context.Context) error { return k.limiter.Wait(ctx) }
