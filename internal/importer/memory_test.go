// SPDX-License-Identifier: GPL-3.0-or-later

package importer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panelarr/server/internal/importer/mihonbackup"
	"github.com/panelarr/server/internal/importer/mihonbackup/backuptest"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/storetest"
)

// TestUploadMemory measures the peak heap of one upload: the read, the
// rows and storing them, all under the read gate, and checks it stays at
// the read's own peak. docs/import.md gives the figures. It takes a while, so it runs only with
// PANELARR_IMPORT_MEMORY=1:
//
//	PANELARR_IMPORT_MEMORY=1 go test ./internal/importer -run TestUploadMemory -v
func TestUploadMemory(t *testing.T) {
	if os.Getenv("PANELARR_IMPORT_MEMORY") == "" {
		t.Skip("set PANELARR_IMPORT_MEMORY=1 to measure")
	}
	// The default limits' series and chapters: 50,000 series with 20
	// read, bookmarked chapters each, with long URLs.
	const series, perSeries = 50000, 20
	pad := strings.Repeat("x", 80)
	var b backuptest.Backup
	for i := range series {
		se := backuptest.Series{SourceID: int64(i % 50), URL: fmt.Sprintf("/series/%s/%d", pad, i), Title: fmt.Sprint("Series ", i), Author: "A. Author", DateAdded: added}
		for c := range perSeries {
			se.Chapters = append(se.Chapters, backuptest.Chapter{URL: fmt.Sprintf("/c/%s/%d/%d", pad, i, c), Number: float32(c + 1), Read: true, Bookmark: true, DateFetch: fetched})
		}
		b.Series = append(b.Series, se)
	}
	file := b.File()
	b = backuptest.Backup{}
	t.Logf("file: %.1f MiB", float64(len(file))/(1<<20))

	readPeak := measure(t, func() {
		m, err := mihonbackup.Read(bytes.NewReader(file), mihonbackup.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(m)
	})
	t.Logf("Read alone: peak heap %d MiB above before", readPeak>>20)

	st := storetest.New(t)
	s := NewService(st, nil, nil, nil, queue.New(st, nil, nil, nil, nil), nil, Options{TempDir: t.TempDir(), Gate: NewReadGate()}, nil)
	user := newUser(t, st, "big")
	start := time.Now()
	peak := measure(t, func() {
		if _, _, err := s.Upload(context.Background(), viewerOf(user), bytes.NewReader(file)); err != nil {
			t.Fatal(err)
		}
	})
	took := time.Since(start)
	t.Logf("Upload (read, rows, stored) took %s: peak heap %d MiB above before", took.Round(100*time.Millisecond), peak>>20)
	// Storing the rows adds next to nothing to the read's own peak: the
	// model shrinks as the rows go out.
	if peak > readPeak+32<<20 {
		t.Errorf("the upload peaked %d MiB above the read", (peak-readPeak)>>20)
	}
}

// measure is the peak heap while f runs, above the heap before it.
func measure(t *testing.T, f func()) uint64 {
	t.Helper()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-sampled
	return peak.Load() - min(base.HeapAlloc, peak.Load())
}
