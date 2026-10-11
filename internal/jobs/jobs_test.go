// SPDX-License-Identifier: GPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/jobs"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
)

func waitRun(t *testing.T, st *store.Store, id uuid.UUID) db.JobRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := st.GetJobRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != jobs.StatusRunning {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitIdle waits until the manager has fully finished a job's run (the
// row is written first, then old runs are pruned and the job released).
func waitIdle(t *testing.T, m *jobs.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still running", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func start(t *testing.T, m *jobs.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
}

func TestManualRunsAndParams(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	m := jobs.NewManager(st, nil, nil, nil)
	release := make(chan struct{})
	m.Register(jobs.Definition{
		ID: "demo.work", Name: "Demo", Group: "Maintenance",
		Params: []jobs.Param{
			{Name: "force", Type: jobs.ParamBool, Default: "false"},
			{Name: "langs", Type: jobs.ParamStringList, Choices: []string{"en", "de"}},
		},
		Run: func(_ context.Context, r *jobs.Run) error {
			<-release
			r.Count("done", 2)
			r.Note("warn", "something odd", "ref-1")
			r.SetMessage("did " + r.Params["force"])
			return nil
		},
	})
	m.Register(jobs.Definition{ID: "demo.broken", Run: func(context.Context, *jobs.Run) error { return errors.New("broke") }})
	m.Register(jobs.Definition{ID: "demo.panics", Run: func(context.Context, *jobs.Run) error { panic("boom") }})
	start(t, m)
	ctx := context.Background()

	var bad *jobs.BadParamError
	for _, p := range []map[string]string{{"nope": "1"}, {"force": "maybe"}, {"langs": "en,fr"}} {
		if _, err := m.RunNow(ctx, "demo.work", p, "admin"); !errors.As(err, &bad) {
			t.Fatalf("%v: %v", p, err)
		}
	}
	if _, err := m.RunNow(ctx, "missing", nil, "admin"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	run, err := m.RunNow(ctx, "demo.work", map[string]string{"langs": "en,de"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var busy *jobs.AlreadyRunningError
	if _, err := m.RunNow(ctx, "demo.work", nil, "admin"); !errors.As(err, &busy) || busy.RunID != run.ID {
		t.Fatalf("second run: %v", err)
	}
	v, _ := m.Get(ctx, "demo.work")
	if !v.Running || v.CurrentRunID != run.ID {
		t.Fatalf("view %+v", v)
	}
	close(release)
	done := waitRun(t, st, run.ID)
	var counts map[string]int64
	_ = json.Unmarshal(done.Counts, &counts)
	var params map[string]string
	_ = json.Unmarshal(done.Params, &params)
	if done.Status != jobs.StatusSucceeded || done.Message != "did false" || counts["done"] != 2 ||
		params["langs"] != "en,de" || params["force"] != "false" || done.TriggeredBy != "admin" || done.Trigger != jobs.TriggerManual {
		t.Fatalf("done %+v %s", done, done.Params)
	}

	broken, _ := m.RunNow(ctx, "demo.broken", nil, "")
	if r := waitRun(t, st, broken.ID); r.Status != jobs.StatusFailed || r.Message != "broke" {
		t.Fatalf("broken %+v", r)
	}
	panicked, _ := m.RunNow(ctx, "demo.panics", nil, "")
	if r := waitRun(t, st, panicked.ID); r.Status != jobs.StatusFailed || r.Message != "panic: boom" {
		t.Fatalf("panic %+v", r)
	}
	if _, err := m.Cancel(ctx, broken.ID); !errors.Is(err, jobs.ErrNotRunning) {
		t.Fatalf("cancel finished: %v", err)
	}
}

func TestCancelScheduleAndSkip(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	m := jobs.NewManager(st, nil, nil, nil)
	m.Register(jobs.Definition{
		ID: "demo.slow", Cancellable: true, Interval: time.Second, RunOnStart: true,
		Run: func(ctx context.Context, _ *jobs.Run) error { <-ctx.Done(); return ctx.Err() },
	})
	m.Register(jobs.Definition{
		ID: "demo.stuck", Run: func(ctx context.Context, _ *jobs.Run) error { <-ctx.Done(); return ctx.Err() },
	})
	start(t, m)
	ctx := context.Background()

	v, _ := m.Get(ctx, "demo.slow")
	if !v.Running || v.NextRunAt == nil {
		t.Fatalf("startup run: %+v", v)
	}
	// While it runs, the schedule records skipped runs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := m.Runs(ctx, "demo.slow", jobs.StatusSkipped, nil, 10)
		if len(runs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no skipped run")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := m.Cancel(ctx, v.CurrentRunID); err != nil {
		t.Fatal(err)
	}
	if r := waitRun(t, st, v.CurrentRunID); r.Status != jobs.StatusCancelled || r.Trigger != jobs.TriggerStartup {
		t.Fatalf("cancelled %+v", r)
	}
	stuck, _ := m.RunNow(ctx, "demo.stuck", nil, "")
	if _, err := m.Cancel(ctx, stuck.ID); !errors.Is(err, jobs.ErrNotCancellable) {
		t.Fatalf("not cancellable: %v", err)
	}
}

func TestRetentionAndInterrupted(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	ctx := context.Background()
	// A run left running by a crashed process.
	old, err := st.CreateJobRun(ctx, db.CreateJobRunParams{JobID: "demo.quick", Status: jobs.StatusRunning, Trigger: jobs.TriggerManual, Params: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(st, nil, nil, nil)
	m.Retention = 3
	m.Register(jobs.Definition{ID: "demo.quick", Run: func(context.Context, *jobs.Run) error { return nil }})
	start(t, m)
	if r, _ := st.GetJobRun(ctx, old.ID); r.Status != jobs.StatusFailed || r.Message != "interrupted" {
		t.Fatalf("interrupted %+v", r)
	}
	for range 5 {
		r, err := m.RunNow(ctx, "demo.quick", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		waitRun(t, st, r.ID)
		waitIdle(t, m, "demo.quick")
	}
	runs, _ := m.Runs(ctx, "demo.quick", "", nil, 100)
	if len(runs) != 3 {
		t.Fatalf("%d runs kept, want 3", len(runs))
	}
	page, _ := m.Runs(ctx, "demo.quick", "", &runs[0], 100)
	if len(page) != 2 || page[0].ID != runs[1].ID {
		t.Fatalf("cursor page %d", len(page))
	}
}
