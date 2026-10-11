// SPDX-License-Identifier: GPL-3.0-or-later

// Package events fans server events out to connected clients (the
// server-sent event stream). Every event says who may see it, so each
// user's stream only carries their own and shared events.
package events

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Audience says who may receive an event.
type Audience struct {
	// All users.
	All bool
	// Admins (always included when All).
	Admins bool
	// Users by ID.
	Users []uuid.UUID
}

// Event is one server event.
type Event struct {
	// ID increases across the server's life.
	ID   int64
	Type string
	Data any
	To   Audience
}

// Viewer is a subscriber's identity.
type Viewer struct {
	UserID uuid.UUID
	Admin  bool
}

func (a Audience) allows(v Viewer) bool {
	if a.All || (a.Admins && v.Admin) {
		return true
	}
	for _, u := range a.Users {
		if u == v.UserID {
			return true
		}
	}
	return false
}

// Hub fans events out. Publishing never blocks: a subscriber that falls
// behind is dropped (its stream ends, and the client reconnects and
// refetches).
type Hub struct {
	seq  atomic.Int64
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[*Subscription]struct{}{}} }

// Subscription receives the events its viewer may see.
type Subscription struct {
	C      chan Event
	viewer Viewer
	hub    *Hub
	once   sync.Once
}

// subscriberBuffer is how many events a subscriber may fall behind.
const subscriberBuffer = 256

// Subscribe starts receiving events for v. Call Close when done.
func (h *Hub) Subscribe(v Viewer) *Subscription {
	s := &Subscription{C: make(chan Event, subscriberBuffer), viewer: v, hub: h}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Close stops the subscription and closes C.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		s.hub.mu.Unlock()
		close(s.C)
	})
}

// Publish sends an event to every subscriber allowed to see it. Safe on a
// nil hub.
func (h *Hub) Publish(typ string, data any, to Audience) {
	if h == nil {
		return
	}
	e := Event{ID: h.seq.Add(1), Type: typ, Data: data, To: to}
	h.mu.RLock()
	var slow []*Subscription
	for s := range h.subs {
		if !to.allows(s.viewer) {
			continue
		}
		select {
		case s.C <- e:
		default:
			slow = append(slow, s)
		}
	}
	h.mu.RUnlock()
	for _, s := range slow {
		s.Close()
	}
}

// Subscribers counts live subscriptions.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
