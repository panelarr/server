// SPDX-License-Identifier: GPL-3.0-or-later

package meta_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/meta/metatest"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store/storetest"
)

func newService(t *testing.T, env meta.Conn) *meta.Service {
	t.Helper()
	st := storetest.New(t)
	box := secret.NewBox("", filepath.Join(t.TempDir(), "secret.key"))
	return meta.NewService(st, box, env, nil)
}

func TestCheckSteps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := metatest.New(t)
	fake.AddKey("reader", "catalog:read")
	s := newService(t, meta.Conn{})

	c := s.Check(ctx, true)
	if c.Configured || c.Ready || !strings.Contains(c.Problem, "Connect") {
		t.Fatalf("unconfigured: %+v", c)
	}
	if _, err := s.Client(); !errors.Is(err, meta.ErrNotConfigured) {
		t.Fatalf("client: %v", err)
	}

	// Unreachable, bad key and missing admin scope are refused and not saved.
	for _, tc := range []struct {
		conn meta.Conn
		want string
	}{
		{meta.Conn{URL: "http://127.0.0.1:1", APIKey: metatest.AdminKey}, "Can't reach"},
		{meta.Conn{URL: fake.URL, APIKey: "wrong"}, "refused the API key"},
		{meta.Conn{URL: fake.URL, APIKey: "reader"}, "admin scope"},
	} {
		c, err := s.Save(ctx, tc.conn)
		if !errors.Is(err, meta.ErrCheckFailed) || !strings.Contains(c.Problem, tc.want) {
			t.Fatalf("%+v: %v %+v", tc.conn, err, c)
		}
	}
	if _, err := s.Client(); err == nil {
		t.Fatal("a failed connection was kept")
	}
	if _, err := s.Save(ctx, meta.Conn{URL: "ftp://meta", APIKey: "x"}); !errors.Is(err, meta.ErrInvalidURL) {
		t.Fatalf("bad URL: %v", err)
	}

	// Reachable with an admin key but no providers: saved, not ready.
	c, err := s.Save(ctx, meta.Conn{URL: fake.URL + "/", APIKey: metatest.AdminKey})
	if err != nil || c.Ready || !c.AdminScope || !strings.Contains(c.Problem, "no metadata provider") {
		t.Fatalf("no providers: %v %+v", err, c)
	}
	if err := s.ReadyCheck(ctx); err == nil {
		t.Fatal("ready without providers")
	}
	fake.SetProviders(map[string]int64{"books": 0})
	if c := s.Check(ctx, true); c.Ready || !strings.Contains(c.Problem, "Run a sync") {
		t.Fatalf("empty provider: %+v", c)
	}
	if id, err := s.RunSync(ctx, "books", "dump"); err != nil || id == "" || len(fake.Syncs()) != 1 {
		t.Fatalf("sync: %q %v", id, err)
	}
	fake.SetProviders(map[string]int64{"books": 42})
	if c := s.Check(ctx, true); !c.Ready || c.Problem != "" || len(c.Providers) != 1 || c.Providers[0].Records != 42 {
		t.Fatalf("ready: %+v", c)
	}

	// The cached check is reused until asked for a fresh one.
	fake.SetDown(true)
	if !s.Check(ctx, false).Ready {
		t.Fatal("cached check not reused")
	}
	if c := s.Check(ctx, true); c.Ready || c.Reachable {
		t.Fatalf("down: %+v", c)
	}
}

func TestConnectionPersistsEncrypted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 1})
	st := storetest.New(t)
	box := secret.NewBox("", filepath.Join(t.TempDir(), "secret.key"))
	s := meta.NewService(st, box, meta.Conn{}, nil)
	if _, err := s.Save(ctx, meta.Conn{URL: fake.URL, APIKey: metatest.AdminKey}); err != nil {
		t.Fatal(err)
	}
	raw, err := st.GetSetting(ctx, "meta.connection")
	if err != nil || strings.Contains(string(raw), metatest.AdminKey) {
		t.Fatalf("stored in the clear: %s %v", raw, err)
	}

	// A restart loads it.
	s2 := meta.NewService(st, box, meta.Conn{}, nil)
	if err := s2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !s2.Check(ctx, true).Ready {
		t.Fatal("not ready after reload")
	}
	// Changing only the URL keeps the key.
	if _, err := s2.Save(ctx, meta.Conn{URL: fake.URL}); err != nil {
		t.Fatalf("keep key: %v", err)
	}
}

func TestEnvConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 1})
	s := newService(t, meta.Conn{URL: fake.URL, APIKey: metatest.AdminKey})
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.Status(ctx)
	if !st.ManagedByEnv || !st.Check.Ready {
		t.Fatalf("status %+v", st)
	}
	if _, err := s.Save(ctx, meta.Conn{URL: fake.URL, APIKey: "x"}); !errors.Is(err, meta.ErrManagedByEnv) {
		t.Fatalf("save over env: %v", err)
	}
}

func entry(id, title string, rating metadatav1.ContentRating) *metav1.Entry {
	return &metav1.Entry{
		Id: id, Title: title, ContentRating: rating, Type: metadatav1.EntryType_ENTRY_TYPE_MANGA,
		Titles: []*metadatav1.Title{{Text: title, Language: "en", IsPrimary: true}},
		Images: []*metadatav1.Image{{Kind: metadatav1.ImageKind_IMAGE_KIND_COVER, Url: "http://covers.test/" + id + ".jpg"}},
	}
}

