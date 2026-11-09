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
	if r.GetUrl() == nowhere {
		return nil, plugin.ErrNotFound("nothing found")
	}
	return &sourcev1.ResolveUrlResponse{SourceId: "1001", Series: g.Sources[0].Series[1].Series, BestEffort: true}, nil
}

// knower has no native resolver either, but knows some URLs for certain.
type knower struct {
	*plugintest.Fake
	urls []string
}

func (k knower) ResolveUrl(_ context.Context, r *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	if !slices.Contains(k.urls, r.GetUrl()) {
		return nil, plugin.ErrNotFound("unknown url")
	}
	return &sourcev1.ResolveUrlResponse{SourceId: "1001", Series: k.Sources[0].Series[1].Series}, nil
}

// noResolver doesn't implement ResolveUrl at all.
type noResolver struct{ *plugintest.Fake }

func (noResolver) ResolveUrl(context.Context, *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	return nil, plugin.ErrUnimplemented("ResolveUrl")
}

// broken fails every ResolveUrl call with an ordinary error.
type broken struct{ *plugintest.Fake }

func (broken) ResolveUrl(context.Context, *sourcev1.ResolveUrlRequest) (*sourcev1.ResolveUrlResponse, error) { //nolint:revive // named after the RPC
	return nil, plugin.ErrSourceDown("the site is down")
}

func withoutResolveURL(f *plugintest.Fake) *plugintest.Fake {
	for _, s := range f.Sources {
		s.Source.Capabilities = slices.DeleteFunc(s.Source.Capabilities, func(c sourcev1.SourceCapability) bool {
			return c == sourcev1.SourceCapability_SOURCE_CAPABILITY_RESOLVE_URL
		})
	}
	return f
}

const (
	nativeURL    = "https://example-comics.example/series/1" // the fake's native resolver knows it
	elsewhereURL = "https://elsewhere.example/s/1"
	nowhere      = "https://nowhere.example/x"
)

func TestResolveURLAsksEveryPlugin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	// Plugins are listed by ID, so all of these come before "native": the
	// order of asking has to come from ResolveURL itself.
	for id, p := range map[string]plugin.Plugin{
		"abroken": broken{withoutResolveURL(plugintest.New())},
		"guess":   guesser{withoutResolveURL(plugintest.New())},
		"knows":   knower{withoutResolveURL(plugintest.New()), []string{nativeURL, elsewhereURL}},
		"noimpl":  noResolver{withoutResolveURL(plugintest.New())},
		"native":  plugintest.New(),
	} {
		if _, err := h.Register(ctx, plugins.NewPlugin{ID: id, URL: pluginstest.Serve(t, p, key), APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	v := plugins.Viewer{Grants: users.DefaultGrants()}

	// A URL a native resolver knows: native resolvers are asked first, so
	// its answer wins although another plugin, listed earlier, knows the
	// URL for certain too. The failing plugin is skipped.
	r, err := h.ResolveURL(ctx, v, nativeURL)
	if err != nil || r.Source.ID != "native:1001" || r.BestEffort || r.Series.GetTitle() != "The Lantern Keeper" {
		t.Fatalf("native: %v %+v", err, r)
	}
	// No native resolver knows it, and a guess comes first: a plugin that
	// knows it for certain, asked later, still wins.
	r, err = h.ResolveURL(ctx, v, elsewhereURL)
	if err != nil || r.Source.ID != "knows:1001" || r.BestEffort {
		t.Fatalf("certain: %v %+v", err, r)
	}
	// Nobody knows it for certain: the guess is used, flagged.
	r, err = h.ResolveURL(ctx, v, "https://example-comics.example/?search=salt")
	if err != nil || r.Source.ID != "guess:1001" || !r.BestEffort || r.Series.GetTitle() != "Salt and Iron" {
		t.Fatalf("best effort: %v %+v", err, r)
	}
	if _, err := h.ResolveURL(ctx, v, nowhere); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("unknown url: %v", err)
	}
}
