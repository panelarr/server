// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"slices"
	"testing"

	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
	"github.com/panelarr/server/internal/users"
)

func TestSyncStoresCompatIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	fake := plugintest.New()
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: pluginstest.Serve(t, fake, key), APIKey: key}); err != nil {
		t.Fatal(err)
	}
	compat := func() []string {
		t.Helper()
		s, err := h.Source(ctx, "fake:1001", admin())
		if err != nil {
			t.Fatal(err)
		}
		return s.CompatIds
	}
	if got := compat(); !slices.Equal(got, []string{"example-app:1001"}) {
		t.Fatalf("stored %q", got)
	}

	// Changed IDs replace the stored ones on the next sync; malformed ones
	// and repeats are dropped.
	fake.Sources[0].Source.CompatIds = []string{"other-app:x/1", "example-app:1001", "No Scheme", "other-app:x/1", ":1", "x:"}
	if _, err := h.Sync(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if got := compat(); !slices.Equal(got, []string{"other-app:x/1", "example-app:1001"}) {
		t.Fatalf("updated %q", got)
	}

	// None left: an empty list, not NULL (the sync would fail on it).
	fake.Sources[0].Source.CompatIds = nil
	if _, err := h.Sync(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if got := compat(); got == nil || len(got) != 0 {
		t.Fatalf("cleared %#v", got)
	}
	srcs, err := h.SourcesByCompatID(ctx, admin(), "example-app:1001")
	if err != nil || len(srcs) != 0 {
		t.Fatalf("lookup after clearing: %v %+v", err, srcs)
	}
}

func TestSourcesByCompatIDPrefersUsableThenOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	// Registered oldest first, against the ID order, so the order can only
	// come from when the plugins were added.
	for _, id := range []string{"zeta", "mid", "alpha"} {
		if _, err := h.Register(ctx, plugins.NewPlugin{ID: id, URL: pluginstest.Serve(t, plugintest.New(), key), APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(v plugins.Viewer, compatID string) []string {
		t.Helper()
		srcs, err := h.SourcesByCompatID(ctx, v, compatID)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(srcs))
		for _, s := range srcs {
			out = append(out, s.ID)
		}
		return out
	}
	everyone := plugins.Viewer{Grants: users.DefaultGrants()}

	// All usable: oldest plugin first.
	if got := ids(everyone, "example-app:1001"); !slices.Equal(got, []string{"zeta:1001", "mid:1001", "alpha:1001"}) {
		t.Fatalf("oldest first %v", got)
	}
	// A user allowed only some sources gets those first, the others after
	// (still oldest first), so a caller can say "not allowed".
	limited := plugins.Viewer{Grants: users.DefaultGrants()}
	limited.Grants.AllSources = false
	limited.Grants.AllowedSources = []string{"alpha:1001", "mid:1001"}
	if got := ids(limited, "example-app:1001"); !slices.Equal(got, []string{"mid:1001", "alpha:1001", "zeta:1001"}) {
		t.Fatalf("allowed first %v", got)
	}
	// A source turned off isn't usable: it goes last, for everyone.
	if _, err := h.SetSourceEnabled(ctx, "zeta:1001", false); err != nil {
		t.Fatal(err)
	}
	if got := ids(everyone, "example-app:1001"); !slices.Equal(got, []string{"mid:1001", "alpha:1001", "zeta:1001"}) {
		t.Fatalf("usable first %v", got)
	}
	// Only exact IDs match; an unknown one finds nothing.
	if got := ids(everyone, "example-app:2001"); !slices.Equal(got, []string{"zeta:2001", "mid:2001", "alpha:2001"}) {
		t.Fatalf("other source %v", got)
	}
	if got := ids(everyone, "example-app:100"); len(got) != 0 {
		t.Fatalf("unknown %v", got)
	}
}
