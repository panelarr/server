// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"
	"github.com/panelarr/source-api/sdk/settings"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/storetest"
	"github.com/panelarr/server/internal/users"
)

const key = "plugin-key"

type env struct {
	st  *store.Store
	box *secret.Box
}

func newHost(t *testing.T) (*plugins.Host, env) {
	t.Helper()
	e := env{st: storetest.New(t), box: secret.NewBox("", filepath.Join(t.TempDir(), "secret.key"))}
	return plugins.NewHost(e.st, e.box, plugins.RemoteDialer, nil), e
}

func admin() plugins.Viewer { return plugins.Viewer{Admin: true} }

func TestRegisterAndSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, e := newHost(t)
	st := e.st
	fake := plugintest.New()
	u := pluginstest.Serve(t, fake, key)

	// Wrong key and wrong address are refused, with a reason.
	var unreachable *plugins.UnreachableError
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: u, APIKey: "wrong"}); !errors.As(err, &unreachable) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: "http://127.0.0.1:1"}); !errors.As(err, &unreachable) {
		t.Fatalf("no plugin: %v", err)
	}
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: "ftp://x"}); !errors.Is(err, plugins.ErrInvalidURL) {
		t.Fatalf("bad url: %v", err)
	}

	p, err := h.Register(ctx, plugins.NewPlugin{URL: u + "/", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "fake" || p.Health != "ok" || p.SourceCount != 2 || p.Info.GetId() != "fake" || p.Url != u {
		t.Fatalf("registered %+v", p)
	}
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: u, APIKey: key}); !errors.Is(err, plugins.ErrIDTaken) {
		t.Fatalf("same id: %v", err)
	}
	second, err := h.Register(ctx, plugins.NewPlugin{ID: "fake-2", URL: u, APIKey: key})
	if err != nil || second.SourceCount != 2 {
		t.Fatalf("second instance: %v %+v", err, second)
	}
	// The key is stored encrypted.
	row, _ := st.GetPlugin(ctx, "fake")
	if row.ApiKey == key || !strings.HasPrefix(row.ApiKey, "enc:") {
		t.Fatalf("stored key %q", row.ApiKey)
	}

	srcs, err := h.Sources(ctx, admin(), false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]plugins.Source{}
	for _, s := range srcs {
		byID[s.ID] = s
	}
	ex := byID["fake:1001"]
	if len(srcs) != 4 || ex.Name != "Example Comics" || ex.ContentRating != "safe" || ex.SourceID != "1001" ||
		!strings.Contains(strings.Join(ex.Capabilities, ","), "latest") {
		t.Fatalf("sources %+v", srcs)
	}

	// A source the plugin stops listing stays, marked absent.
	fake.Sources = fake.Sources[:1]
	if p, err = h.Sync(ctx, "fake"); err != nil || p.SourceCount != 1 {
		t.Fatalf("resync: %v %+v", err, p)
	}
	all, _ := h.Sources(ctx, admin(), true)
	var absent int
	for _, s := range all {
		if !s.Present {
			absent++
		}
	}
	if absent != 1 {
		t.Fatalf("%d absent", absent)
	}

	// Disabled plugins hide their sources and refuse calls.
	off := false
	if _, err := h.Update(ctx, "fake-2", plugins.Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Popular(ctx, admin(), "fake-2:1001", 1); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("disabled plugin: %v", err)
	}
	if err := h.Delete(ctx, "fake-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Get(ctx, "fake-2"); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestDownPluginsAndReadiness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	u := pluginstest.Serve(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: u, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := h.ReadyCheck(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	// Point it somewhere dead without checking (as after a crash): the
	// next sync records it down.
	bad := "http://127.0.0.1:1"
	on := false
	if _, err := h.Update(ctx, "fake", plugins.Patch{URL: &bad, Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	on = true
	if _, err := h.Update(ctx, "fake", plugins.Patch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	p, err := h.Sync(ctx, "fake")
	if err != nil || p.Health != "down" || p.HealthMessage == "" {
		t.Fatalf("down: %v %+v", err, p)
	}
	if err := h.ReadyCheck(ctx); err == nil || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("readiness: %v", err)
	}
	// Its sources are still known.
	if srcs, _ := h.Sources(ctx, admin(), false); len(srcs) != 2 {
		t.Fatalf("sources kept: %d", len(srcs))
	}
}

func TestConfigKeepsSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schema, err := settings.Parse(`{"type":"object","properties":{
		"endpoint":{"type":"string","default":"http://backend"},
		"token":{"type":"string","writeOnly":true}
	}}`)
	if err != nil {
		t.Fatal(err)
	}
	var applied []map[string]any
	fake := plugintest.New()
	fake.Config = plugin.NewPluginConfig(schema, func(_ context.Context, v map[string]any) error {
		applied = append(applied, v)
		return nil
	})
	u := pluginstest.Serve(t, fake, key)
	h, e := newHost(t)
	st := e.st
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: u, APIKey: key}); err != nil {
		t.Fatal(err)
	}

	c, err := h.SetConfig(ctx, "fake", map[string]any{"endpoint": "http://other", "token": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Values["token"] != "" || c.Values["endpoint"] != "http://other" {
		t.Fatalf("shown %v", c.Values)
	}
	row, _ := st.GetPlugin(ctx, "fake")
	if strings.Contains(row.Config, "s3cret") {
		t.Fatal("config stored in the clear")
	}
	// Saving again without the secret keeps it.
	if _, err := h.SetConfig(ctx, "fake", map[string]any{"endpoint": "http://third", "token": ""}); err != nil {
		t.Fatal(err)
	}
	last := applied[len(applied)-1]
	if last["token"] != "s3cret" || last["endpoint"] != "http://third" {
		t.Fatalf("applied %v", last)
	}
	got, err := h.Config(ctx, "fake")
	if err != nil || got.Schema == nil || got.Values["endpoint"] != "http://third" {
		t.Fatalf("config %v %+v", err, got)
	}

	// A restarted server hands the stored config back.
	before := len(applied)
	restarted := plugins.NewHost(st, e.box, plugins.RemoteDialer, nil)
	if err := restarted.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if len(applied) != before+1 || applied[len(applied)-1]["token"] != "s3cret" {
		t.Fatalf("not re-applied on load: %d → %d", before, len(applied))
	}

	// Plugins without settings say so.
	plain := pluginstest.Serve(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{ID: "plain", URL: plain, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Config(ctx, "plain"); !errors.Is(err, plugins.ErrNoConfig) {
		t.Fatalf("no config: %v", err)
	}
}

func TestVisibility(t *testing.T) {
	src := func(rating string) plugins.Source {
		var s plugins.Source
		s.ID, s.ContentRating, s.Present, s.Enabled, s.PluginEnabled = "p:1", rating, true, true, true
		return s
	}
	user := plugins.Viewer{Grants: users.DefaultGrants()} // suggestive, all sources
	for _, tc := range []struct {
		v    plugins.Viewer
		s    plugins.Source
		want bool
	}{
		{user, src("safe"), true},
		{user, src("mixed"), true},
		{user, src("nsfw"), false},
		{user, src("unknown"), true},
		{plugins.Viewer{Grants: users.DefaultGrants(), HideUnknown: true}, src("unknown"), false},
		{plugins.Viewer{Grants: users.Grants{ContentRatingCeiling: "safe", AllowedSources: []string{"p:1"}}}, src("safe"), true},
		{plugins.Viewer{Grants: users.Grants{ContentRatingCeiling: "safe", AllowedSources: []string{"p:2"}}}, src("safe"), false},
		{plugins.Viewer{Grants: users.Grants{ContentRatingCeiling: "safe", AllSources: true}}, src("mixed"), false},
		{plugins.Viewer{Admin: true}, src("nsfw"), true},
	} {
		if got := tc.v.Visible(tc.s); got != tc.want {
			t.Errorf("%+v sees %s: %v", tc.v, tc.s.ContentRating, got)
		}
	}
	off := src("safe")
	off.Enabled = false
	if (plugins.Viewer{Admin: true}).Visible(off) {
		t.Error("disabled source usable")
	}
}

func TestBrowse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	u := pluginstest.Serve(t, plugintest.New(), key)
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: u, APIKey: key}); err != nil {
		t.Fatal(err)
	}
	v := plugins.Viewer{Grants: users.DefaultGrants()}

	pop, err := h.Popular(ctx, v, "fake:1001", 1)
	if err != nil || len(pop.Series) == 0 {
		t.Fatalf("popular %v %+v", err, pop)
	}
	if _, err := h.Latest(ctx, v, "fake:2001", 1); !errors.Is(err, plugins.ErrUnsupported) {
		t.Fatalf("latest without capability: %v", err)
	}
	res, err := h.Search(ctx, v, "fake:1001", "lantern", 1, nil)
	if err != nil || len(res.Series) != 1 || res.Series[0].GetTitle() != "The Lantern Keeper" {
		t.Fatalf("search %v %+v", err, res)
	}
	se, err := h.Series(ctx, v, "fake:1001", res.Series[0].GetKey())
	if err != nil || len(se.GetAltTitles()) != 2 {
		t.Fatalf("series %v %+v", err, se)
	}
	chs, err := h.Chapters(ctx, v, "fake:1001", se.GetKey())
	if err != nil || len(chs) != 3 {
		t.Fatalf("chapters %v %d", err, len(chs))
	}
	ct, data, err := h.Image(ctx, v, "fake:1001", se.GetCoverUrl())
	if err != nil || ct != "image/png" || len(data) == 0 {
		t.Fatalf("image %v %q %d", err, ct, len(data))
	}
	f, err := h.Filters(ctx, v, "fake:1001")
	if err != nil || len(f.GetFilters()) == 0 {
		t.Fatalf("filters %v", err)
	}
	if _, err := h.Popular(ctx, v, "fake:404", 1); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("unknown source: %v", err)
	}
}
