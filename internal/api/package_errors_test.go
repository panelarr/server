// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// iconFake links each package to an icon: a path on the plugin's own
// address, or (one package) an absolute URL elsewhere.
type iconFake struct {
	*plugintest.Fake
	elsewhere string
}

func (f *iconFake) withIcon(p *sourcev1.Package) {
	switch p.GetId() {
	case "beispiel.comics":
		p.IconUrl = f.elsewhere + "/b.svg"
	default:
		p.IconUrl = "/icons/" + p.GetId() + ".png"
	}
}

func (f *iconFake) ListPackages(ctx context.Context, r *sourcev1.ListPackagesRequest) (*sourcev1.ListPackagesResponse, error) {
	res, err := f.Fake.ListPackages(ctx, r)
	for _, p := range res.GetPackages() {
		f.withIcon(p)
	}
	return res, err
}

func (f *iconFake) GetPackage(ctx context.Context, r *sourcev1.GetPackageRequest) (*sourcev1.GetPackageResponse, error) {
	res, err := f.Fake.GetPackage(ctx, r)
	if res.GetPackage() != nil {
		f.withIcon(res.GetPackage())
	}
	return res, err
}

func TestPackageIcons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)

	var elsewhereAuth atomic.Value
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte("<svg/>"))
	}))
	t.Cleanup(elsewhere.Close)
	var hits atomic.Int64
	icons := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pluginKey {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		if strings.HasPrefix(r.URL.Path, "/icons/multi") {
			_, _ = w.Write(bytes.Repeat([]byte{1}, 3<<20)) // too large
			return
		}
		_, _ = w.Write(pngBytes)
	})
	fake := &iconFake{Fake: plugintest.New(), elsewhere: elsewhere.URL}
	u := pluginstest.ServeWithRoutes(t, fake, pluginKey, map[string]http.Handler{"/icons/": icons})
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)

	byID := map[string]apigen.Package{}
	for _, p := range call[apigen.PackageList](admin, http.MethodGet, "/api/v1/plugins/fake/packages", nil, 200).Packages {
		if p.IconUrl == nil || !strings.HasPrefix(*p.IconUrl, "/api/v1/plugins/fake/packages/"+p.Id+"/icon?") {
			t.Fatalf("%s icon %v", p.Id, p.IconUrl)
		}
		byID[p.Id] = p
	}

	// Fetched with the plugin's key from its own address, then cached.
	icon := *byID["example.comics"].IconUrl
	for range 2 {
		res := admin.do(http.MethodGet, icon, nil)
		if res.status != 200 || res.header.Get("Content-Type") != "image/png" || !bytes.Equal(res.body, pngBytes) {
			t.Fatalf("icon %d %s", res.status, res.header.Get("Content-Type"))
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("%d fetches, want 1 (cached)", hits.Load())
	}
	admin.expectProblem(http.MethodGet, strings.Replace(icon, "ref=", "ref=x", 1), nil, 403, "bad_signature")
	admin.expectProblem(http.MethodGet, strings.Replace(icon, "/example.comics/", "/multi.comics/", 1), nil, 403, "bad_signature")
	user.expectProblem(http.MethodGet, icon, nil, 403, "forbidden")

	// Too large: refused.
	admin.expectProblem(http.MethodGet, *byID["multi.comics"].IconUrl, nil, 502, "icon_failed")

	// Elsewhere: fetched without the plugin's key; SVG is never served.
	admin.expectProblem(http.MethodGet, *byID["beispiel.comics"].IconUrl, nil, 404, CodeNotFound)
	if got, _ := elsewhereAuth.Load().(string); got != "" {
		t.Fatalf("the plugin's key went elsewhere: %q", got)
	}
}

// failingFake fails package calls on demand.
type failingFake struct {
	*plugintest.Fake
	mu                   sync.Mutex
	list, install, langs error
}

func (f *failingFake) fail(which *error, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*which = err
}

func (f *failingFake) err(which *error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *which
}

func (f *failingFake) ListPackages(ctx context.Context, r *sourcev1.ListPackagesRequest) (*sourcev1.ListPackagesResponse, error) {
	if err := f.err(&f.list); err != nil {
		return nil, err
	}
	return f.Fake.ListPackages(ctx, r)
}

func (f *failingFake) InstallPackage(ctx context.Context, r *sourcev1.InstallPackageRequest) (*sourcev1.InstallPackageResponse, error) {
	if err := f.err(&f.install); err != nil {
		return nil, err
	}
	return f.Fake.InstallPackage(ctx, r)
}

