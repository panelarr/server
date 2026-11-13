// SPDX-License-Identifier: GPL-3.0-or-later

package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/importer/mihonbackup/backuptest"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/meta/metatest"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
	"github.com/panelarr/server/internal/users"
)

// node is one server process over a shared database: its own queue and
// services, without source plugins (series import through the catalogue).
type node struct {
	q    *queue.Queue
	imp  *Service
	stop func()
}

func newNode(t *testing.T, st *store.Store, metaURL, keyDir string) *node {
	t.Helper()
	ctx := context.Background()
	box := secret.NewBox("", filepath.Join(keyDir, "secret.key"))
	m := meta.NewService(st, box, meta.Conn{URL: metaURL, APIKey: metatest.AdminKey}, nil)
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	host := plugins.NewHost(st, box, plugins.RemoteDialer, nil)
	q := queue.New(st, hub, nil, nil, nil)
	q.Poll = 20 * time.Millisecond
	lib := library.NewService(st, m, host, q, nil, hub, nil)
	chs := chapters.NewService(st, host, q, nil, hub, nil)
	lib.SetChapterHooks(chs)
	imp := NewService(st, lib, m, host, q, hub, Options{TempDir: t.TempDir(), Gate: NewReadGate()}, nil)
	chs.OnUnified(imp.WorkChaptersChanged)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(runCtx) }()
	var once sync.Once
	n := &node{q: q, imp: imp, stop: func() { once.Do(func() { cancel(); <-done }) }}
	t.Cleanup(n.stop)
	return n
}

// catalogue serves five made-up works by one author, e0 … e4.
func catalogue(t *testing.T) *metatest.Fake {
	t.Helper()
	mf := metatest.New(t)
	mf.SetProviders(map[string]int64{"books": 5})
	for i := range 5 {
		title := fmt.Sprintf("Work Number %d", i)
		mf.AddEntry(&metav1.Entry{
			Id: fmt.Sprintf("e%d", i), Title: title, ContentRating: metadatav1.ContentRating_CONTENT_RATING_SAFE,
			Type:    metadatav1.EntryType_ENTRY_TYPE_MANGA,
			Titles:  []*metadatav1.Title{{Text: title, Language: "en", IsPrimary: true}},
			Credits: []*metadatav1.Credit{{Person: &metadatav1.Person{Name: "A. Author"}, Role: metadatav1.PersonRole_PERSON_ROLE_AUTHOR}},
		})
	}
	return mf
}

// fiveSeries is a backup of the five works on a source Panelarr lacks.
func fiveSeries() []byte {
	var b backuptest.Backup
	for i := range 5 {
		b.Series = append(b.Series, backuptest.Series{
			SourceID: 9999, URL: fmt.Sprintf("/w/%d", i), Title: fmt.Sprintf("Work Number %d", i), Author: "A. Author",
			DateAdded: added, Chapters: []backuptest.Chapter{{URL: fmt.Sprintf("/w/%d/1", i), Number: 1, Read: true}},
		})
	}
	return b.File()
}

