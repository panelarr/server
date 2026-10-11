// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"flag"
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
		"metrics listen":  {EnvMetricsEnabled: "true"},
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

func TestMetricsOn(t *testing.T) {
	cfg, err := load(t, map[string]string{EnvConfigDir: "/config", EnvMetricsEnabled: "1", EnvMetricsListen: "127.0.0.1:9464"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.Enabled || cfg.Metrics.Listen != "127.0.0.1:9464" {
		t.Fatalf("metrics: %+v", cfg.Metrics)
	}
}
