// SPDX-License-Identifier: GPL-3.0-or-later

// Package plugins hosts source plugins. Plugins run as their own services
// (remote mode) and the server talks to them over the source plugin
// contract. The host keeps their settings, syncs the sources they list
// with their health, hands them their configuration and decides which
// sources each user may use.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors callers map to responses.
var (
	ErrNotFound     = errors.New("plugins: not found")
	ErrIDTaken      = errors.New("plugins: a plugin with this ID exists")
	ErrInvalidID    = errors.New("plugins: the ID must be lowercase letters, digits, '-' or '_' (up to 40)")
	ErrInvalidURL   = errors.New("plugins: the URL must be http(s)://host[:port]")
	ErrDisabled     = errors.New("plugins: the plugin is disabled")
	ErrNoConfig     = errors.New("plugins: the plugin has no configuration")
	ErrSourceAbsent = errors.New("plugins: the plugin no longer lists this source")
)

// UnreachableError is a plugin that couldn't be reached or doesn't speak
// the source contract.
type UnreachableError struct{ Reason string }

func (e *UnreachableError) Error() string { return "plugins: " + e.Reason }

// Dialer returns a client for a plugin at url.
type Dialer func(url, apiKey string) client.Client

// RemoteDialer speaks gRPC to plugins running as services.
func RemoteDialer(u, apiKey string) client.Client {
	return client.Remote(u, client.RemoteOptions{APIKey: apiKey})
}

// callTimeout bounds calls the host makes on its own (sync, config).
const callTimeout = 15 * time.Second

// Plugin is a registered plugin with its last known info.
type Plugin struct {
	db.Plugin
	Info        *sourcev1.PluginInfo
	SourceCount int64
}

// Host manages the plugins.
type Host struct {
	st   *store.Store
	box  *secret.Box
	log  *slog.Logger
	dial Dialer

	mu      sync.RWMutex
	clients map[string]client.Client
	limits  *limits.Registry

	// Recovery of plugins found down (recover.go).
	rmu        sync.Mutex
	bg         context.Context //nolint:containedctx // the host's lifetime, for rechecks
	recheck    []time.Duration
	grace      []time.Duration
	followUp   []time.Duration
	rechecking map[string]*chain
	hub        *events.Hub
	// sourcesChanged runs when sources become usable or stop being usable.
	sourcesChanged func(ctx context.Context, sourceIDs []string)
}

// SetHub makes the host tell admins about each plugin sync
// (plugin.health events).
func (h *Host) SetHub(hub *events.Hub) { h.hub = hub }

// SetLimits makes the host keep each source's limits (from its plugin's
// declared limits) in reg.
func (h *Host) SetLimits(reg *limits.Registry) { h.limits = reg }

// sourceLimit turns a plugin's declared limits into a source's cap.
func sourceLimit(info *sourcev1.PluginInfo) limits.Limit {
	l := limits.Default
	if c := info.GetLimits().GetMaxConcurrencyPerSource(); c > 0 {
		l.Concurrency = int(c)
	}
	if rl := info.GetLimits().GetDefaultRateLimit(); rl.GetRequests() > 0 && rl.GetPeriod().AsDuration() > 0 {
		l.Every = rl.GetPeriod().AsDuration() / time.Duration(rl.GetRequests())
		l.Burst = max(int(rl.GetBurst()), 1)
	}
	return l
}

// NewHost returns a host that reaches plugins with dial.
func NewHost(st *store.Store, box *secret.Box, dial Dialer, log *slog.Logger) *Host {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Host{
		st: st, box: box, dial: dial, log: log, clients: map[string]client.Client{},
		recheck: slices.Clone(DefaultRecheck), grace: slices.Clone(DefaultStartupGrace),
		followUp: slices.Clone(DefaultConfigFollowUp), rechecking: map[string]*chain{},
	}
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

func validURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidURL
	}
	return nil
}

func (h *Host) open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	return h.box.Open(sealed)
}

func (h *Host) seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return h.box.Seal(plain)
}

// probe checks that a plugin answers and speaks the source contract.
func probe(ctx context.Context, c client.Client) (*sourcev1.PluginInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	info, err := c.GetInfo(ctx)
	if err != nil {
		return nil, &UnreachableError{Reason: "the plugin didn't answer: " + errorMessage(err)}
	}
	if !slices.Contains(info.GetContracts(), plugin.Protocol) {
		return nil, &UnreachableError{Reason: fmt.Sprintf("the plugin doesn't offer %s (it offers %s)", plugin.Protocol, strings.Join(info.GetContracts(), ", "))}
	}
	return info, nil
}

