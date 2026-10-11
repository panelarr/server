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
	"net/netip"
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
	// SecretKey encrypts stored secrets (service and plugin keys, storage
	// credentials): base64 of 32 bytes. Empty: a key file in ConfigDir,
	// generated on first use.
	SecretKey string
	// TrustedProxies are the networks whose X-Forwarded-For header is
	// believed when finding a client's address (for login limits and the
	// session list). Default: loopback and private networks, where reverse
	// proxies usually run.
	TrustedProxies []netip.Prefix
	// Meta, when its URL is set, is the metadata service connection; it
	// then can't be changed in the app. Empty: an admin connects it in the
	// first-run wizard.
	MetaURL    string
	MetaAPIKey string
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

// Metrics configures Prometheus metrics. They are off by default. When on,
// they are served on their own address, or on the API port to callers
// whose API key has the metrics:read scope.
type Metrics struct {
	Enabled bool
	// Listen is the metrics address, e.g. "127.0.0.1:9464". Empty: /metrics
	// on the API port, behind a metrics:read key.
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
	EnvSecretKey      = "PANELARR_SECRET_KEY"
	EnvTrustedProxies = "PANELARR_TRUSTED_PROXIES"
	EnvMetaURL        = "PANELARR_META_URL"
	EnvMetaAPIKey     = "PANELARR_META_KEY" //nolint:gosec // a variable name, not a credential
)

// DefaultTrustedProxies are loopback and private networks.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// DefaultListen is the server's port by convention.
const DefaultListen = ":8900"

// Flags binds the settings to fs. Call Load after fs.Parse.
type Flags struct {
	listen, databaseURL, configDir, dataDir, baseURL string
	logFormat, logLevel                              string
	metricsEnabled, metricsListen                    string
	trustedProxies                                   string
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
	fs.StringVar(&f.trustedProxies, "trusted-proxies", "", "comma-separated networks of trusted reverse proxies, or \"none\" (env "+EnvTrustedProxies+")")
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
		Metrics:    Metrics{Listen: pick(f.metricsListen, EnvMetricsListen, "")},
		SecretKey:  strings.TrimSpace(getenv(EnvSecretKey)),
		MetaURL:    strings.TrimRight(strings.TrimSpace(getenv(EnvMetaURL)), "/"),
		MetaAPIKey: strings.TrimSpace(getenv(EnvMetaAPIKey)),
	}
	proxies, err := parsePrefixes(pick(f.trustedProxies, EnvTrustedProxies, DefaultTrustedProxies))
	if err != nil {
		return cfg, fmt.Errorf("config: %s: %w", EnvTrustedProxies, err)
	}
	cfg.TrustedProxies = proxies
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
	if c.MetaURL != "" {
		u, err := url.Parse(c.MetaURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: %q is not an http(s) URL", EnvMetaURL, c.MetaURL))
		}
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("%s: %q is not json or text", EnvLogFormat, c.Log.Format))
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.Log.Level)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %q is not debug, info, warn or error", EnvLogLevel, c.Log.Level))
	}
	if c.Metrics.Enabled && c.Metrics.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
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

// parsePrefixes parses a comma-separated list of networks (or single
// addresses). "none" means an empty list.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("%q is not a network or address", part)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a network or address", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// SecureCookies reports whether the public URL is https.
func (c Config) SecureCookies() bool { return strings.HasPrefix(c.BaseURL, "https://") }
