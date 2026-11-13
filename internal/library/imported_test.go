// SPDX-License-Identifier: GPL-3.0-or-later

package library_test

import (
	"context"
	"testing"

	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/store/db"
)

// TestAttachImported: an import attaches its series only where the work
// has none on the source, never replaces one (works are shared) and never
// brings back a series someone rejected.
func TestAttachImported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	added, err := f.lib.Add(ctx, f.user, "e1", &library.SourceRef{SourceID: "fake:1001", Key: "/series/1"})
	if err != nil {
		t.Fatal(err)
	}
	w := added.Work.ID
	waitMatched(t, f, w)
	series := func(key string) db.SourceSeries {
		t.Helper()
		se, err := f.st.GetSourceSeriesByKey(ctx, db.GetSourceSeriesByKeyParams{WorkID: w, SourceID: "fake:1001", Key: key})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return se
	}
	attach := func(key, want string) {
		t.Helper()
		got, err := f.lib.AttachImported(ctx, f.user, w, "fake:1001", key)
		if err != nil || got != want {
			t.Fatalf("attach %s: %q %v, want %q", key, got, err, want)
		}
	}
	// The same series: nothing to do.
	attach("/series/1", library.ImportAttachedBefore)
	// Another series: the attached one stays, the import's is a candidate.
	attach("/series/3", library.ImportKeptOther)
	if se := series("/series/1"); se.State != library.StateAttached || se.Origin != library.OriginManual {
		t.Fatalf("the attached series changed: %+v", se)
	}
	if se := series("/series/3"); se.State != library.StateCandidate || se.Origin != library.OriginImport {
		t.Fatalf("the imported series: %+v", se)
	}
	// Rejected by someone: not attached again.
	if _, err := f.lib.Detach(ctx, f.user, w, series("/series/1").ID); err != nil {
		t.Fatal(err)
	}
	attach("/series/1", library.ImportRejected)
	if se := series("/series/1"); se.State != library.StateRejected {
		t.Fatalf("a rejected series came back: %+v", se)
	}
	// Nothing attached on the source: the import attaches its series.
	attach("/series/3", library.ImportAttached)
	if se := series("/series/3"); se.State != library.StateAttached || se.Origin != library.OriginImport {
		t.Fatalf("attached: %+v", se)
	}
	// A series the source doesn't have isn't attached.
	got, err := f.lib.AttachImported(ctx, f.user, w, "fake:2001", "/no/such/series")
	if err != nil || got != library.ImportUnavailable {
		t.Fatalf("unknown series: %q %v", got, err)
	}
}
