// SPDX-License-Identifier: GPL-3.0-or-later

// Package queue runs the server's tasks: one database row per piece of work
// (match a work, check it for updates, download a chapter). Tasks are
// claimed with SKIP LOCKED, hold a lease while they run, retry with
// back-off, and are capped per kind and per limit key (a source), across
// all users' work. Identical live tasks are stored once (dedupe keys).
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Task states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Errors callers map to responses.
var (
	ErrNotFound    = errors.New("queue: task not found")
	ErrNotRetrying = errors.New("queue: only failed or cancelled tasks can be retried")
	ErrFinished    = errors.New("queue: the task has finished")
	ErrUnknownKind = errors.New("queue: unknown task kind")
)

// permanentError fails a task without retrying.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying (bad input, gone for good).
func Permanent(err error) error { return permanentError{err} }

// RetryAfterError asks for the next attempt no sooner than After (e.g. a
// site rate-limited the plugin).
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// Handler does a task's work and returns a result to keep (or nil).
type Handler func(ctx context.Context, t *Task) (result any, err error)

// Kind configures one kind of task.
type Kind struct {
	Handler Handler
	// Concurrency caps how many of this kind run at once. Default 2.
	Concurrency int
	// MaxAttempts before a task fails. Default 5.
	MaxAttempts int
}

// Progress is how far a running task is.
type Progress struct {
	Phase string `json:"phase,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total,omitempty"`
	Unit  string `json:"unit,omitempty"`
}

// Task is a running task, handed to its handler.
type Task struct {
	db.Task
	q *Queue

	mu        sync.Mutex
	progress  Progress
	lastWrite time.Time
}

// Decode reads the task's payload into v.
func (t *Task) Decode(v any) error { return json.Unmarshal(t.Payload, v) }

// progressEvery throttles progress writes and events.
const progressEvery = time.Second

// SetProgress reports how far the task is. Writes and events are throttled.
func (t *Task) SetProgress(p Progress) {
	t.mu.Lock()
	t.progress = p
	due := time.Since(t.lastWrite) >= progressEvery || (p.Total > 0 && p.Done >= p.Total)
	if due {
		t.lastWrite = time.Now()
	}
	t.mu.Unlock()
	if !due {
		return
	}
	raw, _ := json.Marshal(p)
	if err := t.q.st.SetTaskProgress(context.Background(), db.SetTaskProgressParams{ID: t.ID, Progress: raw}); err != nil {
		t.q.log.Warn("task progress", "task", t.ID, "err", err)
	}
	t.q.publish("task.progress", t.Task, &p)
}

// NewTask is a task to enqueue.
type NewTask struct {
	Kind    string
	Payload any
	// DedupeKey keeps one live (queued or running) task per key.
	DedupeKey string
	// LimitKey caps concurrent tasks per key, e.g. "source:<id>".
	LimitKey  string
	Priority  int32
	CreatedBy uuid.UUID
	// RunAfter delays the task.
	RunAfter time.Time
}

// Queue runs tasks.
type Queue struct {
	st      *store.Store
	log     *slog.Logger
	hub     *events.Hub
	limits  *limits.Registry
	metrics *metrics.Metrics

	// Workers caps all running tasks. Default 8.
	Workers int
	// Lease is how long a claimed task stays claimed without a heartbeat.
	Lease time.Duration
	// Poll is how often the queue looks for due tasks without a wake-up.
	Poll time.Duration
	// Backoff gives the delay before attempt n+1 after n failed attempts.
	Backoff func(attempts int32) time.Duration

	mu       sync.Mutex
	kinds    map[string]Kind
	paused   map[string]bool
	running  map[uuid.UUID]*running
	byKind   map[string]int
	wake     chan struct{}
	wg       sync.WaitGroup
	runCtx   context.Context //nolint:containedctx // the queue's lifetime
	stopping bool
}

type running struct {
	cancel    context.CancelFunc
	cancelled bool
}

