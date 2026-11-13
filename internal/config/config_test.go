// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func load(t *testing.T, env map[string]string, args ...string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f Flags
	f.Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f.Load(func(k string) string { return env[k] })
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8900" || cfg.Log.Format != "json" || cfg.Log.Level != "info" || cfg.Metrics.Enabled {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.DataDir != "/config/data" {
		t.Fatalf("data dir: %q", cfg.DataDir)
	}
	if cfg.RequireDatabase() == nil {
		t.Fatal("missing database URL accepted")
	}
}

func TestFlagsWinOverEnv(t *testing.T) {
	env := map[string]string{EnvConfigDir: "/config", EnvListen: ":1", EnvDatabaseURL: "postgres://env", EnvLogFormat: "TEXT"}
	cfg, err := load(t, env, "--listen", ":2", "--base-url", "https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":2" || cfg.DatabaseURL != "postgres://env" || cfg.Log.Format != "text" {
		t.Fatalf("got %+v", cfg)
	}
	if cfg.BaseURL != "https://example.test" || !cfg.SecureCookies() {
		t.Fatalf("base URL: %q", cfg.BaseURL)
	}
}

func TestValidation(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"listen":          {EnvListen: "8900"},
		"base url":        {EnvBaseURL: "example.test"},
		"log format":      {EnvLogFormat: "xml"},
		"log level":       {EnvLogLevel: "loud"},
		"metrics bool":    {EnvMetricsEnabled: "maybe"},
		"trusted proxies": {EnvTrustedProxies: "10.0.0.0/8,example"},
		"metrics address": {EnvMetricsEnabled: "true", EnvMetricsListen: "9464"},
	} {
		t.Run(name, func(t *testing.T) {
			env[EnvConfigDir] = "/config"
			_, err := load(t, env)
			if err == nil || !strings.HasPrefix(err.Error(), "config:") {
				t.Fatalf("want a config error, got %v", err)
			}
		})
	}
}

func TestTrustedProxies(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config"})
	if err != nil || len(cfg.TrustedProxies) != 6 {
		t.Fatalf("defaults: %v %v", cfg.TrustedProxies, err)
	}
	cfg, err = load(t, map[string]string{EnvConfigDir: "/config", EnvTrustedProxies: "203.0.113.7, 10.1.2.3/8"})
	if err != nil || len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0].String() != "203.0.113.7/32" || cfg.TrustedProxies[1].String() != "10.0.0.0/8" {
		t.Fatalf("custom: %v %v", cfg.TrustedProxies, err)
	}
	cfg, err = load(t, map[string]string{EnvConfigDir: "/config"}, "--trusted-proxies", "none")
	if err != nil || len(cfg.TrustedProxies) != 0 {
		t.Fatalf("none: %v %v", cfg.TrustedProxies, err)
	}
}

func TestMetricsOnAPIPort(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config", EnvMetricsEnabled: "true"})
	if err != nil || !cfg.Metrics.Enabled || cfg.Metrics.Listen != "" {
		t.Fatalf("got %+v, %v", cfg.Metrics, err)
	}
}

func TestMetricsOn(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config", EnvMetricsEnabled: "1", EnvMetricsListen: "127.0.0.1:9464"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.Enabled || cfg.Metrics.Listen != "127.0.0.1:9464" {
		t.Fatalf("metrics: %+v", cfg.Metrics)
	}
}

func TestOIDCOffByDefault(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.Enabled() {
		t.Fatalf("OIDC on without configuration: %v", cfg.OIDC)
	}
}