func waitState(t *testing.T, s *Service, user, id uuid.UUID, ok func(string) bool) db.Import {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		imp, err := s.Get(context.Background(), user, id)
		if err != nil {
			t.Fatal(err)
		}
		if ok(imp.State) {
			return imp
		}
		if time.Now().After(deadline) {
			t.Fatalf("import stays %s", imp.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func is(states ...string) func(string) bool {
	return func(s string) bool {
		for _, x := range states {
			if s == x {
				return true
			}
		}
		return false
	}
}

func previewed(t *testing.T, n *node, st *store.Store, user uuid.UUID, file []byte) db.Import {
	t.Helper()
	imp, created, err := n.imp.Upload(context.Background(), viewerFor(t, st, user), bytes.NewReader(file))
	if err != nil || !created {
		t.Fatalf("upload: %v %v", created, err)
	}
	return waitState(t, n.imp, user, imp.ID, is(StatePreview))
}

func viewerFor(t *testing.T, st *store.Store, id uuid.UUID) library.Viewer {
	t.Helper()
	u, err := st.GetUser(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return library.Viewer{UserID: id, Grants: users.Effective(u)}
}

func items(t *testing.T, st *store.Store, id uuid.UUID) []db.ImportItem {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `SELECT idx, outcome, result, applied_at FROM import_items WHERE import_id = $1 ORDER BY idx`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []db.ImportItem
	for rows.Next() {
		var it db.ImportItem
		if err := rows.Scan(&it.Idx, &it.Outcome, &it.Result, &it.AppliedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, it)
	}
	return out
}

func libraryCount(t *testing.T, st *store.Store, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM library_entries WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestApplyResumesAfterARestart: a server that stops mid-apply leaves the
// task to the next one, which goes on from the first series not applied,
// without applying any twice.
func TestApplyResumesAfterARestart(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	mf := catalogue(t)
	keys := t.TempDir()
	first := newNode(t, st, mf.URL, keys)
	user := newUser(t, st, "restart")
	imp := previewed(t, first, st, user, fiveSeries())

	// The first server stops right after applying series 1.
	stopped := make(chan struct{})
	first.imp.afterApplyItem = func(idx int32) {
		if idx == 1 {
			go first.stop()
			<-stopped
		}
	}
	first.q.SetPaused(KindPending, true) //nolint:errcheck // a known kind
	if _, err := first.imp.Apply(context.Background(), user, imp.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first apply to reach series 1", func() bool {
		return len(applied(items(t, st, imp.ID))) == 2
	})
	close(stopped)
	first.stop()
	if got := applied(items(t, st, imp.ID)); len(got) != 2 {
		t.Fatalf("applied before the restart: %v", got)
	}
	if cur, _ := first.imp.Get(context.Background(), user, imp.ID); cur.State != StateApplying {
		t.Fatalf("state after the stop: %s", cur.State)
	}

	second := newNode(t, st, mf.URL, keys)
	var again []int32
	var mu sync.Mutex
	second.imp.afterApplyItem = func(idx int32) {
		mu.Lock()
		again = append(again, idx)
		mu.Unlock()
	}
	done := waitState(t, second.imp, user, imp.ID, is(StateDone, StateFailed))
	if done.State != StateDone {
		t.Fatalf("after the restart: %+v", done)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(again) != "[2 3 4]" {
		t.Fatalf("the second server applied %v", again)
	}
	for _, it := range items(t, st, imp.ID) {
		if it.Result != ResultApplied {
			t.Fatalf("series %d: %s", it.Idx, it.Outcome)
		}
	}
	if n := libraryCount(t, st, user); n != 5 {
		t.Fatalf("%d library entries", n)
	}
}

func applied(its []db.ImportItem) []int32 {
	var out []int32
	for _, it := range its {
		if it.AppliedAt != nil {
			out = append(out, it.Idx)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCancelMidway: cancelling an apply stops it between series; what was
// applied stays and is reported, the rest stays as the preview left it.
// Applying it again resumes with the rest.
func TestCancelMidway(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	mf := catalogue(t)
	n := newNode(t, st, mf.URL, t.TempDir())
	user := newUser(t, st, "cancel")
	imp := previewed(t, n, st, user, fiveSeries())
	reached, release := make(chan struct{}), make(chan struct{})
	n.imp.afterApplyItem = func(idx int32) {
		if idx == 1 {
			close(reached)
			<-release
		}
	}
	if _, err := n.imp.Apply(context.Background(), user, imp.ID); err != nil {
		t.Fatal(err)
	}
	<-reached
	out, err := n.imp.Cancel(context.Background(), user, imp.ID)
	if err != nil || out.State != StateCancelled || out.FinishedAt == nil {
		close(release)
		t.Fatalf("cancel: %+v %v", out, err)
	}
	// Applied again at once, while the cancelled task still runs: refused,
	// rather than handed that dying task (the import would end failed).
	if _, err := n.imp.Apply(context.Background(), user, imp.ID); !errors.Is(err, ErrRunning) {
		close(release)
		t.Fatalf("apply while the cancelled task runs: %v", err)
	}
	if cur, _ := n.imp.Get(context.Background(), user, imp.ID); cur.State != StateCancelled || cur.TaskID != out.TaskID {
		close(release)
		t.Fatalf("after the refused apply: %+v", cur)
	}
	close(release)
	if _, err := n.imp.Cancel(context.Background(), user, imp.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel again: %v", err)
	}
	// The task ends cancelled.
	waitFor(t, "the task to end", func() bool {
		tk, err := n.q.Get(context.Background(), out.TaskID.UUID)
		return err == nil && tk.State != queue.StateRunning && tk.State != queue.StateQueued
	})
	got := items(t, st, imp.ID)
	if fmt.Sprint(applied(got)) != "[0 1]" {
		t.Fatalf("applied %v", applied(got))
	}
	for _, it := range got[2:] {
		if it.Outcome != OutcomeSourceMissing {
			t.Fatalf("series %d after cancel: %s", it.Idx, it.Outcome)
		}
	}
	if c := libraryCount(t, st, user); c != 2 {
		t.Fatalf("%d library entries", c)
	}
	if cur, _ := n.imp.Get(context.Background(), user, imp.ID); cur.State != StateCancelled {
		t.Fatalf("state %s", cur.State)
	}
	// Applied again, it resumes after the series applied.
	var again []int32
	var mu sync.Mutex
	n.imp.afterApplyItem = func(idx int32) {
		mu.Lock()
		again = append(again, idx)
		mu.Unlock()
	}
	if _, err := n.imp.Apply(context.Background(), user, imp.ID); err != nil {
		t.Fatalf("apply a cancelled import: %v", err)
	}
	if done := waitState(t, n.imp, user, imp.ID, is(StateDone, StateFailed)); done.State != StateDone || done.FinishedAt == nil {
		t.Fatalf("resumed %+v", done)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(again) != "[2 3 4]" {
		t.Fatalf("resumed with %v", again)
	}
	if c := libraryCount(t, st, user); c != 5 {
		t.Fatalf("%d library entries", c)
	}
	// Done: not again.
	if _, err := n.imp.Apply(context.Background(), user, imp.ID); !errors.Is(err, ErrState) {
		t.Fatalf("apply a done import: %v", err)
	}
}

// TestApplyNeedsAFinishedPreview: an import cancelled while it previewed
// can't be applied: some of its series were never matched.
func TestApplyNeedsAFinishedPreview(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	q := queue.New(st, nil, nil, nil, nil) // never run: the preview doesn't start
	s := NewService(st, nil, nil, nil, q, events.NewHub(), Options{TempDir: t.TempDir(), Gate: NewReadGate()}, nil)
	user := newUser(t, st, "early")
	imp, _, err := s.Upload(context.Background(), viewerOf(user), bytes.NewReader(fiveSeries()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(context.Background(), user, imp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), user, imp.ID); !errors.Is(err, ErrState) {
		t.Fatalf("apply: %v", err)
	}
}

// TestOneRunningImportIsEnforcedByTheIndex: two uploads of different
// files by one user that both pass the check for a running import are
// stopped by the partial unique index: one is created, the other refused.
// With a plain index, both would run.
func TestOneRunningImportIsEnforcedByTheIndex(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	q := queue.New(st, nil, nil, nil, nil) // never run: imports stay previewing
	s := NewService(st, nil, nil, nil, q, nil, Options{TempDir: t.TempDir(), Gate: NewReadGate(), GateWait: time.Minute}, nil)
	user := newUser(t, st, "twice")
	var checked sync.WaitGroup
	checked.Add(2)
	var reads atomic.Int32
	s.beforeRead = func() {
		// The first read waits until both uploads have passed the check
		// for a running import (the second is then waiting for the gate).
		if reads.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			_, _, err := s.Upload(context.Background(), viewerOf(user), bytes.NewReader(sample(fmt.Sprint("Title ", i)).File()))
			errs <- err
		}()
	}
	var ok, running int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrRunning):
			running++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || running != 1 {
		t.Fatalf("%d created, %d refused", ok, running)
	}
	var n int
	if err := st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM imports WHERE user_id = $1`, user).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d imports (%v)", n, err)
	}
	// Directly: a second running import of one user is refused by the
	// index itself.
	if _, err := st.CreateImport(context.Background(), db.CreateImportParams{UserID: user, Format: FormatMihonBackup, FileSha256: "x", Summary: []byte("{}")}); !isUniqueViolation(err) {
		t.Fatalf("second running import: %v", err)
	}
}

// TestSweepTemp: leftover uploads are deleted at startup; other files
// aren't.
func TestSweepTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"panelarr-import-123", "panelarr-import-456", "keep-me", "panelarr-download-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "panelarr-import-dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	s := &Service{opts: Options{TempDir: dir}, log: slog.New(slog.DiscardHandler)}
	n, err := s.SweepTemp()
	if err != nil || n != 2 {
		t.Fatalf("swept %d: %v", n, err)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[keep-me panelarr-download-1 panelarr-import-dir]" {
		t.Fatalf("left %v", names)
	}
	// Without a directory of its own, the shared one is left alone.
	if n, err := (&Service{log: slog.New(slog.DiscardHandler)}).SweepTemp(); n != 0 || err != nil {
		t.Fatalf("no dir: %d %v", n, err)
	}
}

func TestChapterNumbers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   float32
		want float64
	}{{1, 1}, {1.1, 1.1}, {10.5, 10.5}, {0, 0}, {123.45, 123.45}} {
		if got := number(c.in); got != c.want {
			t.Errorf("number(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	undated := now.Add(-time.Hour)
	if got, dated := readTime(0, undated, now); !got.Equal(undated) || dated {
		t.Errorf("undated: %v, %v", got, dated)
	}
	if got, dated := readTime(readAt.UnixMilli(), undated, now); !got.Equal(readAt) || !dated {
		t.Errorf("dated: %v, %v", got, dated)
	}
	if got, dated := readTime(now.Add(24*time.Hour).UnixMilli(), undated, now); !got.Equal(now) || !dated {
		t.Errorf("future: %v, %v", got, dated)
	}
}

func TestPendingBackoff(t *testing.T) {
	t.Parallel()
	var total time.Duration
	for a := int32(1); total < PendingFor; a++ {
		d := pendingBackoff(a)
		if d < 5*time.Minute || d > 3*time.Hour {
			t.Fatalf("attempt %d waits %s", a, d)
		}
		total += d
		if a > 40 {
			t.Fatal("more attempts than the task kind allows")
		}
	}
}