// NewPlugin is a plugin to register.
type NewPlugin struct {
	// ID names this plugin instance; empty takes the plugin's own ID.
	ID     string
	URL    string
	APIKey string
}

// Register checks a plugin, stores it and syncs its sources.
func (h *Host) Register(ctx context.Context, n NewPlugin) (Plugin, error) {
	n.URL = strings.TrimRight(strings.TrimSpace(n.URL), "/")
	if err := validURL(n.URL); err != nil {
		return Plugin{}, err
	}
	c := h.dial(n.URL, n.APIKey)
	info, err := probe(ctx, c)
	if err != nil {
		_ = c.Close()
		return Plugin{}, err
	}
	if n.ID == "" {
		n.ID = strings.ToLower(info.GetId())
	}
	if !validID.MatchString(n.ID) {
		_ = c.Close()
		return Plugin{}, ErrInvalidID
	}
	key, err := h.seal(n.APIKey)
	if err != nil {
		_ = c.Close()
		return Plugin{}, err
	}
	raw, _ := protojson.Marshal(info)
	_, err = h.st.CreatePlugin(ctx, db.CreatePluginParams{ID: n.ID, Url: n.URL, ApiKey: key, Info: raw, Health: "ok"})
	if isUniqueViolation(err) {
		_ = c.Close()
		return Plugin{}, ErrIDTaken
	}
	if err != nil {
		_ = c.Close()
		return Plugin{}, err
	}
	h.setClient(n.ID, c)
	h.log.InfoContext(ctx, "plugin registered", "component", "audit", "plugin", n.ID, "url", n.URL)
	return h.Sync(ctx, n.ID)
}

func (h *Host) setClient(id string, c client.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.clients[id]; old != nil {
		_ = old.Close()
	}
	if c == nil {
		delete(h.clients, id)
		return
	}
	h.clients[id] = c
}

// Load connects to every stored plugin, hands each its configuration and
// syncs its sources. Failures are recorded, not fatal. ctx is the host's
// lifetime: rechecks of plugins found down run until it ends.
func (h *Host) Load(ctx context.Context) error {
	h.rmu.Lock()
	h.bg = ctx
	h.rmu.Unlock()
	rows, err := h.st.ListPlugins(ctx)
	if err != nil {
		return err
	}
	for _, p := range rows {
		key, err := h.open(p.ApiKey)
		if err != nil {
			h.log.ErrorContext(ctx, "plugin key", "plugin", p.ID, "err", err)
			continue
		}
		h.setClient(p.ID, h.dial(p.Url, key))
		if !p.Enabled {
			continue
		}
		h.awaitReachable(ctx, p.ID)
		if err := h.pushConfig(ctx, p); err != nil {
			h.log.WarnContext(ctx, "plugin config", "plugin", p.ID, "err", err)
		}
		if _, err := h.Sync(ctx, p.ID); err != nil {
			h.log.WarnContext(ctx, "plugin sync", "plugin", p.ID, "err", err)
		}
	}
	return nil
}

// Client returns the client of an enabled plugin.
func (h *Host) Client(ctx context.Context, id string) (client.Client, error) {
	p, err := h.st.GetPlugin(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, ErrDisabled
	}
	h.mu.RLock()
	c := h.clients[id]
	h.mu.RUnlock()
	if c == nil {
		return nil, ErrNotFound
	}
	return c, nil
}

func (h *Host) decode(p db.Plugin, counts map[string]int64) Plugin {
	out := Plugin{Plugin: p, Info: &sourcev1.PluginInfo{}, SourceCount: counts[p.ID]}
	_ = protojson.Unmarshal(p.Info, out.Info)
	return out
}

func (h *Host) counts(ctx context.Context) (map[string]int64, error) {
	rows, err := h.st.CountSourcesByPlugin(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.PluginID] = r.Listed
	}
	return out, nil
}

// List returns every plugin.
func (h *Host) List(ctx context.Context) ([]Plugin, error) {
	rows, err := h.st.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := h.counts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Plugin, len(rows))
	for i, p := range rows {
		out[i] = h.decode(p, counts)
	}
	return out, nil
}

