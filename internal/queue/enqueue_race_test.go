// SPDX-License-Identifier: GPL-3.0-or-later

package queue_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
)

// scriptedDB answers the task queries Enqueue makes from a script, so a
// test can replay an interleaving with other workers exactly, without a
// database.
type scriptedDB struct {
	t      *testing.T
	script []scripted
	calls  []string
}

type scripted struct {
	query string // sqlc query name
	id    uuid.UUID
	err   error
}

func (s *scriptedDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	name := strings.Fields(strings.TrimPrefix(sql, "-- name: "))[0]
	s.calls = append(s.calls, name)
	if len(s.script) == 0 {
		s.t.Fatalf("unexpected %s", name)
	}
	next := s.script[0]
	s.script = s.script[1:]
	if next.query != name {
		s.t.Fatalf("got %s, want %s", name, next.query)
	}
	return scriptedRow(next)
}

func (s *scriptedDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	s.t.Fatal("unexpected Exec")
	return pgconn.CommandTag{}, nil
}

func (s *scriptedDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	s.t.Fatal("unexpected Query")
	return nil, nil
}

type scriptedRow scripted

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*uuid.UUID) = r.id
	return nil
}

// A live task with the same dedupe key that ends between the insert and
// the lookup must not swallow the request: nothing would do the work. This
// lost network profile pushes made by a change made while the last push
// was wrapping up.
func TestEnqueueWhileTheLiveTaskEnds(t *testing.T) {
	t.Parallel()
	q := queue.New(nil, nil, nil, nil, nil)
	q.Register("demo.ok", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) { return nil, nil }})
	fresh := uuid.New()
	sdb := &scriptedDB{t: t, script: []scripted{
		{query: "InsertTask", err: pgx.ErrNoRows},             // the task is running
		{query: "HurryTask", err: pgx.ErrNoRows},              // so it can't be hurried
		{query: "GetLiveTaskByDedupeKey", err: pgx.ErrNoRows}, // and has just finished
		{query: "InsertTask", id: fresh},
	}}
	task, created, err := q.EnqueueWith(context.Background(), db.New(sdb), queue.NewTask{Kind: "demo.ok", DedupeKey: "k"})
	if err != nil || !created || task.ID != fresh {
		t.Fatalf("enqueue: %v %v %v (queries %v)", err, created, task.ID, sdb.calls)
	}
	if len(sdb.script) != 0 {
		t.Fatalf("queries left %v", sdb.script)
	}
}

// A cancelled caller stops asking again.
func TestEnqueueWhileTheLiveTaskEndsCancelled(t *testing.T) {
	t.Parallel()
	q := queue.New(nil, nil, nil, nil, nil)
	q.Register("demo.ok", queue.Kind{Handler: func(context.Context, *queue.Task) (any, error) { return nil, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sdb := &scriptedDB{t: t, script: []scripted{
		{query: "InsertTask", err: pgx.ErrNoRows},
		{query: "HurryTask", err: pgx.ErrNoRows},
		{query: "GetLiveTaskByDedupeKey", err: pgx.ErrNoRows},
	}}
	if _, _, err := q.EnqueueWith(ctx, db.New(sdb), queue.NewTask{Kind: "demo.ok", DedupeKey: "k"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueue: %v", err)
	}
}
