// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/events"
)

const viewsPath = "/api/v1/me/library-views"

func viewPath(id uuid.UUID) string { return viewsPath + "/" + id.String() }

func listViews(c *client) []apigen.LibraryView {
	return call[apigen.LibraryViewList](c, http.MethodGet, viewsPath, nil, 200).Views
}

func createView(c *client, name string, def map[string]any) apigen.LibraryView {
	if def == nil {
		def = map[string]any{}
	}
	return call[apigen.LibraryView](c, http.MethodPost, viewsPath, map[string]any{"name": name, "definition": def}, 201)
}

// viewOrder is the names in order, the default marked with a star.
func viewOrder(t *testing.T, vs []apigen.LibraryView) string {
	t.Helper()
	names := make([]string, len(vs))
	defaults := 0
	for i, v := range vs {
		if v.Position != i {
			t.Fatalf("view %q at %d has position %d", v.Name, i, v.Position)
		}
		names[i] = v.Name
		if v.Default {
			names[i] += "*"
			defaults++
		}
	}
	if len(vs) > 0 && defaults != 1 {
		t.Fatalf("%d defaults: %v", defaults, names)
	}
	return strings.Join(names, ",")
}

func TestLibraryViews(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)

	if vs := listViews(user); len(vs) != 0 {
		t.Fatalf("before any %+v", vs)
	}

	// The first view is the default, whatever it asks.
	lib := createView(user, "Library", nil)
	if !lib.Default || lib.Position != 0 || lib.Name != "Library" || lib.CreatedAt.IsZero() {
		t.Fatalf("first view %+v", lib)
	}

	// A full definition comes back as sent.
	def := map[string]any{
		"filters":             map[string]any{"unread": "include", "started": "exclude", "downloaded": "include", "bookmarked": "exclude", "new": "include"},
		"types":               []string{"manhwa"},
		"statuses":            []string{"ongoing", "hiatus"},
		"updated_within_days": 7,
		"sort":                "updated",
		"reverse":             true,
		"display":             map[string]any{"layout": "list", "unread_badges": true, "download_badges": false, "continue_button": true},
	}
	manhwa := createView(user, "  Unread manhwa ", def)
	if manhwa.Default || manhwa.Position != 1 || manhwa.Name != "Unread manhwa" {
		t.Fatalf("second view %+v", manhwa)
	}
	sent, _ := json.Marshal(def)
	got, _ := json.Marshal(manhwa.Definition)
	var a, b any
	_ = json.Unmarshal(sent, &a)
	_ = json.Unmarshal(got, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("definition\n got %s\nwant %s", got, sent)
	}
	if v := call[apigen.LibraryView](user, http.MethodGet, viewPath(manhwa.Id), nil, 200); v.Name != manhwa.Name || *v.Definition.Sort != apigen.LibraryViewDefinitionSortUpdated {
		t.Fatalf("get %+v", v)
	}

	// Created as the default: the mark moves.
	latest := call[apigen.LibraryView](user, http.MethodPost, viewsPath, map[string]any{"name": "Latest", "definition": map[string]any{"sort": "added"}, "default": true}, 201)
	if !latest.Default {
		t.Fatalf("created as default %+v", latest)
	}
	if o := viewOrder(t, listViews(user)); o != "Library,Unread manhwa,Latest*" {
		t.Fatalf("order %s", o)
	}

	// Names are unique per user, ignoring case.
	user.expectProblem(http.MethodPost, viewsPath, map[string]any{"name": "LIBRARY", "definition": map[string]any{}}, 409, "name_taken")
	user.expectProblem(http.MethodPatch, viewPath(manhwa.Id), map[string]any{"name": "latest"}, 409, "name_taken")

	// Rename and replace the definition; the rest stays.
	v := call[apigen.LibraryView](user, http.MethodPatch, viewPath(manhwa.Id), map[string]any{"name": "Manhwa", "definition": map[string]any{"sort": "title"}}, 200)
	if v.Name != "Manhwa" || v.Definition.Sort == nil || *v.Definition.Sort != apigen.LibraryViewDefinitionSortTitle || v.Definition.Filters != nil || v.Default || !v.UpdatedAt.After(manhwa.UpdatedAt) {
		t.Fatalf("renamed %+v", v)
	}
	// Renaming to its own name in another case is fine.
	call[apigen.LibraryView](user, http.MethodPatch, viewPath(manhwa.Id), map[string]any{"name": "MANHWA"}, 200)

	// Default: true moves the mark; false on the default is refused, on
	// another view it changes nothing.
	call[apigen.LibraryView](user, http.MethodPatch, viewPath(lib.Id), map[string]any{"default": true}, 200)
	if o := viewOrder(t, listViews(user)); o != "Library*,MANHWA,Latest" {
		t.Fatalf("after default %s", o)
	}
	user.expectProblem(http.MethodPatch, viewPath(lib.Id), map[string]any{"default": false}, 409, "default_required")
	call[apigen.LibraryView](user, http.MethodPatch, viewPath(latest.Id), map[string]any{"default": false}, 200)
	if o := viewOrder(t, listViews(user)); o != "Library*,MANHWA,Latest" {
		t.Fatalf("after default false %s", o)
	}

	// Order: exactly the caller's views, each once.
	order := func(ids ...uuid.UUID) response {
		return user.do(http.MethodPut, viewsPath+"/order", map[string]any{"ids": ids})
	}
	if r := order(latest.Id, lib.Id, manhwa.Id); r.status != 200 {
		t.Fatalf("order %d %s", r.status, r.body)
	}
	if o := viewOrder(t, listViews(user)); o != "Latest,Library*,MANHWA" {
		t.Fatalf("after order %s", o)
	}
	for name, ids := range map[string][]uuid.UUID{
		"missing one": {latest.Id, lib.Id},
		"twice":       {latest.Id, lib.Id, lib.Id},
		"unknown":     {latest.Id, lib.Id, uuid.New()},
		"extra":       {latest.Id, lib.Id, manhwa.Id, uuid.New()},
	} {
		if r := order(ids...); r.status != 409 || r.problem(t).Code != "order_mismatch" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}

	// Deleting the default passes it to the first remaining view, and the
	// order closes up.
	call[struct{}](user, http.MethodDelete, viewPath(lib.Id), nil, 204)
	if o := viewOrder(t, listViews(user)); o != "Latest*,MANHWA" {
		t.Fatalf("after deleting the default %s", o)
	}
	user.expectProblem(http.MethodGet, viewPath(lib.Id), nil, 404, CodeNotFound)
	user.expectProblem(http.MethodDelete, viewPath(lib.Id), nil, 404, CodeNotFound)
	call[struct{}](user, http.MethodDelete, viewPath(manhwa.Id), nil, 204)
	if o := viewOrder(t, listViews(user)); o != "Latest*" {
		t.Fatalf("after deleting another %s", o)
	}
	// The last view stays.
	user.expectProblem(http.MethodDelete, viewPath(latest.Id), nil, 409, "last_view")

	// A new view goes to the end.
	if v := createView(user, "Library", nil); v.Position != 1 || v.Default {
		t.Fatalf("recreated %+v", v)
	}

	// A read-only key lists; changing needs write.
	readKey := h.keyClient(user.newAPIKey("read"))
	if vs := listViews(readKey); len(vs) != 2 {
		t.Fatalf("read key %+v", vs)
	}
	readKey.expectProblem(http.MethodPost, viewsPath, map[string]any{"name": "X", "definition": map[string]any{}}, 403, "insufficient_scope")
	writeKey := h.keyClient(user.newAPIKey("read", "write"))
	call[apigen.LibraryView](writeKey, http.MethodPatch, viewPath(latest.Id), map[string]any{"name": "Newest"}, 200)
}

func TestLibraryViewValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	first := createView(user, "First", nil)

	bad := func(name string, body map[string]any) {
		t.Helper()
		if r := user.do(http.MethodPost, viewsPath, body); r.status != 400 || r.problem(t).Code != CodeInvalidRequest {
			t.Fatalf("create, %s: %d %s", name, r.status, r.body)
		}
		patch := map[string]any{}
		for k, v := range body {
			patch[k] = v
		}
		if r := user.do(http.MethodPatch, viewPath(first.Id), patch); r.status != 400 || r.problem(t).Code != CodeInvalidRequest {
			t.Fatalf("change, %s: %d %s", name, r.status, r.body)
		}
	}
	withDef := func(def map[string]any) map[string]any { return map[string]any{"name": "View", "definition": def} }
	long := func(n int) string { return strings.Repeat("x", n) }
	many := func(n int, v string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = v + strings.Repeat("-", i)
		}
		return out
	}

	bad("unknown key", withDef(map[string]any{"colour": "red"}))
	bad("unknown filter", withDef(map[string]any{"filters": map[string]any{"finished": "include"}}))
	bad("unknown filter mode", withDef(map[string]any{"filters": map[string]any{"unread": "only"}}))
	bad("unknown sort", withDef(map[string]any{"sort": "rating"}))
	bad("unknown layout", withDef(map[string]any{"display": map[string]any{"layout": "grid"}}))
	bad("unknown display key", withDef(map[string]any{"display": map[string]any{"covers": true}}))
	bad("updated within 0", withDef(map[string]any{"updated_within_days": 0}))
	bad("updated within 366", withDef(map[string]any{"updated_within_days": 366}))
	bad("21 types", withDef(map[string]any{"types": many(21, "t")}))
	bad("long status", withDef(map[string]any{"statuses": []string{long(41)}}))
	bad("empty type", withDef(map[string]any{"types": []string{""}}))
	bad("empty name", map[string]any{"name": "", "definition": map[string]any{}})
	bad("blank name", map[string]any{"name": "   ", "definition": map[string]any{}})
	bad("61 characters", map[string]any{"name": long(61), "definition": map[string]any{}})
	// Within every per-field limit, but over 4 KiB in all.
	wide := make([]string, 20)
	for i := range wide {
		wide[i] = strings.Repeat("😀", 38) + fmt.Sprintf("%02d", i)
	}
	big := withDef(map[string]any{"types": wide, "statuses": wide})
	if r := user.do(http.MethodPost, viewsPath, big); r.status != 400 || (*r.problem(t).Errors)[0].Field != "definition" {
		t.Fatalf("over 4 KiB: %d %s", r.status, r.body)
	}
	if r := user.do(http.MethodPost, viewsPath, map[string]any{"name": "No definition"}); r.status != 400 {
		t.Fatalf("no definition: %d %s", r.status, r.body)
	}

	// The limits themselves are fine.
	createView(user, long(60), map[string]any{"types": many(20, long(20)), "updated_within_days": 365})
	createView(user, strings.Repeat("ž", 60), map[string]any{"updated_within_days": 1})

	// At most 50 views.
	for i := len(listViews(user)); i < 50; i++ {
		createView(user, "View "+uuid.NewString()[:8], nil)
	}
	user.expectProblem(http.MethodPost, viewsPath, map[string]any{"name": "One too many", "definition": map[string]any{}}, 409, "view_limit")
	if n := len(listViews(user)); n != 50 {
		t.Fatalf("%d views", n)
	}
}