func TestCatalogRatings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 3})
	fake.AddEntry(entry("e1", "Gentle Story", metadatav1.ContentRating_CONTENT_RATING_SAFE))
	fake.AddEntry(entry("e2", "Spicy Story", metadatav1.ContentRating_CONTENT_RATING_EROTIC))
	fake.AddEntry(entry("e3", "Unrated Story", metadatav1.ContentRating_CONTENT_RATING_UNSPECIFIED))
	s := newService(t, meta.Conn{URL: fake.URL, APIKey: metatest.AdminKey})
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	user := meta.Viewer{Ceiling: "suggestive"}

	res, err := s.Search(ctx, meta.Query{Text: "story"}, user)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range res.GetHits() {
		ids = append(ids, h.GetEntry().GetId())
	}
	if strings.Join(ids, ",") != "e1,e3" {
		t.Fatalf("hits %v", ids)
	}
	if _, err := s.GetEntry(ctx, "e2", "", user); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("above the ceiling: %v", err)
	}
	if _, err := s.CoverURL(ctx, "e2", user); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("cover above the ceiling: %v", err)
	}
	if u, err := s.CoverURL(ctx, "e1", user); err != nil || u != "http://covers.test/e1.jpg" {
		t.Fatalf("cover %q %v", u, err)
	}
	if _, err := s.GetEntry(ctx, "missing", "", user); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if e, err := s.GetEntry(ctx, "e2", "", meta.Viewer{Ceiling: "pornographic"}); err != nil || e.GetId() != "e2" {
		t.Fatalf("within the ceiling: %v", err)
	}

	// Bad filters are reported, not passed on.
	var bad *meta.BadFilterError
	if _, err := s.Browse(ctx, meta.Query{Types: []string{"scroll"}}, user); !errors.As(err, &bad) || bad.Field != "type" {
		t.Fatalf("bad type: %v", err)
	}
	if _, err := s.Browse(ctx, meta.Query{Sort: "unspecified"}, user); !errors.As(err, &bad) {
		t.Fatalf("bad sort: %v", err)
	}
	b, err := s.Browse(ctx, meta.Query{Types: []string{"manga"}, Sort: "popularity"}, user)
	if err != nil || len(b.GetEntries()) != 2 {
		t.Fatalf("browse: %v %d", err, len(b.GetEntries()))
	}
	if f := fake.LastFilter(); len(f.GetTypes()) != 1 || f.GetTypes()[0] != metadatav1.EntryType_ENTRY_TYPE_MANGA {
		t.Fatalf("filter %v", f)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[metadatav1.ContentRating]string{
		metadatav1.ContentRating_CONTENT_RATING_SAFE:         "safe",
		metadatav1.ContentRating_CONTENT_RATING_PORNOGRAPHIC: "pornographic",
		metadatav1.ContentRating_CONTENT_RATING_UNKNOWN:      "unknown",
		metadatav1.ContentRating_CONTENT_RATING_UNSPECIFIED:  "unknown",
	} {
		if got := meta.RatingName(in); got != want {
			t.Errorf("%v: %q", in, got)
		}
	}
	if meta.TypeName(metadatav1.EntryType_ENTRY_TYPE_LIGHT_NOVEL) != "light_novel" || meta.TypeName(0) != "" {
		t.Fatal("type names")
	}
	if meta.StatusName(metadatav1.EntryStatus_ENTRY_STATUS_RELEASING) != "releasing" {
		t.Fatal("status names")
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 1, "comics": 1})
	s := newService(t, meta.Conn{URL: fake.URL, APIKey: metatest.AdminKey})
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}

	// The service answers that a provider's plugin is down: not "meta down".
	fake.SetProviderDown("books", false)
	_, err := s.RunSync(ctx, "books", "dump")
	var pu *meta.ProviderUnavailableError
	if !errors.As(err, &pu) || pu.Provider != "books" || meta.Unreachable(err) {
		t.Fatalf("implicit: %v", err)
	}
	fake.SetProviderDown("comics", true)
	if _, err := s.RunSync(ctx, "comics", "dump"); !errors.As(err, &pu) || pu.Provider != "comics" {
		t.Fatalf("explicit: %v", err)
	}

	// Other answered errors stay what they are.
	fake.FailWith(connect.CodeInternal)
	_, err = s.Search(ctx, meta.Query{Text: "x"}, meta.Viewer{Admin: true})
	if err == nil || meta.Unreachable(err) || errors.As(err, &pu) {
		t.Fatalf("internal: %v", err)
	}
	fake.FailWith(0)

	// No answer at all.
	fake.SetDown(true)
	if _, err := s.RunSync(ctx, "books", "dump"); !meta.Unreachable(err) || errors.As(err, &pu) {
		t.Fatalf("down: %v", err)
	}
	if c := s.Check(ctx, true); c.Reachable || !strings.Contains(c.Problem, "Can't reach") {
		t.Fatalf("check while down: %+v", c)
	}
}