// Get returns one plugin.
func (h *Host) Get(ctx context.Context, id string) (Plugin, error) {
	p, err := h.st.GetPlugin(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrNotFound
	}
	if err != nil {
		return Plugin{}, err
	}
	counts, err := h.counts(ctx)
	if err != nil {
		return Plugin{}, err
	}
	return h.decode(p, counts), nil
}

// Patch changes a plugin; nil fields stay. An empty APIKey removes it.
type Patch struct {
	URL     *string
	APIKey  *string
	Enabled *bool
}

// Update changes a plugin. A new address or key is checked first.
func (h *Host) Update(ctx context.Context, id string, p Patch) (Plugin, error) {
	cur, err := h.st.GetPlugin(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plugin{}, ErrNotFound
	}
	if err != nil {
		return Plugin{}, err
	}
	u := cur.Url
	if p.URL != nil {
		u = strings.TrimRight(strings.TrimSpace(*p.URL), "/")
		if err := validURL(u); err != nil {
			return Plugin{}, err
		}
	}
	key, err := h.open(cur.ApiKey)
	if err != nil {
		return Plugin{}, err
	}
	if p.APIKey != nil {
		key = *p.APIKey
	}
	enabled := cur.Enabled
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	c := h.dial(u, key)
	if enabled && (p.URL != nil || p.APIKey != nil) {
		if _, err := probe(ctx, c); err != nil {
			_ = c.Close()
			return Plugin{}, err
		}
	}
	sealed, err := h.seal(key)
	if err != nil {
		_ = c.Close()
		return Plugin{}, err
	}
	updated, err := h.st.UpdatePlugin(ctx, db.UpdatePluginParams{ID: id, Url: u, ApiKey: sealed, Enabled: enabled})
	if err != nil {
		_ = c.Close()
		return Plugin{}, err
	}
	h.setClient(id, c)
	h.log.InfoContext(ctx, "plugin updated", "component", "audit", "plugin", id, "enabled", enabled)
	if enabled != cur.Enabled {
		if err := h.changedSince(ctx, id, nil); err != nil {
			return Plugin{}, err
		}
	}
	if enabled && (!cur.Enabled || p.URL != nil || p.APIKey != nil) {
		if err := h.pushConfig(ctx, updated); err != nil {
			h.log.WarnContext(ctx, "plugin config", "plugin", id, "err", err)
		}
		return h.Sync(ctx, id)
	}
	return h.Get(ctx, id)
}

