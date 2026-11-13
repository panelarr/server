// SPDX-License-Identifier: GPL-3.0-or-later

package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/importer/mihonbackup"
	"github.com/panelarr/server/internal/importer/mihonbackup/backuptest"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/store/storetest"
)

var (
	added   = time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	fetched = time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)
	readAt  = time.Date(2025, 8, 2, 20, 30, 0, 0, time.UTC)
)

func sample(title string) backuptest.Backup {
	return backuptest.Backup{
		Series: []backuptest.Series{{
			SourceID: 1001, URL: "/series/1", Title: title, Author: "A. Author, B. Writer", DateAdded: added,
			CategoryOrders: []int64{1},
			Chapters: []backuptest.Chapter{
				{URL: "/series/1/1", Number: 1, Read: true, DateFetch: fetched},
				{URL: "/series/1/2", Number: 2, Read: true},
				{URL: "/series/1/3", Number: 3, LastPage: 5, Bookmark: true},
				{URL: "/series/1/4", Number: 4},
			},
			History: []backuptest.History{{URL: "/series/1/2", LastRead: readAt}},
		}, {
			SourceID: 7, URL: "/h", Title: "History only", NotInLibrary: true,
			Chapters: []backuptest.Chapter{{URL: "/h/1", Number: 1, Read: true}},
		}},
		Categories: []backuptest.Category{{Name: "Reading", Order: 1}, {Name: "Later", Order: 2}},
		Sources:    []backuptest.Source{{ID: 1001, Name: "Example Comics"}},
	}
}

func TestRowsOf(t *testing.T) {
	t.Parallel()
	b, err := mihonbackup.Read(bytes.NewReader(sample("Alpha").File()), mihonbackup.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	raw, items, err := rowsOf(b)
	if err != nil {
		t.Fatal(err)
	}
	var sum Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Series != 1 || sum.OutOfLibrary != 1 || sum.ChaptersRead != 2 || sum.Bookmarks != 1 || sum.InProgress != 1 ||
		len(sum.Categories) != 2 || sum.Categories[0] != (Category{"Reading", 1}) || sum.Categories[1] != (Category{"Later", 0}) {
		t.Fatalf("summary %+v", sum)
	}
	if len(items) != 1 {
		t.Fatalf("%d items: only series in the library are imported", len(items))
	}
	it := items[0]
	if it.SourceCompatID != "mihon:1001" || it.SourceName != "Example Comics" || it.SeriesKey != "/series/1" ||
		it.ChaptersRead != 2 || it.Bookmarks != 1 || it.AddedAt == nil || !it.AddedAt.Equal(added) || it.Categories[0] != "Reading" {
		t.Fatalf("item %+v", it)
	}
	var chs []Chapter
	if err := json.Unmarshal(it.Chapters, &chs); err != nil {
		t.Fatal(err)
	}
	// Only chapters with something to import; read_at from history, else
	// the fetch date, else the date added.
	want := []Chapter{
		{Key: "/series/1/1", Number: 1, Read: true, ReadAt: fetched.UnixMilli()},
		{Key: "/series/1/2", Number: 2, Read: true, ReadAt: readAt.UnixMilli()},
		{Key: "/series/1/3", Number: 3, Page: 5, Bookmark: true, ReadAt: added.UnixMilli()},
	}
	if fmt.Sprint(chs) != fmt.Sprint(want) {
		t.Fatalf("chapters %+v, want %+v", chs, want)
	}
}

func TestSplitPeople(t *testing.T) {
	t.Parallel()
	if got := fmt.Sprint(splitPeople(" A. Author ,, B. Writer,")); got != "[A. Author B. Writer]" {
		t.Fatal(got)
	}
}

func newUser(t *testing.T, st *store.Store, name string) uuid.UUID {
	t.Helper()
	u, err := st.CreateUser(context.Background(), db.CreateUserParams{
		Username: name, DisplayName: name, PasswordHash: "x", Role: "user", MayAdd: true,
		ContentRatingCeiling: "suggestive", AllSources: true, AllowedSources: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestOneReadAtATime: concurrent uploads read their backups one after
// another, never two at once (each read may take hundreds of MiB).
func TestOneReadAtATime(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	q := queue.New(st, nil, nil, nil, nil)
	s := NewService(st, nil, nil, nil, q, nil, Options{TempDir: t.TempDir(), Gate: NewReadGate(), GateWait: time.Minute}, nil)
	var inRead, most atomic.Int32
	s.beforeRead = func() {
		n := inRead.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inRead.Add(-1)
	}
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		uid := newUser(t, st, fmt.Sprintf("user%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.Upload(context.Background(), viewerOf(uid), bytes.NewReader(sample(fmt.Sprint("Title ", i)).File()))
			if err == nil && !created {
				err = errors.New("not created")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if m := most.Load(); m != 1 {
		t.Fatalf("%d reads at once", m)
	}
}

// TestBusyWhenTheGateIsHeld: an upload that can't get the gate in time is
// refused as busy, and leaves nothing behind.
func TestBusyWhenTheGateIsHeld(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	q := queue.New(st, nil, nil, nil, nil)
	gate := NewReadGate()
	dir := t.TempDir()
	s := NewService(st, nil, nil, nil, q, nil, Options{TempDir: dir, Gate: gate, GateWait: 20 * time.Millisecond}, nil)
	uid := newUser(t, st, "busy")
	if !gate.Acquire(context.Background(), 0) {
		t.Fatal("gate")
	}
	if _, _, err := s.Upload(context.Background(), viewerOf(uid), bytes.NewReader(sample("A").File())); !errors.Is(err, ErrBusy) {
		t.Fatalf("err %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	gate.Release()
	if _, created, err := s.Upload(context.Background(), viewerOf(uid), bytes.NewReader(sample("A").File())); err != nil || !created {
		t.Fatalf("after release: %v %v", created, err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func viewerOf(id uuid.UUID) library.Viewer { return library.Viewer{UserID: id} }
