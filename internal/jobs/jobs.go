// SPDX-License-Identifier: GPL-3.0-or-later

// Package jobs runs the server's named background jobs (plugin sync,
// cleanups, update checks): on a schedule, at startup or by hand, one run
// at a time per job, each run recorded with its outcome, counts and notes.
// It follows the jobs model every Panelarr admin shares.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Run statuses.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
)

// Triggers.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerStartup  = "startup"
)

// Param types.
const (
	ParamBool       = "bool"
	ParamInt        = "int"
	ParamString     = "string"
	ParamStringList = "string_list"
)

// Errors callers map to responses.
var (
	ErrNotFound       = errors.New("jobs: not found")
	ErrNotCancellable = errors.New("jobs: this job can't be cancelled")
	ErrNotRunning     = errors.New("jobs: the run has finished")
)

// AlreadyRunningError is a manual run of a job that is running.
type AlreadyRunningError struct{ RunID uuid.UUID }

func (e *AlreadyRunningError) Error() string { return "jobs: already running as " + e.RunID.String() }

// BadParamError is a parameter that doesn't fit its definition.
type BadParamError struct{ Name, Reason string }

func (e *BadParamError) Error() string { return "jobs: param " + e.Name + ": " + e.Reason }

// Param is a value a run accepts.
type Param struct {
	Name        string
	Type        string
	Description string
	Required    bool
	Default     string
	Choices     []string
}

// Definition is a named job.
type Definition struct {
	// ID matches ^[a-z0-9][a-z0-9._-]*$ (no ":").
	ID          string
	Name        string
	Description string
	Group       string
	// Interval between scheduled runs; 0 means by hand only.
	Interval time.Duration
	// ScheduleSource says where the schedule is configured.
	ScheduleSource string
	Params         []Param
	Cancellable    bool
	RunOnStart     bool
	Run            func(ctx context.Context, r *Run) error
}

// MaxNotes caps the notes kept per run.
const MaxNotes = 200

// Note is a notable event of a run.
type Note struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Ref     string    `json:"ref,omitempty"`
}

// Progress is how far a run is.
type Progress struct {
	Phase string `json:"phase,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total,omitempty"`
	Unit  string `json:"unit,omitempty"`
}

// Run is a running job, handed to its function.
type Run struct {
	ID     uuid.UUID
	JobID  string
	Params map[string]string
	m      *Manager

	mu        sync.Mutex
	counts    map[string]int64
	notes     []Note
	message   string
	lastWrite time.Time
}

// Count adds delta to a named count (e.g. "synced").
func (r *Run) Count(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[name] += delta
}

// Note records a notable event; level is info, warn or error.
func (r *Run) Note(level, message, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.notes) < MaxNotes {
		r.notes = append(r.notes, Note{At: time.Now().UTC(), Level: level, Message: message, Ref: ref})
	}
}

// SetMessage sets the run's one-line outcome.
func (r *Run) SetMessage(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.message = msg
}

// SetProgress reports how far the run is (writes are throttled).
func (r *Run) SetProgress(p Progress) {
	r.mu.Lock()
	due := time.Since(r.lastWrite) >= time.Second
	if due {
		r.lastWrite = time.Now()
	}
	counts, notes := r.snapshot()
	r.mu.Unlock()
	if !due {
		return
	}
	raw, _ := json.Marshal(p)
	if err := r.m.st.SetJobRunProgress(context.Background(), db.SetJobRunProgressParams{ID: r.ID, Progress: raw, Counts: counts, Notes: notes}); err != nil {
		r.m.log.Warn("job progress", "job", r.JobID, "err", err)
	}
	r.m.hub.Publish("job.progress", map[string]any{"job_id": r.JobID, "run_id": r.ID, "progress": p}, events.Audience{Admins: true})
}

func (r *Run) snapshot() (counts, notes []byte) {
	counts, _ = json.Marshal(r.counts)
	notes, _ = json.Marshal(r.notes)
	return counts, notes
}

// DefaultRetention is how many runs are kept per job.
const DefaultRetention = 100

type active struct {
	runID  uuid.UUID
	cancel context.CancelFunc
}

// Manager runs the jobs.
type Manager struct {
	st        *store.Store
	log       *slog.Logger
	hub       *events.Hub
	metrics   *metrics.Metrics
	Retention int

	mu      sync.Mutex
	defs    map[string]Definition
	order   []string
	running map[string]*active
	next    map[string]time.Time
	baseCtx context.Context //nolint:containedctx // the manager's lifetime
	wg      sync.WaitGroup
}

// NewManager returns a manager. hub and m may be nil.
func NewManager(st *store.Store, hub *events.Hub, m *metrics.Metrics, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		st: st, hub: hub, metrics: m, log: log, Retention: DefaultRetention,
		defs: map[string]Definition{}, running: map[string]*active{}, next: map[string]time.Time{},
	}
}

// Register adds a job. Call before Start.
func (m *Manager) Register(d Definition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.defs[d.ID]; !ok {
		m.order = append(m.order, d.ID)
	}
	m.defs[d.ID] = d
}