// Delete removes a plugin and its sources.
func (h *Host) Delete(ctx context.Context, id string) error {
	n, err := h.st.DeletePlugin(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	h.setClient(id, nil)
	h.log.InfoContext(ctx, "plugin deleted", "component", "audit", "plugin", id)
	return nil
}

// Sync refreshes a plugin's info, health and sources. A plugin that
// doesn't answer is recorded as down (its sources stay as they were) and
// rechecked soon after.
func (h *Host) Sync(ctx context.Context, id string) (Plugin, error) {
	p, err := h.sync(ctx, id)
	if err == nil && p.Health == "down" {
		h.scheduleRecheck(id)
	}
	return p, err
}

func (h *Host) sync(ctx context.Context, id string) (Plugin, error) {
	c, err := h.Client(ctx, id)
	if err != nil {
		return Plugin{}, err
	}
	cur, err := h.st.GetPlugin(ctx, id)
	if err != nil {
		return Plugin{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	state := db.SetPluginStateParams{ID: id, Info: cur.Info, Health: "ok"}
	info, err := c.GetInfo(ctx)
	if err != nil {
		state.Health, state.HealthMessage = "down", errorMessage(err)
		return h.saveState(ctx, cur, state)
	}
	state.Info, _ = protojson.Marshal(info)
	if hr, err := c.Health(ctx); err != nil {
		state.Health, state.HealthMessage = "down", errorMessage(err)
	} else if hr.GetStatus() != sourcev1.HealthStatus_HEALTH_STATUS_SERVING {
		state.Health, state.HealthMessage = "degraded", hr.GetMessage()
	}
	res, err := c.ListSources(ctx, &sourcev1.ListSourcesRequest{})
	if err != nil {
		state.Health, state.HealthMessage = "down", "listing sources failed: "+errorMessage(err)
		return h.saveState(ctx, cur, state)
	}
	before, err := h.usable(ctx, id)
	if err != nil {
		return Plugin{}, err
	}
	err = h.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		listed := make([]string, 0, len(res.GetSources()))
		for _, s := range res.GetSources() {
			listed = append(listed, s.GetId())
			if err := q.UpsertSource(ctx, sourceParams(id, s)); err != nil {
				return err
			}
		}
		return q.MarkSourcesAbsent(ctx, db.MarkSourcesAbsentParams{PluginID: id, Listed: listed})
	})
	if err != nil {
		return Plugin{}, err
	}
	if err := h.changedSince(ctx, id, before); err != nil {
		return Plugin{}, err
	}
	if h.limits != nil {
		l := sourceLimit(info)
		for _, s := range res.GetSources() {
			h.limits.Set("source:"+id+":"+s.GetId(), l)
		}
	}
	return h.saveState(ctx, cur, state)
}

func (h *Host) saveState(ctx context.Context, before db.Plugin, state db.SetPluginStateParams) (Plugin, error) {
	p, err := h.st.SetPluginState(ctx, state)
	if err != nil {
		return Plugin{}, err
	}
	if before.Health != p.Health {
		h.log.InfoContext(ctx, "plugin health changed", "plugin", p.ID, "from", before.Health, "to", p.Health, "message", p.HealthMessage)
		// A plugin coming back may have restarted without its settings.
		if before.Health == "down" && p.Health != "down" {
			if err := h.pushConfig(ctx, p); err != nil {
				h.log.WarnContext(ctx, "plugin config", "plugin", p.ID, "err", err)
			}
		}
	}
	counts, err := h.counts(ctx)
	if err != nil {
		return Plugin{}, err
	}
	// Every sync may change the plugin's or its sources' health: open
	// admin pages refetch.
	h.hub.Publish("plugin.health", map[string]any{"plugin_id": p.ID, "health": p.Health, "health_message": p.HealthMessage},
		events.Audience{Admins: true})
	return h.decode(p, counts), nil
}

func sourceParams(pluginID string, s *sourcev1.Source) db.UpsertSourceParams {
	caps := make([]string, 0, len(s.GetCapabilities()))
	for _, c := range s.GetCapabilities() {
		if name := enumName(c.String(), "SOURCE_CAPABILITY_"); name != "" {
			caps = append(caps, name)
		}
	}
	p := db.UpsertSourceParams{
		ID: pluginID + ":" + s.GetId(), PluginID: pluginID, SourceID: s.GetId(),
		Name: s.GetName(), Lang: s.GetLang(), ContentRating: sourceRating(s.GetContentRating()),
		IconUrl: s.GetIconUrl(), HomeUrl: s.GetHomeUrl(), Capabilities: caps, Health: "unknown",
	}
	if hl := s.GetHealth(); hl != nil {
		switch hl.GetStatus() {
		case sourcev1.SourceHealthStatus_SOURCE_HEALTH_STATUS_OK:
			p.Health = "ok"
		case sourcev1.SourceHealthStatus_SOURCE_HEALTH_STATUS_FAILING:
			p.Health = "failing"
		}
		p.HealthReason, p.HealthMessage = hl.GetReason(), hl.GetMessage()
		if hl.GetCheckedAt() != nil {
			t := hl.GetCheckedAt().AsTime()
			p.HealthCheckedAt = &t
		}
	}
	return p
}

func sourceRating(r sourcev1.ContentRating) string {
	switch r {
	case sourcev1.ContentRating_CONTENT_RATING_SAFE:
		return "safe"
	case sourcev1.ContentRating_CONTENT_RATING_MIXED:
		return "mixed"
	case sourcev1.ContentRating_CONTENT_RATING_NSFW:
		return "nsfw"
	}
	return "unknown"
}

func enumName(s, prefix string) string {
	s = strings.ToLower(strings.TrimPrefix(s, prefix))
	if s == "unspecified" {
		return ""
	}
	return s
}

// SyncAll syncs every enabled plugin and reports how many are ok and down.
func (h *Host) SyncAll(ctx context.Context) (ok, down int, err error) {
	rows, err := h.st.ListPlugins(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, p := range rows {
		if !p.Enabled {
			continue
		}
		synced, err := h.Sync(ctx, p.ID)
		if err != nil {
			return ok, down, err
		}
		if synced.Health == "down" {
			down++
		} else {
			ok++
		}
	}
	return ok, down, nil
}

// Config is a plugin's configuration: its schema and current values.
type Config struct {
	Schema *structpb.Struct
	Values map[string]any
}

// Config returns a plugin's configuration as the plugin reports it
// (secrets left empty).
func (h *Host) Config(ctx context.Context, id string) (Config, error) {
	p, err := h.Get(ctx, id)
	if err != nil {
		return Config{}, err
	}
	schema := p.Info.GetConfigSchema()
	if schema == nil || len(schema.GetFields()) == 0 {
		return Config{}, ErrNoConfig
	}
	c, err := h.Client(ctx, id)
	if err != nil {
		return Config{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	values, err := c.GetConfig(ctx)
	if err != nil {
		return Config{}, err
	}
	return Config{Schema: schema, Values: values.AsMap()}, nil
}

// SetConfig validates values with the plugin, applies them and stores them
// (encrypted) to hand back whenever the plugin restarts. Secret fields
// (writeOnly in the schema) left empty keep their stored value.
func (h *Host) SetConfig(ctx context.Context, id string, values map[string]any) (Config, error) {
	p, err := h.Get(ctx, id)
	if err != nil {
		return Config{}, err
	}
	schema := p.Info.GetConfigSchema()
	if schema == nil || len(schema.GetFields()) == 0 {
		return Config{}, ErrNoConfig
	}
	stored, err := h.storedConfig(p.Plugin)
	if err != nil {
		return Config{}, err
	}
	merged := maps.Clone(values)
	for _, k := range writeOnlyFields(schema) {
		if v, ok := merged[k]; (!ok || v == nil || v == "") && stored[k] != nil {
			merged[k] = stored[k]
		}
	}
	st, err := structpb.NewStruct(merged)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrBadConfig, err)
	}
	c, err := h.Client(ctx, id)
	if err != nil {
		return Config{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	shown, err := c.SetConfig(ctx, st)
	if err != nil {
		return Config{}, err
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return Config{}, err
	}
	sealed, err := h.box.Seal(string(raw))
	if err != nil {
		return Config{}, err
	}
	if err := h.st.SetPluginConfig(ctx, db.SetPluginConfigParams{ID: id, Config: sealed}); err != nil {
		return Config{}, err
	}
	h.log.InfoContext(ctx, "plugin config changed", "component", "audit", "plugin", id)
	// The plugin applies the settings in the background; its health tells
	// how that went.
	h.scheduleFollowUp(id)
	return Config{Schema: schema, Values: shown.AsMap()}, nil
}

// ErrBadConfig is configuration the server can't even encode.
var ErrBadConfig = errors.New("plugins: invalid configuration")

func (h *Host) storedConfig(p db.Plugin) (map[string]any, error) {
	if p.Config == "" {
		return map[string]any{}, nil
	}
	plain, err := h.box.Open(p.Config)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(plain), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// pushConfig hands a plugin its stored configuration.
func (h *Host) pushConfig(ctx context.Context, p db.Plugin) error {
	stored, err := h.storedConfig(p)
	if err != nil || len(stored) == 0 {
		return err
	}
	c, err := h.Client(ctx, p.ID)
	if err != nil {
		return err
	}
	st, err := structpb.NewStruct(stored)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err = c.SetConfig(ctx, st)
	return err
}

// writeOnlyFields lists the top-level properties a JSON Schema marks
// writeOnly (secrets the plugin never returns).
func writeOnlyFields(schema *structpb.Struct) []string {
	props := schema.GetFields()["properties"].GetStructValue().GetFields()
	var out []string
	for name, v := range props {
		if v.GetStructValue().GetFields()["writeOnly"].GetBoolValue() {
			out = append(out, name)
		}
	}
	return out
}

// errorMessage shortens a plugin error for people.
func errorMessage(err error) string {
	var pe *plugin.Error
	if errors.As(err, &pe) && pe.Message != "" {
		return pe.Message
	}
	return err.Error()
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ReadyCheck reports enabled plugins that are down, from their last sync
// (it calls no plugin). Plugins are optional: this only marks the server
// degraded.
func (h *Host) ReadyCheck(ctx context.Context) error {
	rows, err := h.st.ListPlugins(ctx)
	if err != nil {
		return err
	}
	var down []string
	for _, p := range rows {
		if p.Enabled && p.Health == "down" {
			down = append(down, p.ID+" ("+p.HealthMessage+")")
		}
	}
	if len(down) > 0 {
		return errors.New("plugins down: " + strings.Join(down, ", "))
	}
	return nil
}