func (f *failingFake) SetPackageSettings(ctx context.Context, r *sourcev1.SetPackageSettingsRequest) (*sourcev1.SetPackageSettingsResponse, error) {
	if err := f.err(&f.langs); err != nil {
		return nil, err
	}
	return f.Fake.SetPackageSettings(ctx, r)
}

// waitTaskAttempted waits until a task has run at least once and stopped.
func waitTaskAttempted(t *testing.T, c *client, id string) apigen.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := call[apigen.Task](c, http.MethodGet, "/api/v1/tasks/"+id, nil, 200)
		if task.Attempts > 0 && task.State != apigen.TaskStateRunning {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: %s after %d attempts", id, task.State, task.Attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPackageFailures(t *testing.T) {
	t.Parallel()
	logs := &logRecorder{}
	h := newHarness(t, withLog(slog.New(logs)))
	admin, _ := h.setupAdmin()
	fake := &failingFake{Fake: plugintest.New()}
	u := pluginstest.Serve(t, fake, pluginKey)
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": u, "api_key": pluginKey}, 201)
	const base = "/api/v1/plugins/fake/packages"

	// Store problems on the list.
	fake.fail(&fake.list, plugin.ErrStoreFetchFailed("store-a didn't answer"))
	if p := admin.expectProblem(http.MethodGet, base, nil, 502, "store_fetch_failed"); p.Detail == nil || !strings.Contains(*p.Detail, "store-a") {
		t.Fatalf("detail %+v", p)
	}
	fake.fail(&fake.list, plugin.ErrStoreInvalid("store-a's index is broken"))
	admin.expectProblem(http.MethodGet, base, nil, 502, "store_invalid")
	fake.fail(&fake.list, nil)
	// Outside the image routes, a 502 is an error in the request log:
	// not every handler logs its cause.
	if n := len(slices.DeleteFunc(logs.warnings(), func(m string) bool { return m != "request" })); n != 2 {
		t.Fatalf("%d request errors for two 502s, want 2", n)
	}

	// A request the plugin refuses.
	fake.fail(&fake.langs, plugin.ErrInvalidArgument("the package has no language xx"))
	admin.expectProblem(http.MethodPut, base+"/multi.comics/languages", map[string]any{"enabled_langs": []string{"xx"}}, 422, "package_rejected")

	// A package that fails its checks, or a broken store: the task fails at
	// once, with the plugin's reason.
	fake.fail(&fake.install, plugin.ErrInvalidArgument("bad signature"))
	ref := call[apigen.TaskRef](admin, http.MethodPost, base+"/multi.comics/install", nil, 202)
	task := waitTaskAttempted(t, admin, ref.TaskId.String())
	if task.State != apigen.TaskStateFailed || task.Attempts != 1 || task.LastError == nil || !strings.Contains(*task.LastError, "bad signature") {
		t.Fatalf("bad signature %+v", task)
	}
	fake.fail(&fake.install, plugin.ErrStoreInvalid("store-a's index is broken"))
	ref = call[apigen.TaskRef](admin, http.MethodPost, base+"/multi.comics/install", nil, 202)
	task = waitTaskAttempted(t, admin, ref.TaskId.String())
	if task.State != apigen.TaskStateFailed || task.Attempts != 1 || task.LastError == nil || !strings.HasPrefix(*task.LastError, "STORE_INVALID: ") {
		t.Fatalf("store invalid %+v", task)
	}

	// A store that couldn't be reached is tried again later.
	fake.fail(&fake.install, plugin.ErrStoreFetchFailed("store-a didn't answer"))
	ref = call[apigen.TaskRef](admin, http.MethodPost, base+"/multi.comics/install", nil, 202)
	task = waitTaskAttempted(t, admin, ref.TaskId.String())
	if task.State != apigen.TaskStateQueued || task.Attempts != 1 || task.LastError == nil || !strings.HasPrefix(*task.LastError, "STORE_FETCH_FAILED: ") {
		t.Fatalf("store fetch failed %+v", task)
	}
	if p := call[apigen.Package](admin, http.MethodGet, base+"/multi.comics", nil, 200); p.Installed || p.TaskId == nil || *p.TaskId != ref.TaskId {
		t.Fatalf("waiting to retry %+v", p)
	}
}
