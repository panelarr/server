// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// secretSettingsFake adds a secret "token" to the fake's source settings.
type secretSettingsFake struct {
	*plugintest.Fake
	mu    sync.Mutex
	token string
}

func (f *secretSettingsFake) GetSettingsSchema(ctx context.Context, r *sourcev1.GetSettingsSchemaRequest) (*sourcev1.GetSettingsSchemaResponse, error) {
	res, err := f.Fake.GetSettingsSchema(ctx, r)
	if err != nil {
		return nil, err
	}
	props := res.GetSchema().GetFields()["properties"].GetStructValue()
	tok, _ := structpb.NewStruct(map[string]any{"type": "string", "writeOnly": true})
	props.Fields["token"] = structpb.NewStructValue(tok)
	return res, nil
}

func (f *secretSettingsFake) GetSettings(ctx context.Context, r *sourcev1.GetSettingsRequest) (*sourcev1.GetSettingsResponse, error) {
	res, err := f.Fake.GetSettings(ctx, r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	res.Values.Fields["token"] = structpb.NewStringValue(f.token)
	f.mu.Unlock()
	return res, nil
}

func (f *secretSettingsFake) SetSettings(ctx context.Context, r *sourcev1.SetSettingsRequest) (*sourcev1.SetSettingsResponse, error) {
	f.mu.Lock()
	if v, ok := r.GetValues().GetFields()["token"]; ok {
		f.token = v.GetStringValue()
		delete(r.Values.Fields, "token")
	}
	tok := f.token
	f.mu.Unlock()
	res, err := f.Fake.SetSettings(ctx, r)
	if err != nil {
		return nil, err
	}
	res.Values.Fields["token"] = structpb.NewStringValue(tok)
	return res, nil
}

func (f *secretSettingsFake) secret() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func TestSourceSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin, _ := h.setupAdmin()
	user, _ := h.createUser(admin, "reader", nil)
	fake := &secretSettingsFake{Fake: plugintest.New()}
	call[apigen.Plugin](admin, http.MethodPost, "/api/v1/plugins", map[string]string{"url": pluginstest.Serve(t, fake, pluginKey), "api_key": pluginKey}, 201)
	const url = "/api/v1/sources/fake:1001/settings"

	st := call[apigen.SourceSettings](admin, http.MethodGet, url, nil, 200)
	if st.Schema["properties"] == nil || st.Values["show_adult"] != false {
		t.Fatalf("settings %+v", st)
	}
	st = call[apigen.SourceSettings](admin, http.MethodPatch, url, map[string]any{"values": map[string]any{"show_adult": true, "token": "s3cret"}}, 200)
	if st.Values["show_adult"] != true || st.Values["token"] != "" || fake.secret() != "s3cret" {
		t.Fatalf("saved %+v (token %q)", st.Values, fake.secret())
	}
	if strings.Contains(string(admin.do(http.MethodGet, url, nil).body), "s3cret") {
		t.Fatal("the secret is shown")
	}
	// A secret left empty keeps its value; keys left out keep theirs.
	st = call[apigen.SourceSettings](admin, http.MethodPatch, url, map[string]any{"values": map[string]any{"token": ""}}, 200)
	if fake.secret() != "s3cret" || st.Values["show_adult"] != true {
		t.Fatalf("kept %+v (token %q)", st.Values, fake.secret())
	}
	// The plugin's checks; a source without settings; users.
	admin.expectProblem(http.MethodPatch, url, map[string]any{"values": map[string]any{"show_adult": "yes"}}, 400, CodeInvalidRequest)
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:2001/settings", nil, 422, "unsupported")
	admin.expectProblem(http.MethodGet, "/api/v1/sources/fake:9999/settings", nil, 404, CodeNotFound)
	user.expectProblem(http.MethodGet, url, nil, 403, "forbidden")
	// A source turned off can still be set up.
	call[apigen.SourceInfo](admin, http.MethodPatch, "/api/v1/sources/fake:1001", map[string]any{"enabled": false}, 200)
	call[apigen.SourceSettings](admin, http.MethodGet, url, nil, 200)
}
