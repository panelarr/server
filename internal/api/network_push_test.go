// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// pushFake counts profile pushes and can pretend to have restarted.
type pushFake struct {
	*plugintest.Fake
	instance atomic.Value
	pushes   atomic.Int64
}

func (f *pushFake) Health(context.Context, *sourcev1.HealthRequest) (*sourcev1.HealthResponse, error) {
	id, _ := f.instance.Load().(string)
	return &sourcev1.HealthResponse{Status: sourcev1.HealthStatus_HEALTH_STATUS_SERVING, InstanceId: id}, nil
}

func (f *pushFake) SetNetworkProfile(ctx context.Context, r *sourcev1.SetNetworkProfileRequest) (*sourcev1.SetNetworkProfileResponse, error) {
	f.pushes.Add(1)
	return f.Fake.SetNetworkProfile(ctx, r)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pushStates(t *testing.T, c *client) map[string]apigen.NetworkPushState {
	t.Helper()
	out := map[string]apigen.NetworkPushState{}
	for _, s := range call[apigen.PluginNetwork](c, http.MethodGet, "/api/v1/plugins/fake/network", nil, 200).Sources {
		out[s.SourceId] = s.Push.State
	}
	return out
}

func TestNetworkProfilesPush(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	fake := &pushFake{Fake: plugintest.New()}
	fake.instance.Store("first")
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": pluginstest.Serve(t, fake, pluginKey), "api_key": pluginKey}, 201)
	call[apigen.Solver](admin, http.MethodPost, "/api/v1/network/solvers", map[string]any{"id": "fs", "name": "FS", "type": "flaresolverr", "url": "http://fs:8191"}, 201)
	call[apigen.Proxy](admin, http.MethodPost, "/api/v1/network/proxies", map[string]any{"id": "nl", "name": "NL", "url": "http://gluetun:8888", "username": "u", "password": "pw-1"}, 201)
	call[apigen.NetworkProfile](admin, http.MethodPost, "/api/v1/network/profiles", map[string]any{
		"id": "vpn", "name": "VPN", "proxy_id": "nl", "solver_id": "fs", "rate_limit": map[string]any{"requests": 1, "period_seconds": 2},
	}, 201)

	// Nothing assigned: nothing pushed.
	if st := pushStates(t, admin); st["fake:1001"] != apigen.NetworkPushStateNotNeeded || fake.pushes.Load() != 0 {
		t.Fatalf("before %v, %d pushes", st, fake.pushes.Load())
	}

	// Assigned to the plugin: every source gets it, with the proxy's
	// password, and without the solver (the plugin doesn't take one).
	call[apigen.NetworkAssignment](admin, http.MethodPut, "/api/v1/network/assignments", map[string]any{"target_kind": "plugin", "target_id": "fake", "profile_id": "vpn"}, 200)
	waitFor(t, "the push", func() bool { return fake.Profile("1001") != nil && fake.Profile("2001") != nil })
	p := fake.Profile("1001")
	if p.GetName() != "VPN" || p.GetProxy().GetUrl() != "http://gluetun:8888" || p.GetProxy().GetPassword() != "pw-1" || p.GetSolver() != nil || p.GetRateLimit().GetRequests() != 1 {
		t.Fatalf("pushed %v", p)
	}
	waitFor(t, "push states", func() bool {
		st := pushStates(t, admin)
		return st["fake:1001"] == apigen.NetworkPushStatePushed && st["fake:2001"] == apigen.NetworkPushStatePushed
	})
	sn := call[apigen.SourceNetwork](admin, http.MethodGet, "/api/v1/sources/fake:1001/network", nil, 200)
	if sn.Applied == nil || sn.Applied.Origin != apigen.NetworkAppliedOriginPushed || sn.Effective.Level != "plugin" {
		t.Fatalf("source network %+v", sn)
	}

	// One source back on the plugin's own settings.
	call[apigen.NetworkAssignment](admin, http.MethodPut, "/api/v1/network/assignments", map[string]any{"target_kind": "source", "target_id": "fake:2001", "plugin_default": true}, 200)
	waitFor(t, "the unset", func() bool { return fake.Profile("2001") == nil })
	if fake.Profile("1001") == nil {
		t.Fatal("the other source lost its profile")
	}

	// A changed password reaches the plugin.
	call[apigen.Proxy](admin, http.MethodPatch, "/api/v1/network/proxies/nl", map[string]any{"password": "pw-2"}, 200)
	waitFor(t, "the new password", func() bool { return fake.Profile("1001").GetProxy().GetPassword() == "pw-2" })

	// A sync with nothing changed pushes nothing; a restarted plugin gets
	// everything again.
	waitFor(t, "quiet", func() bool { return pushStates(t, admin)["fake:1001"] == apigen.NetworkPushStatePushed })
	before := fake.pushes.Load()
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	if n := fake.pushes.Load(); n != before {
		t.Fatalf("%d pushes for nothing", n-before)
	}
	fake.instance.Store("second")
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins/fake/sync", nil, 200)
	if n := fake.pushes.Load() - before; n != 2 {
		t.Fatalf("%d pushes after a restart, want 2", n)
	}
}
