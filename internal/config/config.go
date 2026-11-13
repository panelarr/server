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
	"slices"
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
	// WebDir serves the web app from a directory (a local build) instead of
	// the one embedded in the binary. For development.
	WebDir string
	// Log configures logging.
	Log Log
	// Metrics configures the Prometheus endpoint.
	Metrics Metrics
	// OIDC configures single sign-on. Off unless an issuer is set.
	OIDC OIDC
}

// OIDC configures signing in with an OpenID Connect provider. It is off
// unless Issuer is set; password login keeps working either way.
type OIDC struct {
	// Issuer is the provider's issuer URL; discovery reads
	// <Issuer>/.well-known/openid-configuration. https, except on
	// localhost.
	Issuer string
	// ClientID is Panelarr's client at the provider.
	ClientID string
	// ClientSecret is the client's secret; empty for a public client
	// (PKCE only). Never logged.
	ClientSecret string
	// Scopes asked for; always includes openid.
	Scopes []string
	// ButtonLabel is the login page's button.
	ButtonLabel string
	// GroupsClaim names the claim that lists the user's groups.
	GroupsClaim string
	// AllowedGroups, when set, lets only their members sign in with it.
	AllowedGroups []string
}

// Enabled reports whether single sign-on is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

// LogValue keeps the client secret out of logs.
func (o OIDC) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("issuer", o.Issuer), slog.String("client_id", o.ClientID),
		slog.Bool("client_secret_set", o.ClientSecret != ""), slog.Any("scopes", o.Scopes),
	)
}

// String keeps the client secret out of formatted output.
func (o OIDC) String() string { return o.LogValue().String() }

// GoString keeps the client secret out of %#v.
func (o OIDC) GoString() string { return o.String() }

// LogValue keeps secrets out of logs: the database URL's password, the
// secret key, the metadata service's API key and the OIDC client secret.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen", c.Listen), slog.String("database_url", redactURL(c.DatabaseURL)),
		slog.String("config_dir", c.ConfigDir), slog.String("data_dir", c.DataDir),
		slog.String("base_url", c.BaseURL), slog.Bool("secret_key_set", c.SecretKey != ""),
		slog.Any("trusted_proxies", c.TrustedProxies), slog.String("meta_url", c.MetaURL),
		slog.Bool("meta_api_key_set", c.MetaAPIKey != ""), slog.String("web_dir", c.WebDir),
		slog.String("log_format", c.Log.Format), slog.String("log_level", c.Log.Level),
		slog.Bool("metrics_enabled", c.Metrics.Enabled), slog.String("metrics_listen", c.Metrics.Listen),
		slog.Any("oidc", c.OIDC),
	)
}

// String keeps secrets out of formatted output.
func (c Config) String() string { return c.LogValue().String() }

// GoString keeps secrets out of %#v.
func (c Config) GoString() string { return c.String() }

// redactURL hides a database URL's password. A key=value DSN, which
// can't be parsed as a URL, is hidden whole.
func redactURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return "[redacted]"
	}
	return u.Redacted()
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
	EnvWebDir         = "PANELARR_WEB_DIR"

	EnvOIDCIssuer           = "PANELARR_OIDC_ISSUER"
	EnvOIDCClientID         = "PANELARR_OIDC_CLIENT_ID"
	EnvOIDCClientSecret     = "PANELARR_OIDC_CLIENT_SECRET"      //nolint:gosec // a variable name, not a credential
	EnvOIDCClientSecretFile = "PANELARR_OIDC_CLIENT_SECRET_FILE" //nolint:gosec // a variable name, not a credential
	EnvOIDCScopes           = "PANELARR_OIDC_SCOPES"
	EnvOIDCButtonLabel      = "PANELARR_OIDC_BUTTON_LABEL"
	EnvOIDCGroupsClaim      = "PANELARR_OIDC_GROUPS_CLAIM"
	EnvOIDCAllowedGroups    = "PANELARR_OIDC_ALLOWED_GROUPS"
)