// New returns a queue. hub, lim and m may be nil.
func New(st *store.Store, hub *events.Hub, lim *limits.Registry, m *metrics.Metrics, log *slog.Logger) *Queue {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if lim == nil {
		lim = limits.NewRegistry()
	}
	return &Queue{
		st: st, hub: hub, limits: lim, metrics: m, log: log,
		Workers: 8, Lease: 5 * time.Minute, Poll: 2 * time.Second, Backoff: DefaultBackoff,
		kinds: map[string]Kind{}, paused: map[string]bool{}, running: map[uuid.UUID]*running{},
		byKind: map[string]int{}, wake: make(chan struct{}, 1),
	}
}

// DefaultBackoff waits 30s, 1m, 2m, … up to an hour, with jitter.
func DefaultBackoff(attempts int32) time.Duration {
	d := 30 * time.Second << min(max(attempts-1, 0), 7)
	d = min(d, time.Hour)
	return d + time.Duration(rand.Int64N(int64(d/5)+1)) //nolint:gosec // jitter, not security
}

// Register adds a task kind. Call before Run.
func (q *Queue) Register(kind string, k Kind) {
	if k.Concurrency < 1 {
		k.Concurrency = 2
	}
	if k.MaxAttempts < 1 {
		k.MaxAttempts = 5
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.kinds[kind] = k
}

// Kinds lists the registered kinds.
func (q *Queue) Kinds() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.kinds))
	for k := range q.kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (q *Queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Enqueue adds a task, or returns the live task with the same dedupe key
// (created is then false).
func (q *Queue) Enqueue(ctx context.Context, n NewTask) (db.Task, bool, error) {
	return q.EnqueueWith(ctx, q.st.Queries, n)
}

// EnqueueWith enqueues with dbq, so the task can join a transaction.
func (q *Queue) EnqueueWith(ctx context.Context, dbq *db.Queries, n NewTask) (db.Task, bool, error) {
	q.mu.Lock()
	k, ok := q.kinds[n.Kind]
	q.mu.Unlock()
	if !ok {
		return db.Task{}, false, fmt.Errorf("%w: %s", ErrUnknownKind, n.Kind)
	}
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return db.Task{}, false, err
	}
	if n.Payload == nil {
		payload = []byte("{}")
	}
	runAfter := n.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Now()
	}
	createdBy := uuid.NullUUID{UUID: n.CreatedBy, Valid: n.CreatedBy != uuid.Nil}
	t, err := dbq.InsertTask(ctx, db.InsertTaskParams{
		Kind: n.Kind, Payload: payload, DedupeKey: n.DedupeKey, LimitKey: n.LimitKey, Priority: n.Priority,
		MaxAttempts: int32(k.MaxAttempts), RunAfter: runAfter, CreatedBy: createdBy, //nolint:gosec // small
	})
	if errors.Is(err, pgx.ErrNoRows) {
		live, err := dbq.GetLiveTaskByDedupeKey(ctx, n.DedupeKey)
		return live, false, err
	}
	if err != nil {
		return db.Task{}, false, err
	}
	q.publish("task.queued", t, nil)
	q.poke()
	return t, true, nil
}

// Run claims and runs tasks until ctx ends, then waits for running tasks
// to stop (they see ctx cancelled and go back to the queue).
func (q *Queue) Run(ctx context.Context) error {
	if n, err := q.st.RequeueExpiredTasks(ctx); err != nil {
		return err
	} else if n > 0 {
		q.log.InfoContext(ctx, "interrupted tasks requeued", "count", n)
	}
	q.mu.Lock()
	q.runCtx = ctx
	q.mu.Unlock()
	t := time.NewTicker(q.Poll)
	defer t.Stop()
	for {
		if err := q.claim(ctx); err != nil && ctx.Err() == nil {
			q.log.WarnContext(ctx, "claim tasks", "err", err)
		}
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.stopping = true
			q.mu.Unlock()
			q.wg.Wait()
			return nil
		case <-t.C:
		case <-q.wake:
		}
	}
}

// claimBatch is how many due tasks one claim looks at.
const claimBatch = 50

