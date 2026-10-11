// SPDX-License-Identifier: GPL-3.0-or-later

package events

import (
	"testing"

	"github.com/google/uuid"
)

func TestAudience(t *testing.T) {
	h := NewHub()
	alice, bob := uuid.New(), uuid.New()
	a := h.Subscribe(Viewer{UserID: alice})
	b := h.Subscribe(Viewer{UserID: bob})
	admin := h.Subscribe(Viewer{UserID: uuid.New(), Admin: true})
	defer a.Close()
	defer b.Close()
	defer admin.Close()

	h.Publish("everyone", 1, Audience{All: true})
	h.Publish("admins", 2, Audience{Admins: true})
	h.Publish("alice", 3, Audience{Users: []uuid.UUID{alice}, Admins: true})

	got := func(s *Subscription) []string {
		var out []string
		for len(s.C) > 0 {
			out = append(out, (<-s.C).Type)
		}
		return out
	}
	if g := got(a); len(g) != 2 || g[0] != "everyone" || g[1] != "alice" {
		t.Fatalf("alice %v", g)
	}
	if g := got(b); len(g) != 1 || g[0] != "everyone" {
		t.Fatalf("bob %v", g)
	}
	if g := got(admin); len(g) != 3 {
		t.Fatalf("admin %v", g)
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h := NewHub()
	s := h.Subscribe(Viewer{UserID: uuid.New()})
	for i := range subscriberBuffer + 1 {
		h.Publish("e", i, Audience{All: true})
	}
	if h.Subscribers() != 0 {
		t.Fatal("slow subscriber kept")
	}
	n := 0
	for range s.C {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("%d buffered events", n)
	}
	s.Close() // twice is fine
	var nilHub *Hub
	nilHub.Publish("e", nil, Audience{All: true})
}
