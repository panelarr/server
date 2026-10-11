// SPDX-License-Identifier: GPL-3.0-or-later

// Package config loads the server's startup settings, *arr-style: from
// environment variables and command-line flags. Only what the server needs
// before it can reach its database lives here. Everything else is managed in
// the app and stored in the database.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the server's startup configuration.
type Config struct {
	// Listen is the address of the HTTP server. Default ":8900".
	Listen string
	// DatabaseURL is the PostgreSQL URL (or key=value DSN). Required.
	DatabaseURL string
	// ConfigDir holds the server's own files: the encryption key for stored
	// secrets, caches and logs. The image uses /config.
	ConfigDir string
	// DataDir is the default library location for downloads. The image
	// uses /data.
	DataDir string
	// BaseURL is the public URL the server is reached at, e.g.
	// "https://panelarr.example.com". Optional. It decides whether cookies
	// are marked Secure and how absolute links are built.
	BaseURL string
	// Log configures logging.
	Log Log
	// Metrics configures the Prometheus endpoint.
	Metrics Metrics
}

// Log configures structured logging.
type Log struct {
	// Format is "json" (the default) or "text".
	Format string
	// Level is "debug", "info" (the default), "warn" or "error".
	Level string
}

// Metrics configures Prometheus metrics. They are off by default and, when
// on, served on their own address so they aren't open on the API port.
type Metrics struct {
	Enabled bool
	// Listen is the metrics address, e.g. "127.0.0.1:9464". Required while
	// metrics are on.
	Listen string
}

// Env names. Each setting can also be given as a flag.
const (
	EnvListen         = "PANELARR_LISTEN"
	EnvDatabaseURL    = "PANELARR_DB"
	EnvConfigDir      = "PANELARR_CONFIG"
	EnvDataDir        = "PANELARR_DATA"
	EnvBaseURL        = "PANELARR_BASE_URL"
	EnvLogFormat      = "PANELARR_LOG_FORMAT"
	EnvLogLevel       = "PANELARR_LOG_LEVEL"
	EnvMetricsEnabled = "PANELARR_METRICS_ENABLED"
	EnvMetricsListen  = "PANELARR_METRICS_LISTEN"
)

// DefaultListen is the server's port by convention.
const DefaultListen = ":8900"

// Flags binds the settings to fs. Call Load after fs.Parse.
type Flags struct {
	listen, databaseURL, configDir, dataDir, baseURL string
	logFormat, logLevel                              string
	metricsEnabled, metricsListen                    string
}

// Register adds the flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.listen, "listen", "", "HTTP listen address (env "+EnvListen+", default "+DefaultListen+")")
	fs.StringVar(&f.databaseURL, "db", "", "PostgreSQL URL (env "+EnvDatabaseURL+")")
	fs.StringVar(&f.configDir, "config-dir", "", "directory for the server's own files (env "+EnvConfigDir+")")
	fs.StringVar(&f.dataDir, "data-dir", "", "default library directory (env "+EnvDataDir+")")
	fs.StringVar(&f.baseURL, "base-url", "", "public URL of the server (env "+EnvBaseURL+")")
	fs.StringVar(&f.logFormat, "log-format", "", "json or text (env "+EnvLogFormat+")")
	fs.StringVar(&f.logLevel, "log-level", "", "debug, info, warn or error (env "+EnvLogLevel+")")
	fs.StringVar(&f.metricsEnabled, "metrics", "", "serve Prometheus metrics: true or false (env "+EnvMetricsEnabled+")")
	fs.StringVar(&f.metricsListen, "metrics-listen", "", "metrics listen address (env "+EnvMetricsListen+")")
}

// Load builds the configuration from defaults, then the environment (read
// through getenv), then the flags, and validates it.
func (f *Flags) Load(getenv func(string) string) (Config, error) {
	pick := func(flagVal, env, def string) string {
		if flagVal != "" {
			return flagVal
		}
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v
		}
		return def
	}
	cfg := Config{
		Listen:      pick(f.listen, EnvListen, DefaultListen),
		DatabaseURL: pick(f.databaseURL, EnvDatabaseURL, ""),
		ConfigDir:   pick(f.configDir, EnvConfigDir, ""),
		DataDir:     pick(f.dataDir, EnvDataDir, ""),
		BaseURL:     strings.TrimRight(pick(f.baseURL, EnvBaseURL, ""), "/"),
		Log: Log{
			Format: strings.ToLower(pick(f.logFormat, EnvLogFormat, "json")),
			Level:  strings.ToLower(pick(f.logLevel, EnvLogLevel, "info")),
		},
		Metrics: Metrics{Listen: pick(f.metricsListen, EnvMetricsListen, "")},
	}
	v := pick(f.metricsEnabled, EnvMetricsEnabled, "false")
	b, err := strconv.ParseBool(v)
	if err != nil {
		return cfg, fmt.Errorf("config: %s: %q is not true or false", EnvMetricsEnabled, v)
	}
	cfg.Metrics.Enabled = b
	if cfg.ConfigDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return cfg, fmt.Errorf("config: no %s and no user config directory: %w", EnvConfigDir, err)
		}
		cfg.ConfigDir = filepath.Join(dir, "panelarr")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(cfg.ConfigDir, "data")
	}
	return cfg, cfg.Validate()
}

// Validate checks the settings that can be checked without side effects.
// A missing database URL is not an error here: commands that need the
// database check it themselves.
func (c Config) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("%s: %q: %w", EnvListen, c.Listen, err))
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: %q is not an http(s) URL", EnvBaseURL, c.BaseURL))
		}
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("%s: %q is not json or text", EnvLogFormat, c.Log.Format))
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.Log.Level)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %q is not debug, info, warn or error", EnvLogLevel, c.Log.Level))
	}
	if c.Metrics.Enabled {
		if c.Metrics.Listen == "" {
			errs = append(errs, fmt.Errorf("%s: required while metrics are on, so they aren't open on the API port", EnvMetricsListen))
		} else if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
			errs = append(errs, fmt.Errorf("%s: %q: %w", EnvMetricsListen, c.Metrics.Listen, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// RequireDatabase reports a clear error when no database URL is set.
func (c Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("config: %s is required (a PostgreSQL URL, e.g. postgres://panelarr:secret@db:5432/panelarr)", EnvDatabaseURL)
	}
	return nil
}

// SecureCookies reports whether the public URL is https.
func (c Config) SecureCookies() bool { return strings.HasPrefix(c.BaseURL, "https://") }
