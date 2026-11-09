// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
	"github.com/panelarr/server/internal/users"
)

// guesser has no source that resolves URLs natively, but finds a series
// for any URL by searching (best effort), like a plugin that searches the
// site for a pasted URL.
type guesser struct{ *plugintest.Fake }

func (g guesser) ResolveUrl(_ context.Context, r *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	if r.GetUrl() == "https://nowhere.example/x" {
		return nil, plugin.ErrNotFound("nothing found")
	}
	return &sourcev1.ResolveUrlResponse{SourceId: "1001", Series: g.Sources[0].Series[1].Series, BestEffort: true}, nil
}

// noResolver doesn't implement ResolveUrl at all.
type noResolver struct{ *plugintest.Fake }

func (noResolver) ResolveUrl(context.Context, *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	return nil, plugin.ErrUnimplemented("ResolveUrl")
}

func withoutResolveURL(f *plugintest.Fake) *plugintest.Fake {
	for _, s := range f.Sources {
		s.Source.Capabilities = slices.DeleteFunc(s.Source.Capabilities, func(c sourcev1.SourceCapability) bool {
			return c == sourcev1.SourceCapability_SOURCE_CAPABILITY_RESOLVE_URL
		})
	}
	return f
}

func TestResolveURLAsksEveryPlugin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	// Registered before the native resolver, to show native ones go first.
	for id, p := range map[string]plugin.Plugin{
		"guess":  guesser{withoutResolveURL(plugintest.New())},
		"noimpl": noResolver{withoutResolveURL(plugintest.New())},
	} {
		if _, err := h.Register(ctx, plugins.NewPlugin{ID: id, URL: pluginstest.Serve(t, p, key), APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Register(ctx, plugins.NewPlugin{ID: "native", URL: pluginstest.Serve(t, plugintest.New(), key), APIKey: key}); err != nil {
		t.Fatal(err)
	}
	v := plugins.Viewer{Grants: users.DefaultGrants()}

	// A URL a native resolver knows: its certain answer wins over a guess.
	r, err := h.ResolveURL(ctx, v, "https://example-comics.example/series/1")
	if err != nil || r.Source.ID != "native:1001" || r.BestEffort || r.Series.GetTitle() != "The Lantern Keeper" {
		t.Fatalf("native: %v %+v", err, r)
	}
	// No native resolver knows it, but a plugin without one finds it by
	// searching; a plugin that doesn't implement ResolveUrl is skipped.
	r, err = h.ResolveURL(ctx, v, "https://example-comics.example/?search=salt")
	if err != nil || r.Source.ID != "guess:1001" || !r.BestEffort || r.Series.GetTitle() != "Salt and Iron" {
		t.Fatalf("best effort: %v %+v", err, r)
	}
	if _, err := h.ResolveURL(ctx, v, "https://nowhere.example/x"); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("unknown url: %v", err)
	}
}
