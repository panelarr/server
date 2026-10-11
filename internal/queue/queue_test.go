// SPDX-License-Identifier: GPL-3.0-or-later

package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
)

type fixture struct {
	st     *store.Store
	q      *queue.Queue
	hub    *events.Hub
	limits *limits.Registry
	cancel context.CancelFunc
	done   chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)
	hub := events.NewHub()
	lim := limits.NewRegistry()
	q := queue.New(st, hub, lim, nil, nil)
	q.Poll = 20 * time.Millisecond
	q.Backoff = func(int32) time.Duration { return 10 * time.Millisecond }
	return &fixture{st: st, q: q, hub: hub, limits: lim}
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		if err := f.q.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	t.Cleanup(f.stop)
}

func (f *fixture) stop() {
	if f.cancel != nil {
		f.cancel()
		<-f.done
		f.cancel = nil
	}
}

func waitState(t *testing.T, st *store.Store, id uuid.UUID, want string) db.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := st.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: state %s (%s), want %s", id, task.State, task.LastError, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type payload struct {
	N int `json:"n"`
}

func TestRunRetryAndDedupe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var calls atomic.Int32
	f.q.Register("demo.flaky", queue.Kind{Handler: func(_ context.Context, task *queue.Task) (any, error) {
		var p payload
		if err := task.Decode(&p); err != nil {
			return nil, err
		}
		if calls.Add(1) < 3 {
			return nil, errors.New("not yet")
		}
		return map[string]int{"doubled": p.N * 2}, nil
	}})
	ctx := context.Background()
	user := uuid.Nil

	task, created, err := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.flaky", Payload: payload{N: 21}, DedupeKey: "flaky:1", CreatedBy: user})
	if err != nil || !created {
		t.Fatalf("enqueue: %v %v", err, created)
	}
	again, created, err := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.flaky", Payload: payload{N: 99}, DedupeKey: "flaky:1"})
	if err != nil || created || again.ID != task.ID {
		t.Fatalf("dedupe: %v %v %v", err, created, again.ID)
	}
	if _, _, err := f.q.Enqueue(ctx, queue.NewTask{Kind: "nope"}); !errors.Is(err, queue.ErrUnknownKind) {
		t.Fatalf("unknown kind: %v", err)
	}

	f.start(t)
	done := waitState(t, f.st, task.ID, queue.StateSucceeded)
	if done.Attempts != 3 || string(done.Result) != `{"doubled": 42}` || done.FinishedAt == nil {
		t.Fatalf("done %+v %s", done, done.Result)
	}
	// A finished task no longer blocks its dedupe key.
	if _, created, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.flaky", DedupeKey: "flaky:1"}); !created {
		t.Fatal("finished task still dedupes")
	}
}

func TestFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.q.Register("demo.permanent", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) {
		return nil, queue.Permanent(errors.New("bad input"))
	}})
	f.q.Register("demo.always", queue.Kind{MaxAttempts: 2, Handler: func(context.Context, *queue.Task) (any, error) {
		return nil, errors.New("site down")
	}})
	f.q.Register("demo.panics", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) {
		panic("boom")
	}})
	f.q.Register("demo.ratelimited", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) {
		return nil, &queue.RetryAfterError{Err: errors.New("slow down"), After: time.Hour}
	}})
	ctx := context.Background()
	perm, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.permanent"})
	always, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.always"})
	panics, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.panics"})
	rl, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.ratelimited"})
	f.start(t)

	if got := waitState(t, f.st, perm.ID, queue.StateFailed); got.Attempts != 1 || got.LastError != "bad input" {
		t.Fatalf("permanent %+v", got)
	}
	if got := waitState(t, f.st, always.ID, queue.StateFailed); got.Attempts != 2 {
		t.Fatalf("max attempts %+v", got)
	}
	if got := waitState(t, f.st, panics.ID, queue.StateFailed); got.LastError != "panic: boom" {
		t.Fatalf("panic %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.st.GetTask(ctx, rl.ID)
		if got.Attempts == 1 && got.State == queue.StateQueued {
			if time.Until(got.RunAfter) < 50*time.Minute {
				t.Fatalf("retry-after ignored: %v", got.RunAfter)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rate-limited task: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Failed tasks can be retried by hand.
	if _, err := f.q.Retry(ctx, rl.ID); !errors.Is(err, queue.ErrNotRetrying) {
		t.Fatalf("retry queued: %v", err)
	}
	if _, err := f.q.Retry(ctx, perm.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitState(t, f.st, perm.ID, queue.StateFailed); got.Attempts != 1 {
		t.Fatalf("retried attempts %d", got.Attempts)
	}
}

func TestLimitKeysAndKindConcurrency(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.limits.Set("source:a", limits.Limit{Concurrency: 1})
	var mu sync.Mutex
	active, peak := map[string]int{}, map[string]int{}
	handler := func(_ context.Context, task *queue.Task) (any, error) {
		key := task.LimitKey + "|" + task.Kind
		mu.Lock()
		active[key]++
		peak[key] = max(peak[key], active[key])
		active["kind:"+task.Kind]++
		peak["kind:"+task.Kind] = max(peak["kind:"+task.Kind], active["kind:"+task.Kind])
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active[key]--
		active["kind:"+task.Kind]--
		mu.Unlock()
		return nil, nil
	}
	f.q.Register("demo.fetch", queue.Kind{Concurrency: 10, Handler: handler})
	f.q.Register("demo.narrow", queue.Kind{Concurrency: 1, Handler: handler})
	ctx := context.Background()
	var ids []uuid.UUID
	for range 4 {
		task, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.fetch", LimitKey: "source:a"})
		ids = append(ids, task.ID)
		task, _, _ = f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.narrow"})
		ids = append(ids, task.ID)
	}
	f.start(t)
	for _, id := range ids {
		waitState(t, f.st, id, queue.StateSucceeded)
	}
	if peak["source:a|demo.fetch"] != 1 {
		t.Fatalf("source cap broken: %d at once", peak["source:a|demo.fetch"])
	}
	if peak["kind:demo.narrow"] != 1 {
		t.Fatalf("kind cap broken: %d at once", peak["kind:demo.narrow"])
	}
}

func TestCancelPauseAndShutdown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	started := make(chan uuid.UUID, 10)
	f.q.Register("demo.slow", queue.Kind{Handler: func(ctx context.Context, task *queue.Task) (any, error) {
		started <- task.ID
		task.SetProgress(queue.Progress{Phase: "waiting", Done: 1, Total: 2, Unit: "steps"})
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	ctx := context.Background()
	u, err := f.st.CreateUser(ctx, db.CreateUserParams{
		Username: "owner", PasswordHash: "x", Role: "user", ContentRatingCeiling: "safe", AllowedSources: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := u.ID
	sub := f.hub.Subscribe(events.Viewer{UserID: owner})
	defer sub.Close()

	// Paused kinds wait.
	if err := f.q.SetPaused("demo.slow", true); err != nil {
		t.Fatal(err)
	}
	queued, _, err := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.slow", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	time.Sleep(100 * time.Millisecond)
	if got, _ := f.st.GetTask(ctx, queued.ID); got.State != queue.StateQueued {
		t.Fatalf("paused kind ran: %s", got.State)
	}
	// Cancelling a queued task.
	if got, err := f.q.Cancel(ctx, queued.ID); err != nil || got.State != queue.StateCancelled {
		t.Fatalf("cancel queued: %v %s", err, got.State)
	}
	if _, err := f.q.Cancel(ctx, queued.ID); !errors.Is(err, queue.ErrFinished) {
		t.Fatalf("cancel twice: %v", err)
	}

	// Cancelling a running task.
	_ = f.q.SetPaused("demo.slow", false)
	run, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.slow", CreatedBy: owner})
	<-started
	if got := waitProgress(t, f.st, run.ID); got == "" {
		t.Fatal("no progress written")
	}
	if _, err := f.q.Cancel(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, f.st, run.ID, queue.StateCancelled)

	// The owner hears about their task.
	seen := map[string]bool{}
	for len(sub.C) > 0 {
		seen[(<-sub.C).Type] = true
	}
	for _, typ := range []string{"task.queued", "task.started", "task.progress", "task.updated"} {
		if !seen[typ] {
			t.Errorf("owner missed %s (got %v)", typ, seen)
		}
	}

	// Stopping the server sends a running task back without using an attempt.
	inflight, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.slow"})
	<-started
	f.stop()
	got, _ := f.st.GetTask(ctx, inflight.ID)
	if got.State != queue.StateQueued || got.Attempts != 0 || got.LastError != "interrupted" {
		t.Fatalf("after shutdown: %+v", got)
	}
}

func waitProgress(t *testing.T, st *store.Store, id uuid.UUID) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := st.GetTask(context.Background(), id)
		if len(task.Progress) > 0 {
			return string(task.Progress)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

func TestExpiredLeasesRequeued(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.q.Register("demo.ok", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) { return nil, nil }})
	ctx := context.Background()
	task, _, _ := f.q.Enqueue(ctx, queue.NewTask{Kind: "demo.ok"})
	// Simulate a crash: claimed with a lease that ran out.
	past := time.Now().Add(-time.Minute)
	if _, err := f.st.StartTask(ctx, db.StartTaskParams{ID: task.ID, LeaseUntil: &past}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	waitState(t, f.st, task.ID, queue.StateSucceeded)
}
