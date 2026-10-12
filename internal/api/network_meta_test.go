// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"testing"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta/metatest"
)

func providerNetworks(t *testing.T, c *client) map[string]apigen.ProviderNetwork {
	t.Helper()
	out := map[string]apigen.ProviderNetwork{}
	for _, p := range call[apigen.MetaNetwork](c, http.MethodGet, "/api/v1/system/meta/network", nil, 200).Providers {
		out[p.ProviderId] = p
	}
	return out
}

func TestNetworkProfilesToMeta(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	admin.expectProblem(http.MethodGet, "/api/v1/system/meta/network", nil, 503, "meta_unavailable")
	fake := metatest.New(t)
	fake.SetProviders(map[string]int64{"books": 2, "comics": 1})
	fake.SetUnsupported("comics")
	connect := map[string]string{"url": fake.URL, "api_key": metatest.AdminKey}
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", connect, 200)
	call[apigen.Solver](admin, http.MethodPost, "/api/v1/network/solvers", map[string]any{"id": "fs", "name": "FS", "type": "byparr", "url": "http://byparr:8191"}, 201)
	call[apigen.Proxy](admin, http.MethodPost, "/api/v1/network/proxies", map[string]any{"id": "nl", "name": "NL", "url": "socks5://tor:9050", "username": "u", "password": "pw"}, 201)
	call[apigen.NetworkProfile](admin, http.MethodPost, "/api/v1/network/profiles", map[string]any{"id": "vpn", "name": "VPN", "proxy_id": "nl", "solver_id": "fs", "doh_url": "https://dns.example/dns-query"}, 201)

	// Nothing assigned: nothing pushed.
	pn := providerNetworks(t, admin)
	if len(pn) != 2 || !pn["books"].AcceptsProfiles || pn["comics"].AcceptsProfiles || pn["books"].Push.State != apigen.NetworkPushStateNotNeeded ||
		pn["books"].Applied.Origin != apigen.ProviderNetworkAppliedOriginNone || pn["books"].Effective.Level != "none" {
		t.Fatalf("before %+v", pn)
	}

	// A provider's profile goes to meta with the password and the solver.
	assign := func(body map[string]any) {
		call[apigen.NetworkAssignment](admin, http.MethodPut, "/api/v1/network/assignments", body, 200)
	}
	assign(map[string]any{"target_kind": "provider", "target_id": "books", "profile_id": "vpn"})
	waitFor(t, "the push to meta", func() bool { return fake.Pushed("books") != nil })
	p := fake.Pushed("books")
	if p.GetName() != "VPN" || p.GetProxy().GetPassword() != "pw" || p.GetSolver().GetUrl() != "http://byparr:8191" || p.GetDohUrl() != "https://dns.example/dns-query" {
		t.Fatalf("pushed %v", p)
	}
	waitFor(t, "the push state", func() bool { return providerNetworks(t, admin)["books"].Push.State == apigen.NetworkPushStatePushed })
	if b := providerNetworks(t, admin)["books"]; b.Applied.Origin != apigen.ProviderNetworkAppliedOriginPushed || *b.Applied.ProfileName != "VPN" || b.Effective.Level != "provider" {
		t.Fatalf("books %+v", b)
	}

	// Global reaches every provider meta has (meta hands it on only to
	// plugins that take it); meta's own settings at the meta level stop it.
	assign(map[string]any{"target_kind": "global", "profile_id": "vpn"})
	waitFor(t, "global", func() bool { return fake.Pushed("comics") != nil })
	assign(map[string]any{"target_kind": "meta", "plugin_default": true})
	waitFor(t, "meta's own settings", func() bool { return fake.Pushed("comics") == nil })
	if fake.Pushed("books") == nil {
		t.Fatal("the provider's own assignment lost to the meta level")
	}
	if c := providerNetworks(t, admin)["comics"]; c.Effective.Level != "meta" || !c.Effective.PluginDefault {
		t.Fatalf("comics %+v", c)
	}

	// A meta that lost its pushed profiles gets them back.
	fake.ForgetPushed()
	call[apigen.MetaConnection](admin, http.MethodPut, "/api/v1/system/meta", connect, 200)
	waitFor(t, "the re-push", func() bool { return fake.Pushed("books") != nil })
}