func (q *Queue) claim(ctx context.Context) error {
	q.mu.Lock()
	free := q.Workers - len(q.running)
	var kinds []string
	for name, k := range q.kinds {
		if !q.paused[name] && q.byKind[name] < k.Concurrency {
			kinds = append(kinds, name)
		}
	}
	q.mu.Unlock()
	if free <= 0 || len(kinds) == 0 {
		return nil
	}
	type claimed struct {
		task db.Task
		key  *limits.Key
	}
	var started []claimed
	err := q.st.InTx(ctx, func(dbq *db.Queries, _ pgx.Tx) error {
		started = started[:0]
		cands, err := dbq.ClaimCandidates(ctx, db.ClaimCandidatesParams{Kinds: kinds, Lim: claimBatch})
		if err != nil {
			return err
		}
		perKind := map[string]int{}
		for _, c := range cands {
			if len(started) >= free {
				break
			}
			q.mu.Lock()
			fits := q.byKind[c.Kind]+perKind[c.Kind] < q.kinds[c.Kind].Concurrency
			q.mu.Unlock()
			if !fits {
				continue
			}
			var key *limits.Key
			if c.LimitKey != "" {
				key = q.limits.For(c.LimitKey)
				if !key.TryAcquire() {
					continue
				}
			}
			t, err := dbq.StartTask(ctx, db.StartTaskParams{ID: c.ID, LeaseUntil: ptr(time.Now().Add(q.Lease))})
			if err != nil {
				if key != nil {
					key.Release()
				}
				return err
			}
			perKind[c.Kind]++
			started = append(started, claimed{task: t, key: key})
		}
		return nil
	})
	if err != nil {
		for _, c := range started {
			if c.key != nil {
				c.key.Release()
			}
		}
		return err
	}
	for _, c := range started {
		q.start(c.task, c.key)
	}
	return nil
}

func (q *Queue) start(t db.Task, key *limits.Key) {
	q.mu.Lock()
	base := q.runCtx
	k := q.kinds[t.Kind]
	ctx, cancel := context.WithCancel(base)
	r := &running{cancel: cancel}
	q.running[t.ID] = r
	q.byKind[t.Kind]++
	q.wg.Add(1)
	q.mu.Unlock()

	go func() {
		defer q.wg.Done()
		defer cancel()
		q.metrics.TaskStarted(t.Kind)
		q.publish("task.started", t, nil)
		begin := time.Now()

		hbDone := make(chan struct{})
		go q.heartbeat(ctx, t.ID, hbDone)
		task := &Task{Task: t, q: q}
		result, err := q.call(ctx, k.Handler, task)
		close(hbDone)
		if key != nil {
			key.Release()
		}

		q.mu.Lock()
		wasCancelled := r.cancelled
		shuttingDown := q.stopping || base.Err() != nil
		delete(q.running, t.ID)
		q.byKind[t.Kind]--
		q.mu.Unlock()

		outcome := q.finish(t, result, err, wasCancelled, shuttingDown)
		q.metrics.TaskEnded(t.Kind, outcome, time.Since(begin))
		q.poke()
	}()
}

// call runs a handler, turning a panic into a failure.
func (q *Queue) call(ctx context.Context, h Handler, t *Task) (result any, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = Permanent(fmt.Errorf("panic: %v", v))
		}
	}()
	return h(ctx, t)
}

func (q *Queue) heartbeat(ctx context.Context, id uuid.UUID, done <-chan struct{}) {
	t := time.NewTicker(q.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := q.st.ExtendTaskLease(context.WithoutCancel(ctx), db.ExtendTaskLeaseParams{ID: id, LeaseUntil: ptr(time.Now().Add(q.Lease))}); err != nil {
				q.log.Warn("extend task lease", "task", id, "err", err)
			}
		}
	}
}