func TestOIDC(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("s3cret-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(t, map[string]string{
		EnvConfigDir: "/config", EnvBaseURL: "https://panelarr.example.com",
		EnvOIDCIssuer: "https://id.example.com/realm/", EnvOIDCClientID: "panelarr",
		EnvOIDCClientSecretFile: secretFile, EnvOIDCScopes: "profile groups,email",
		EnvOIDCAllowedGroups: "readers, admins,readers",
	})
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.OIDC
	if !o.Enabled() || o.Issuer != "https://id.example.com/realm/" || o.ClientID != "panelarr" || o.ClientSecret != "s3cret-from-file" {
		t.Fatalf("got %+v", o)
	}
	if !slices.Equal(o.Scopes, []string{"openid", "profile", "groups", "email"}) {
		t.Fatalf("scopes: %v", o.Scopes)
	}
	if !slices.Equal(o.AllowedGroups, []string{"readers", "admins"}) || o.GroupsClaim != "groups" || o.ButtonLabel != "Sign in with SSO" {
		t.Fatalf("got %+v", o)
	}
	// The secret never shows when the settings are printed or logged,
	// alone or as part of the whole configuration.
	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("config", "oidc", o, "cfg", cfg)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "oidc", o, "cfg", cfg)
	for _, out := range []string{
		fmt.Sprint(o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o),
		fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), logged.String(),
	} {
		if strings.Contains(out, "s3cret") {
			t.Fatalf("secret printed: %s", out)
		}
	}
}

// The database URL's password and the other secrets stay out of logs too.
func TestConfigLogValueRedacts(t *testing.T) {
	cfg, err := load(t, map[string]string{
		EnvConfigDir: "/config", EnvDatabaseURL: "postgres://panelarr:dbpass@db/panelarr",
		EnvSecretKey: "c2VjcmV0LWtleS1zZWNyZXQta2V5LXNlY3JldC1rZXk=",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MetaAPIKey = "meta-key-value"
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "cfg", cfg)
	for _, s := range []string{"dbpass", cfg.SecretKey, "meta-key-value"} {
		if strings.Contains(logged.String(), s) || strings.Contains(fmt.Sprintf("%+v", cfg), s) {
			t.Fatalf("%q logged: %s", s, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "postgres://panelarr:xxxxx@db/panelarr") {
		t.Fatalf("database URL not shown redacted: %s", logged.String())
	}
}

func TestOIDCLocalhostMayUseHTTP(t *testing.T) {
	for _, issuer := range []string{"http://localhost:5556/dex", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		_, err := load(t, map[string]string{
			EnvConfigDir: "/config", EnvBaseURL: "http://localhost:8900",
			EnvOIDCIssuer: issuer, EnvOIDCClientID: "panelarr",
		})
		if err != nil {
			t.Fatalf("%s: %v", issuer, err)
		}
	}
}

func TestOIDCValidation(t *testing.T) {
	ok := func() map[string]string {
		return map[string]string{
			EnvConfigDir: "/config", EnvBaseURL: "https://panelarr.example.com",
			EnvOIDCIssuer: "https://id.example.com", EnvOIDCClientID: "panelarr",
		}
	}
	for name, change := range map[string]map[string]string{
		"no base url":         {EnvBaseURL: ""},
		"no client id":        {EnvOIDCClientID: ""},
		"plain http":          {EnvOIDCIssuer: "http://id.example.com"},
		"not a url":           {EnvOIDCIssuer: "id.example.com"},
		"issuer query":        {EnvOIDCIssuer: "https://id.example.com/?x=1"},
		"secret twice":        {EnvOIDCClientSecret: "a", EnvOIDCClientSecretFile: "/nonexistent"},
		"missing secret file": {EnvOIDCClientSecretFile: "/nonexistent/secret"},
		"no issuer":           {EnvOIDCIssuer: ""},
		"only scopes":         {EnvOIDCIssuer: "", EnvOIDCClientID: "", EnvOIDCScopes: "openid"},
		"only button label":   {EnvOIDCIssuer: "", EnvOIDCClientID: "", EnvOIDCButtonLabel: "SSO"},
		"only groups claim":   {EnvOIDCIssuer: "", EnvOIDCClientID: "", EnvOIDCGroupsClaim: "roles"},
		"only allowed groups": {EnvOIDCIssuer: "", EnvOIDCClientID: "", EnvOIDCAllowedGroups: "readers"},
	} {
		t.Run(name, func(t *testing.T) {
			env := ok()
			for k, v := range change {
				env[k] = v
			}
			_, err := load(t, env)
			if err == nil || !strings.HasPrefix(err.Error(), "config:") {
				t.Fatalf("want a config error, got %v", err)
			}
		})
	}
	if _, err := load(t, ok()); err != nil {
		t.Fatalf("valid: %v", err)
	}
}