// OIDC defaults.
const (
	DefaultOIDCScopes      = "openid profile email"
	DefaultOIDCButtonLabel = "Sign in with SSO"
	DefaultOIDCGroupsClaim = "groups"
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
		WebDir:     strings.TrimSpace(getenv(EnvWebDir)),
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
	oidc, err := loadOIDC(getenv)
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	cfg.OIDC = oidc
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
	errs = append(errs, c.validateOIDC()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// loadOIDC reads the single sign-on settings. The client secret may come
// from a file (a mounted secret) instead of the environment.
func loadOIDC(getenv func(string) string) (OIDC, error) {
	env := func(name string) string { return strings.TrimSpace(getenv(name)) }
	o := OIDC{
		// Exactly as configured: the provider's discovery document must
		// name the same issuer, character for character, and some
		// providers' issuers end in a slash.
		Issuer:      env(EnvOIDCIssuer),
		ClientID:    env(EnvOIDCClientID),
		ButtonLabel: env(EnvOIDCButtonLabel),
		GroupsClaim: env(EnvOIDCGroupsClaim),
	}
	secret, secretFile := env(EnvOIDCClientSecret), env(EnvOIDCClientSecretFile)
	switch {
	case secret != "" && secretFile != "":
		return o, fmt.Errorf("set %s or %s, not both", EnvOIDCClientSecret, EnvOIDCClientSecretFile)
	case secretFile != "":
		raw, err := os.ReadFile(secretFile) //nolint:gosec // the path is the operator's own setting
		if err != nil {
			// The error names the file, never its contents.
			return o, fmt.Errorf("%s: %w", EnvOIDCClientSecretFile, err)
		}
		o.ClientSecret = strings.TrimSpace(string(raw))
		if o.ClientSecret == "" {
			return o, fmt.Errorf("%s: %s is empty", EnvOIDCClientSecretFile, secretFile)
		}
	default:
		o.ClientSecret = secret
	}
	scopes := env(EnvOIDCScopes)
	// Settings that mean nothing without an issuer: set alone, they are
	// half a configuration.
	partial := o.ClientID != "" || o.ClientSecret != "" || scopes != "" || o.ButtonLabel != "" ||
		o.GroupsClaim != "" || env(EnvOIDCAllowedGroups) != ""
	if scopes == "" {
		scopes = DefaultOIDCScopes
	}
	o.Scopes = splitList(scopes, " ,")
	if !slices.Contains(o.Scopes, "openid") {
		o.Scopes = append([]string{"openid"}, o.Scopes...)
	}
	if o.ButtonLabel == "" {
		o.ButtonLabel = DefaultOIDCButtonLabel
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = DefaultOIDCGroupsClaim
	}
	o.AllowedGroups = splitList(env(EnvOIDCAllowedGroups), ",")
	if o.Issuer == "" && partial {
		// Half a configuration is a mistake, not "off".
		return o, fmt.Errorf("%s is required to sign in with OIDC (other %s settings are set)", EnvOIDCIssuer, "PANELARR_OIDC_*")
	}
	return o, nil
}

// splitList splits s at any of seps, dropping empty items and duplicates.
func splitList(s, seps string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func (c Config) validateOIDC() []error {
	o := c.OIDC
	if !o.Enabled() {
		return nil
	}
	var errs []error
	u, err := url.Parse(o.Issuer)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		errs = append(errs, fmt.Errorf("%s: %q is not an http(s) URL", EnvOIDCIssuer, o.Issuer))
	case u.Scheme == "http" && !isLocalHost(u.Hostname()):
		errs = append(errs, fmt.Errorf("%s: %q must use https (plain http only for localhost)", EnvOIDCIssuer, o.Issuer))
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		errs = append(errs, fmt.Errorf("%s: %q must not have a query, fragment or user", EnvOIDCIssuer, o.Issuer))
	}
	if o.ClientID == "" {
		errs = append(errs, fmt.Errorf("%s is required with %s", EnvOIDCClientID, EnvOIDCIssuer))
	}
	if c.BaseURL == "" {
		errs = append(errs, fmt.Errorf("%s is required with %s: the provider redirects back to <%s>/api/v1/auth/oidc/callback", EnvBaseURL, EnvOIDCIssuer, EnvBaseURL))
	}
	return errs
}

// isLocalHost reports whether host is this machine: localhost or a
// loopback address.
func isLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
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
