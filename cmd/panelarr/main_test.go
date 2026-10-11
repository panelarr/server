// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/panelarr/server/internal/config"
	"github.com/panelarr/server/internal/store/storetest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"migrate", "sideways"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard, env(nil)); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestServeNeedsDatabase(t *testing.T) {
	err := run(context.Background(), []string{"serve"}, io.Discard, io.Discard, env(map[string]string{config.EnvConfigDir: t.TempDir()}))
	if err == nil || !strings.Contains(err.Error(), config.EnvDatabaseURL) {
		t.Fatalf("got %v", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	metricsAddr := freeAddr(t)
	e := env(map[string]string{
		config.EnvDatabaseURL:    storetest.URL(t),
		config.EnvConfigDir:      dir + "/config",
		config.EnvDataDir:        dir + "/data",
		config.EnvMetricsEnabled: "true",
		config.EnvMetricsListen:  metricsAddr,
	})

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() { done <- serve(ctx, []string{"--listen", "127.0.0.1:0"}, &logs, io.Discard, e, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve stopped: %v\n%s", err, logs.String())
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not start")
	}
	base := "http://" + addr

	if code, _ := get(t, base+"/healthz"); code != 200 {
		t.Fatalf("healthz %d", code)
	}
	if code, body := get(t, base+"/readyz"); code != 200 || !strings.Contains(body, `"database":{"status":"ok"}`) {
		t.Fatalf("readyz %d %s", code, body)
	}
	if code, body := get(t, base+"/api/v1/system/info"); code != 200 || !strings.Contains(body, `"name":"panelarr"`) {
		t.Fatalf("system info %d %s", code, body)
	}
	if code, body := get(t, "http://"+metricsAddr+"/metrics"); code != 200 || !strings.Contains(body, "panelarr_http_requests_total") {
		t.Fatalf("metrics %d", code)
	}
	if code, _ := get(t, base+"/metrics"); code == 200 {
		t.Fatal("metrics are open on the API port")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(logs.String(), `"service":"server"`) || !strings.Contains(logs.String(), `"msg":"migration applied"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}

	// The database is migrated now; migrate reports it and can roll back.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"migrate", "status"}, &out, io.Discard, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "applied") || strings.Contains(out.String(), "pending") {
		t.Fatalf("status:\n%s", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"migrate", "up"}, &out, io.Discard, e); err != nil || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("up: %v %s", err, out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"migrate", "down"}, &out, io.Discard, e); err != nil || !strings.Contains(out.String(), "rolled back") {
		t.Fatalf("down: %v %s", err, out.String())
	}
}

func TestHealthcheck(t *testing.T) {
	srv := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}), ReadHeaderTimeout: time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if code := runHealthcheck(nil, io.Discard, env(map[string]string{config.EnvListen: ":" + port})); code != 0 {
		t.Fatalf("healthy server: exit %d", code)
	}
	if code := runHealthcheck([]string{"--url", "http://" + ln.Addr().String() + "/nope"}, io.Discard, env(nil)); code != 1 {
		t.Fatalf("404: exit %d", code)
	}
}
