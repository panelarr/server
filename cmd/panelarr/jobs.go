// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/jobs"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/store"
)

// taskRetention is how long finished tasks stay listed.
const taskRetention = 7 * 24 * time.Hour

// registerJobs adds the server's named background jobs.
func registerJobs(m *jobs.Manager, st *store.Store, a *auth.Service, host *plugins.Host, lib *library.Service, chs *chapters.Service) {
	m.Register(jobs.Definition{
		ID: "updates.check", Name: "Check for new chapters", Group: "Library",
		Description: "Queues a chapter check of every attached series of works someone follows, spread over half an hour so sources aren't hit at once.",
		Interval:    6 * time.Hour, ScheduleSource: "built in",
		Run: func(ctx context.Context, r *jobs.Run) error {
			n, err := chs.ScheduleAll(ctx, 30*time.Minute)
			r.Count("queued", int64(n))
			return err
		},
	})
	m.Register(jobs.Definition{
		ID: "works.refresh", Name: "Refresh works from the catalogue", Group: "Library",
		Description: "Copies the latest titles, covers, statuses and ratings of followed works from the metadata service.",
		Interval:    24 * time.Hour, ScheduleSource: "built in",
		Params: []jobs.Param{{Name: "limit", Type: jobs.ParamInt, Default: "1000", Description: "Works per run, least recently refreshed first."}},
		Run: func(ctx context.Context, r *jobs.Run) error {
			limit, _ := strconv.Atoi(r.Params["limit"])
			updated, conflicts, err := lib.RefreshFromMeta(ctx, limit)
			r.Count("updated", int64(updated))
			r.Count("merged_entries", int64(conflicts))
			if conflicts > 0 {
				r.Note("warn", fmt.Sprintf("%d works point at entries the catalogue merged into another work's entry", conflicts), "")
			}
			return err
		},
	})
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