// AC9: nobody else can see or change a user's views; they go with the user.
func TestLibraryViewsAreTheirOwners(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	alice, aliceUser := h.createUser(admin, "alice", nil)
	bob, bobUser := h.createUser(admin, "bob", nil)
	av := createView(alice, "Mine", nil)
	createView(alice, "Also mine", nil)
	bv := createView(bob, "Mine", nil) // the same name is fine for someone else

	for _, other := range []*client{bob, admin} {
		if vs := listViews(other); len(vs) > 1 || (len(vs) == 1 && vs[0].Id != bv.Id) {
			t.Fatalf("someone else's list %+v", vs)
		}
		other.expectProblem(http.MethodGet, viewPath(av.Id), nil, 404, CodeNotFound)
		other.expectProblem(http.MethodPatch, viewPath(av.Id), map[string]any{"name": "Taken"}, 404, CodeNotFound)
		other.expectProblem(http.MethodPatch, viewPath(av.Id), map[string]any{"default": true}, 404, CodeNotFound)
		other.expectProblem(http.MethodDelete, viewPath(av.Id), nil, 404, CodeNotFound)
		other.expectProblem(http.MethodPut, viewsPath+"/order", map[string]any{"ids": []uuid.UUID{av.Id}}, 409, "order_mismatch")
	}
	if v := call[apigen.LibraryView](alice, http.MethodGet, viewPath(av.Id), nil, 200); v.Name != "Mine" || !v.Default {
		t.Fatalf("after the others tried %+v", v)
	}
	if o := viewOrder(t, listViews(alice)); o != "Mine*,Also mine" {
		t.Fatalf("alice's views %s", o)
	}

	// Events reach only the owner's streams.
	bobSub := h.hub.Subscribe(events.Viewer{UserID: bobUser.Id})
	defer bobSub.Close()
	adminSub := h.hub.Subscribe(events.Viewer{UserID: uuid.New(), Admin: true})
	defer adminSub.Close()
	got := readEvents(t, alice, 2, func() {
		call[apigen.LibraryView](alice, http.MethodPatch, viewPath(av.Id), map[string]any{"name": "Renamed"}, 200)
	})
	if len(got) < 2 || got[1] != "library_views.changed" {
		t.Fatalf("owner's events %v", got)
	}
	for name, sub := range map[string]*events.Subscription{"bob": bobSub, "admin": adminSub} {
		for len(sub.C) > 0 {
			if e := <-sub.C; e.Type == "library_views.changed" {
				t.Fatalf("%s heard %s", name, e.Type)
			}
		}
	}
	// Every kind of change is announced.
	for name, change := range map[string]func(){
		"create": func() { createView(alice, "Third", nil) },
		"order": func() {
			call[apigen.LibraryViewList](alice, http.MethodPut, viewsPath+"/order", map[string]any{"ids": idsOf(listViews(alice), true)}, 200)
		},
		"delete": func() { call[struct{}](alice, http.MethodDelete, viewPath(av.Id), nil, 204) },
	} {
		if got := readEvents(t, alice, 2, change); len(got) < 2 || got[1] != "library_views.changed" {
			t.Fatalf("%s: events %v", name, got)
		}
	}

	// Deleting a user deletes their views.
	count := func(id uuid.UUID) int {
		var n int
		if err := h.st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM library_views WHERE user_id = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(aliceUser.Id) == 0 {
		t.Fatal("alice has no views")
	}
	call[struct{}](admin, http.MethodDelete, "/api/v1/users/"+aliceUser.Id.String(), nil, 204)
	if n := count(aliceUser.Id); n != 0 {
		t.Fatalf("%d views left after deleting the user", n)
	}
	if n := count(bobUser.Id); n != 1 {
		t.Fatalf("bob has %d views", n)
	}
}

// idsOf lists views' IDs, reversed if asked.
func idsOf(vs []apigen.LibraryView, reverse bool) []uuid.UUID {
	out := make([]uuid.UUID, len(vs))
	for i, v := range vs {
		if reverse {
			out[len(vs)-1-i] = v.Id
		} else {
			out[i] = v.Id
		}
	}
	return out
}

// Devices creating views at once (e.g. each making the first "Library"
// view): one default, a gap-free order, and one view per name.
func TestLibraryViewsConcurrently(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)

	const n = 8
	statuses := make(chan int, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			statuses <- user.do(http.MethodPost, viewsPath, map[string]any{"name": "Library", "definition": map[string]any{}}).status
		})
		wg.Go(func() {
			statuses <- user.do(http.MethodPost, viewsPath, map[string]any{"name": fmt.Sprintf("View %d", i), "definition": map[string]any{}, "default": i%2 == 0}).status
		})
	}
	wg.Wait()
	close(statuses)
	count := map[int]int{}
	for s := range statuses {
		count[s]++
	}
	if count[201] != n+1 || count[409] != n-1 {
		t.Fatalf("statuses %v", count)
	}
	if vs := listViews(user); len(vs) != n+1 {
		t.Fatalf("%d views", len(vs))
	} else {
		viewOrder(t, vs) // one default, positions 0..n
	}
}