// finish records how an attempt ended and returns the outcome label.
func (q *Queue) finish(t db.Task, result any, err error, cancelled, shuttingDown bool) string {
	ctx := context.Background()
	var (
		out     db.Task
		outcome string
		ferr    error
	)
	switch {
	case err == nil:
		raw, _ := json.Marshal(result)
		if result == nil {
			raw = nil
		}
		out, ferr = q.st.FinishTask(ctx, db.FinishTaskParams{ID: t.ID, State: StateSucceeded, Result: raw})
		outcome = StateSucceeded
	case cancelled:
		out, ferr = q.st.FinishTask(ctx, db.FinishTaskParams{ID: t.ID, State: StateCancelled, LastError: "cancelled"})
		outcome = StateCancelled
	case shuttingDown:
		// Not the task's fault: run it again after the restart, without
		// counting this attempt against it.
		out, ferr = q.st.UnclaimTask(ctx, t.ID)
		outcome = "interrupted"
	default:
		var perm permanentError
		if errors.As(err, &perm) || t.Attempts >= t.MaxAttempts {
			out, ferr = q.st.FinishTask(ctx, db.FinishTaskParams{ID: t.ID, State: StateFailed, LastError: truncate(err.Error())})
			outcome = StateFailed
			break
		}
		delay := q.Backoff(t.Attempts)
		var ra *RetryAfterError
		if errors.As(err, &ra) && ra.After > delay {
			delay = ra.After
		}
		out, ferr = q.st.RetryTaskLater(ctx, db.RetryTaskLaterParams{ID: t.ID, LastError: truncate(err.Error()), RunAfter: time.Now().Add(delay)})
		outcome = "retried"
	}
	if ferr != nil {
		q.log.Error("record task outcome", "task", t.ID, "err", ferr)
		return outcome
	}
	if outcome == StateFailed {
		q.log.Warn("task failed", "task", t.ID, "kind", t.Kind, "attempts", t.Attempts, "err", err)
	}
	q.publish("task.updated", out, nil)
	return outcome
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// Cancel stops a queued or running task.
func (q *Queue) Cancel(ctx context.Context, id uuid.UUID) (db.Task, error) {
	q.mu.Lock()
	r, isRunning := q.running[id]
	if isRunning {
		r.cancelled = true
		r.cancel()
	}
	q.mu.Unlock()
	if isRunning {
		return q.Get(ctx, id)
	}
	t, err := q.st.CancelQueuedTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := q.Get(ctx, id)
		if gerr != nil {
			return db.Task{}, gerr
		}
		return cur, ErrFinished
	}
	if err != nil {
		return db.Task{}, err
	}
	q.publish("task.updated", t, nil)
	return t, nil
}

// Retry queues a failed or cancelled task again.
func (q *Queue) Retry(ctx context.Context, id uuid.UUID) (db.Task, error) {
	t, err := q.st.RequeueTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := q.Get(ctx, id); gerr != nil {
			return db.Task{}, gerr
		}
		return db.Task{}, ErrNotRetrying
	}
	if err != nil {
		if isUniqueViolation(err) {
			// A live task for the same work exists already.
			return db.Task{}, ErrNotRetrying
		}
		return db.Task{}, err
	}
	q.publish("task.queued", t, nil)
	q.poke()
	return t, nil
}

// Get returns a task.
func (q *Queue) Get(ctx context.Context, id uuid.UUID) (db.Task, error) {
	t, err := q.st.GetTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Task{}, ErrNotFound
	}
	return t, err
}

// SetPaused pauses or resumes a kind: queued tasks of a paused kind wait;
// running ones finish.
func (q *Queue) SetPaused(kind string, paused bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.kinds[kind]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	if paused {
		q.paused[kind] = true
	} else {
		delete(q.paused, kind)
	}
	if !paused {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// KindState is a kind's live state.
type KindState struct {
	Kind        string
	Paused      bool
	Running     int
	Concurrency int
}

// State returns each kind's live state.
func (q *Queue) State() []KindState {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]KindState, 0, len(q.kinds))
	for name, k := range q.kinds {
		out = append(out, KindState{Kind: name, Paused: q.paused[name], Running: q.byKind[name], Concurrency: k.Concurrency})
	}
	slices.SortFunc(out, func(a, b KindState) int {
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		return 0
	})
	return out
}

// Event is the data of task events.
type Event struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Attempts  int32     `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	Progress  *Progress `json:"progress,omitempty"`
}

// publish tells admins and the task's creator about a change.
func (q *Queue) publish(typ string, t db.Task, p *Progress) {
	to := events.Audience{Admins: true}
	if t.CreatedBy.Valid {
		to.Users = []uuid.UUID{t.CreatedBy.UUID}
	}
	q.hub.Publish(typ, Event{ID: t.ID, Kind: t.Kind, State: t.State, Attempts: t.Attempts, LastError: t.LastError, Progress: p}, to)
}

func ptr[T any](v T) *T { return &v }

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