// Start marks runs left running by a previous process as interrupted, runs
// the startup jobs and schedules the rest. It returns at once; jobs run
// until ctx ends. Wait waits for them.
func (m *Manager) Start(ctx context.Context) error {
	if n, err := m.st.FailInterruptedJobRuns(ctx); err != nil {
		return err
	} else if n > 0 {
		m.log.InfoContext(ctx, "interrupted job runs marked failed", "count", n)
	}
	m.mu.Lock()
	m.baseCtx = ctx
	now := time.Now()
	var startup []string
	for _, id := range m.order {
		d := m.defs[id]
		if d.Interval > 0 {
			m.next[id] = now.Add(d.Interval)
		}
		if d.RunOnStart {
			startup = append(startup, id)
		}
	}
	m.mu.Unlock()
	for _, id := range startup {
		if _, err := m.start(ctx, id, nil, TriggerStartup, ""); err != nil {
			m.log.WarnContext(ctx, "startup job", "job", id, "err", err)
		}
	}
	m.wg.Add(1)
	go m.schedule(ctx)
	return nil
}

// Wait blocks until every run and the scheduler have stopped.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) schedule(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.mu.Lock()
			var due []string
			for id, at := range m.next {
				if !now.Before(at) {
					due = append(due, id)
					m.next[id] = now.Add(m.defs[id].Interval)
				}
			}
			m.mu.Unlock()
			slices.Sort(due)
			for _, id := range due {
				if _, err := m.start(ctx, id, nil, TriggerSchedule, ""); err != nil {
					var busy *AlreadyRunningError
					if !errors.As(err, &busy) {
						m.log.WarnContext(ctx, "scheduled job", "job", id, "err", err)
					}
				}
			}
		}
	}
}

// RunNow starts a run by hand.
func (m *Manager) RunNow(ctx context.Context, jobID string, params map[string]string, by string) (db.JobRun, error) {
	return m.start(ctx, jobID, params, TriggerManual, by)
}

func (m *Manager) start(ctx context.Context, jobID string, params map[string]string, trigger, by string) (db.JobRun, error) {
	m.mu.Lock()
	d, ok := m.defs[jobID]
	base := m.baseCtx
	m.mu.Unlock()
	if !ok {
		return db.JobRun{}, ErrNotFound
	}
	if base == nil {
		base = ctx
	}
	vals, err := resolveParams(d.Params, params)
	if err != nil {
		return db.JobRun{}, err
	}
	rawParams, _ := json.Marshal(vals)

	m.mu.Lock()
	if cur := m.running[jobID]; cur != nil {
		m.mu.Unlock()
		if trigger == TriggerSchedule {
			now := time.Now()
			if _, err := m.st.CreateJobRun(ctx, db.CreateJobRunParams{
				JobID: jobID, Status: StatusSkipped, Trigger: trigger, Params: rawParams,
				Message: "still running " + cur.runID.String(), FinishedAt: &now,
			}); err != nil {
				m.log.WarnContext(ctx, "record skipped run", "job", jobID, "err", err)
			}
			m.metrics.JobFinished(jobID, StatusSkipped, trigger, 0)
		}
		return db.JobRun{}, &AlreadyRunningError{RunID: cur.runID}
	}
	row, err := m.st.CreateJobRun(ctx, db.CreateJobRunParams{
		JobID: jobID, Status: StatusRunning, Trigger: trigger, TriggeredBy: by, Params: rawParams,
	})
	if err != nil {
		m.mu.Unlock()
		return db.JobRun{}, err
	}
	runCtx, cancel := context.WithCancel(base)
	m.running[jobID] = &active{runID: row.ID, cancel: cancel}
	m.wg.Add(1)
	m.mu.Unlock()

	r := &Run{ID: row.ID, JobID: jobID, Params: vals, m: m, counts: map[string]int64{}}
	m.metrics.JobStarted(jobID)
	m.hub.Publish("job.started", map[string]any{"job_id": jobID, "run_id": row.ID}, events.Audience{Admins: true})
	go m.execute(runCtx, cancel, d, r, trigger)
	return row, nil
}

