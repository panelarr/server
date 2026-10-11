// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/jobs"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/store"
)

// taskRetention is how long finished tasks stay listed.
const taskRetention = 7 * 24 * time.Hour

// registerJobs adds the server's named background jobs.
func registerJobs(m *jobs.Manager, st *store.Store, a *auth.Service, host *plugins.Host) {
	m.Register(jobs.Definition{
		ID: "plugins.sync", Name: "Sync source plugins", Group: "Sources",
		Description: "Refreshes every enabled plugin's info, health and source list.",
		Interval:    5 * time.Minute, ScheduleSource: "built in",
		Run: func(ctx context.Context, r *jobs.Run) error {
			ok, down, err := host.SyncAll(ctx)
			r.Count("ok", int64(ok))
			r.Count("down", int64(down))
			r.SetMessage(fmt.Sprintf("%d ok, %d down", ok, down))
			return err
		},
	})
	m.Register(jobs.Definition{
		ID: "sessions.cleanup", Name: "Remove expired sessions", Group: "Maintenance",
		Description: "Deletes browser sessions that expired.",
		Interval:    time.Hour, ScheduleSource: "built in",
		Run: func(ctx context.Context, r *jobs.Run) error {
			n, err := a.CleanExpired(ctx)
			r.Count("removed", n)
			return err
		},
	})
	m.Register(jobs.Definition{
		ID: "tasks.prune", Name: "Prune finished tasks", Group: "Maintenance",
		Description: "Deletes tasks that succeeded or were cancelled more than a week ago. Failed tasks stay until retried or removed.",
		Interval:    6 * time.Hour, ScheduleSource: "built in",
		Run: func(ctx context.Context, r *jobs.Run) error {
			n, err := st.PruneFinishedTasks(ctx, ptr(time.Now().Add(-taskRetention)))
			r.Count("removed", n)
			return err
		},
	})
}

func ptr[T any](v T) *T { return &v }
