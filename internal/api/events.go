// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/events"
)

// keepAliveEvery keeps proxies from closing a quiet stream.
const keepAliveEvery = 25 * time.Second

// streamEvents serves the caller's events as server-sent events. It is in
// the spec (streamEvents) but written by hand, because each event has to
// be flushed as it happens. Authorization ran already: the router applies
// the spec's policy.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		writeProblem(w, r, NewProblem(http.StatusUnauthorized, "unauthenticated", "sign in or send an API key"))
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer
	w.WriteHeader(http.StatusOK)

	sub := s.deps.Hub.Subscribe(events.Viewer{UserID: p.User.ID, Admin: p.IsAdmin()})
	defer sub.Close()

	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("event: ready\ndata: {}\n\n") {
		return
	}
	tick := time.NewTicker(keepAliveEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !write(": keep-alive\n\n") {
				return
			}
		case e, ok := <-sub.C:
			if !ok {
				return // fell behind; the client reconnects and refetches
			}
			data, err := json.Marshal(e.Data)
			if err != nil {
				continue
			}
			if !write("id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, data) {
				return
			}
		}
	}
}