func (m *Manager) execute(ctx context.Context, cancel context.CancelFunc, d Definition, r *Run, trigger string) {
	defer m.wg.Done()
	defer cancel()
	begin := time.Now()
	err := func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("panic: %v", v)
			}
		}()
		return d.Run(ctx, r)
	}()

	status := StatusSucceeded
	r.mu.Lock()
	msg := r.message
	switch {
	case err == nil:
	case ctx.Err() != nil:
		status = StatusCancelled
		if msg == "" {
			msg = "cancelled"
		}
	default:
		status = StatusFailed
		msg = err.Error()
	}
	counts, notes := r.snapshot()
	r.mu.Unlock()

	bg := context.WithoutCancel(ctx) // record the outcome even when cancelled
	if _, ferr := m.st.FinishJobRun(bg, db.FinishJobRunParams{ID: r.ID, Status: status, Message: msg, Counts: counts, Notes: notes}); ferr != nil {
		m.log.Error("record job run", "job", d.ID, "err", ferr)
	}
	if _, perr := m.st.PruneJobRuns(bg, db.PruneJobRunsParams{JobID: d.ID, Keep: int32(m.Retention)}); perr != nil { //nolint:gosec // small
		m.log.Warn("prune job runs", "job", d.ID, "err", perr)
	}
	m.mu.Lock()
	delete(m.running, d.ID)
	m.mu.Unlock()
	m.metrics.JobFinished(d.ID, status, trigger, time.Since(begin))
	typ := "job.done"
	if status == StatusFailed {
		typ = "job.failed"
		m.log.Warn("job failed", "job", d.ID, "err", err)
	}
	m.hub.Publish(typ, map[string]any{"job_id": d.ID, "run_id": r.ID, "status": status}, events.Audience{Admins: true})
}

// Cancel stops a running run.
func (m *Manager) Cancel(ctx context.Context, runID uuid.UUID) (db.JobRun, error) {
	row, err := m.GetRun(ctx, runID)
	if err != nil {
		return db.JobRun{}, err
	}
	m.mu.Lock()
	d := m.defs[row.JobID]
	cur := m.running[row.JobID]
	m.mu.Unlock()
	// A run is recorded as finished a moment before it leaves the running
	// set: the record decides.
	if cur == nil || cur.runID != runID || row.Status != StatusRunning {
		return row, ErrNotRunning
	}
	if !d.Cancellable {
		return row, ErrNotCancellable
	}
	cur.cancel()
	return row, nil
}

// GetRun returns a run.
func (m *Manager) GetRun(ctx context.Context, id uuid.UUID) (db.JobRun, error) {
	row, err := m.st.GetJobRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.JobRun{}, ErrNotFound
	}
	return row, err
}

// View is a job with its live state.
type View struct {
	Definition
	Running      bool
	CurrentRunID uuid.UUID
	LastRun      *db.JobRun
	NextRunAt    *time.Time
}

// List returns every job.
func (m *Manager) List(ctx context.Context) ([]View, error) {
	last, err := m.st.LastJobRuns(ctx)
	if err != nil {
		return nil, err
	}
	byJob := map[string]db.JobRun{}
	for _, r := range last {
		byJob[r.JobID] = r
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.order))
	for _, id := range m.order {
		v := View{Definition: m.defs[id]}
		if cur := m.running[id]; cur != nil {
			v.Running, v.CurrentRunID = true, cur.runID
		}
		if r, ok := byJob[id]; ok {
			v.LastRun = &r
		}
		if at, ok := m.next[id]; ok {
			at := at
			v.NextRunAt = &at
		}
		out = append(out, v)
	}
	return out, nil
}

// Get returns one job.
func (m *Manager) Get(ctx context.Context, id string) (View, error) {
	all, err := m.List(ctx)
	if err != nil {
		return View{}, err
	}
	for _, v := range all {
		if v.ID == id {
			return v, nil
		}
	}
	return View{}, ErrNotFound
}

// Runs lists a job's runs, newest first, before a cursor.
func (m *Manager) Runs(ctx context.Context, jobID, status string, before *db.JobRun, limit int) ([]db.JobRun, error) {
	m.mu.Lock()
	_, ok := m.defs[jobID]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	p := db.ListJobRunsParams{JobID: jobID, Lim: int32(limit)} //nolint:gosec // bounded
	if status != "" {
		p.Status.String, p.Status.Valid = status, true
	}
	if before != nil {
		p.BeforeStarted = &before.StartedAt
		p.BeforeID = uuid.NullUUID{UUID: before.ID, Valid: true}
	}
	return m.st.ListJobRuns(ctx, p)
}

// resolveParams checks params against their definitions and fills in
// defaults: a scheduled run uses the defaults.
func resolveParams(defs []Param, in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for name := range in {
		if !slices.ContainsFunc(defs, func(p Param) bool { return p.Name == name }) {
			return nil, &BadParamError{Name: name, Reason: "unknown"}
		}
	}
	for _, p := range defs {
		v, ok := in[p.Name]
		if !ok || v == "" {
			if p.Required && p.Default == "" {
				return nil, &BadParamError{Name: p.Name, Reason: "required"}
			}
			v = p.Default
		}
		if v == "" {
			continue
		}
		switch p.Type {
		case ParamBool:
			if _, err := strconv.ParseBool(v); err != nil {
				return nil, &BadParamError{Name: p.Name, Reason: "not true or false"}
			}
		case ParamInt:
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return nil, &BadParamError{Name: p.Name, Reason: "not a whole number"}
			}
		}
		if len(p.Choices) > 0 {
			for _, part := range strings.Split(v, ",") {
				if !slices.Contains(p.Choices, strings.TrimSpace(part)) {
					return nil, &BadParamError{Name: p.Name, Reason: "not one of " + strings.Join(p.Choices, ", ")}
				}
			}
		}
		out[p.Name] = v
	}
	return out, nil
}
