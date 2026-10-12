// SPDX-License-Identifier: GPL-3.0-or-later

package network

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/panelarr/server/internal/store/db"
)

// Owner kinds of custom profiles (and assignment target kinds).
const (
	KindGlobal   = "global"
	KindPlugin   = "plugin"
	KindPackage  = "package"
	KindSource   = "source"
	KindMeta     = "meta"
	KindProvider = "provider"
)

// User agent modes.
const (
	UAPluginDefault = "default"
	UAFixed         = "fixed"
	UARandomDesktop = "random_desktop"
	UAFromSolver    = "from_solver"
)

// ErrNotShared is a custom profile assigned elsewhere than to its owner.
var ErrNotShared = errors.New("network: a custom profile can only be assigned to its owner")

// Owner is who a custom profile belongs to.
type Owner struct{ Kind, ID string }

// RateLimit is a profile's request rate.
type RateLimit struct {
	Requests int
	Period   time.Duration
	Burst    int
	// Scope is "source" (one budget per source) or "host" (per target host).
	Scope string
}

// Timeouts bound a plugin's requests; zero means the plugin's default.
type Timeouts struct{ Connect, Read, Call time.Duration }

// Profile is a network profile: a proxy, a solver and how requests are
// made. Unset parts mean the plugin's own defaults.
type Profile struct {
	ID, Name          string
	ProxyID, SolverID string
	RateLimit         *RateLimit
	Concurrency       int
	UserAgentMode     string
	UserAgent         string
	DoHURL            string
	Headers           map[string]string
	SharedCookieJar   string
	Timeouts          Timeouts
	// Owner is set on custom profiles.
	Owner                *Owner
	CreatedAt, UpdatedAt time.Time
}

func ms(d time.Duration) int32 { return int32(d / time.Millisecond) } //nolint:gosec // bounded by the API

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func profileOf(r db.NetworkProfile) Profile {
	p := Profile{
		ID: r.ID, Name: r.Name, ProxyID: r.ProxyID.String, SolverID: r.SolverID.String, Concurrency: int(r.Concurrency),
		UserAgentMode: r.UserAgentMode, UserAgent: r.UserAgent, DoHURL: r.DohUrl, SharedCookieJar: r.SharedCookieJar,
		Timeouts: Timeouts{
			Connect: time.Duration(r.ConnectTimeoutMs) * time.Millisecond,
			Read:    time.Duration(r.ReadTimeoutMs) * time.Millisecond,
			Call:    time.Duration(r.CallTimeoutMs) * time.Millisecond,
		},
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.RateRequests > 0 {
		p.RateLimit = &RateLimit{Requests: int(r.RateRequests), Period: time.Duration(r.RatePeriodMs) * time.Millisecond, Burst: int(r.RateBurst), Scope: r.RateScope}
	}
	_ = json.Unmarshal(r.Headers, &p.Headers)
	if p.Headers == nil {
		p.Headers = map[string]string{}
	}
	if r.OwnerKind != "" {
		p.Owner = &Owner{Kind: r.OwnerKind, ID: r.OwnerID}
	}
	return p
}

// scope is where custom items may be used: "" for shared ones,
// "plugin:<id>" for a plugin's (and its extensions' and sources'),
// "provider:<id>" for a metadata provider's.
func scope(kind, id string, plugin pgtype.Text) string {
	switch {
	case kind == KindProvider:
		return "provider:" + id
	case plugin.Valid:
		return "plugin:" + plugin.String
	}
	return ""
}

// checkProfile checks a profile's fields and that its proxy and solver
// exist and may be used there: shared ones anywhere, custom ones only in
// their owner's scope (in).
func (s *Service) checkProfile(ctx context.Context, p Profile, in string) error {
	if strings.TrimSpace(p.Name) == "" {
		return invalid("name", "give it a name")
	}
	if p.ProxyID != "" {
		r, err := s.st.GetProxy(ctx, p.ProxyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("proxy_id", "no such proxy")
		} else if err != nil {
			return err
		}
		if sc := scope(r.OwnerKind, r.OwnerID, r.OwnerPlugin); sc != "" && sc != in {
			return invalid("proxy_id", "a custom proxy of another owner")
		}
	}
	if p.SolverID != "" {
		r, err := s.st.GetSolver(ctx, p.SolverID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("solver_id", "no such solver")
		} else if err != nil {
			return err
		}
		if sc := scope(r.OwnerKind, r.OwnerID, r.OwnerPlugin); sc != "" && sc != in {
			return invalid("solver_id", "a custom solver of another owner")
		}
	}
	if rl := p.RateLimit; rl != nil {
		if rl.Requests <= 0 || rl.Period <= 0 {
			return invalid("rate_limit", "give requests and a period")
		}
		if rl.Scope != "source" && rl.Scope != "host" {
			return invalid("rate_limit.scope", "source or host")
		}
	}
	switch p.UserAgentMode {
	case UAPluginDefault, UARandomDesktop:
	case UAFixed:
		if strings.TrimSpace(p.UserAgent) == "" {
			return invalid("user_agent.value", "give the user agent")
		}
	case UAFromSolver:
		if p.SolverID == "" {
			return invalid("user_agent.mode", "from_solver needs a solver")
		}
	default:
		return invalid("user_agent.mode", "default, fixed, random_desktop or from_solver")
	}
	if p.DoHURL != "" {
		if err := checkHTTPURL(p.DoHURL); err != nil {
			return invalid("doh_url", err.Error())
		}
	}
	for k := range p.Headers {
		if k == "" || http.CanonicalHeaderKey(k) == "" || strings.ContainsAny(k, " \r\n:") {
			return invalid("headers", "bad header name "+k)
		}
	}
	return nil
}

// ownerPlugin is the plugin a custom profile's owner belongs to, so the
// profile goes with it; empty for providers.
func (s *Service) ownerPlugin(ctx context.Context, o *Owner) (string, error) {
	if o == nil {
		return "", nil
	}
	if o.ID == "" {
		return "", invalid("owner.id", "say whose profile it is")
	}
	switch o.Kind {
	case KindPlugin, KindPackage, KindSource:
		return s.pluginOf(ctx, o.Kind, o.ID)
	case KindProvider:
		return "", nil
	}
	return "", invalid("owner.kind", "plugin, package, source or provider")
}

// pluginOf checks a plugin, package or source target exists and returns
// its plugin.
func (s *Service) pluginOf(ctx context.Context, kind, id string) (string, error) {
	plugin := id
	switch kind {
	case KindPackage:
		p, pkg, ok := strings.Cut(id, ":")
		if !ok || pkg == "" {
			return "", invalid("target_id", `a package is "plugin:package"`)
		}
		plugin = p
	case KindSource:
		src, err := s.st.GetSource(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		return src.Source.PluginID, nil
	}
	if _, err := s.st.GetPlugin(ctx, plugin); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	return plugin, nil
}

func profileParams(p Profile) (db.InsertProfileParams, error) {
	headers, err := json.Marshal(p.Headers)
	if err != nil {
		return db.InsertProfileParams{}, err
	}
	if p.Headers == nil {
		headers = []byte("{}")
	}
	out := db.InsertProfileParams{
		ID: p.ID, Name: strings.TrimSpace(p.Name), ProxyID: text(p.ProxyID), SolverID: text(p.SolverID),
		RateScope: "source", Concurrency: int32(p.Concurrency), //nolint:gosec // bounded by the API
		UserAgentMode: p.UserAgentMode, UserAgent: p.UserAgent, DohUrl: p.DoHURL, Headers: headers,
		SharedCookieJar: p.SharedCookieJar, ConnectTimeoutMs: ms(p.Timeouts.Connect), ReadTimeoutMs: ms(p.Timeouts.Read),
		CallTimeoutMs: ms(p.Timeouts.Call),
	}
	if out.UserAgentMode == "" {
		out.UserAgentMode = UAPluginDefault
	}
	if rl := p.RateLimit; rl != nil {
		out.RateRequests, out.RatePeriodMs, out.RateBurst, out.RateScope = int32(rl.Requests), ms(rl.Period), int32(rl.Burst), rl.Scope //nolint:gosec // bounded by the API
	}
	if p.Owner != nil {
		out.OwnerKind, out.OwnerID = p.Owner.Kind, p.Owner.ID
	}
	return out, nil
}

// Profiles lists shared profiles, and custom ones when withCustom; owner
// narrows the list to one owner's custom profiles.
func (s *Service) Profiles(ctx context.Context, owner *Owner, withCustom bool) ([]Profile, error) {
	rows, err := s.st.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := []Profile{}
	for _, r := range rows {
		switch {
		case owner != nil && (r.OwnerKind != owner.Kind || r.OwnerID != owner.ID):
		case owner == nil && r.OwnerKind != "" && !withCustom:
		default:
			out = append(out, profileOf(r))
		}
	}
	return out, nil
}

// Profile returns one profile.
func (s *Service) Profile(ctx context.Context, id string) (Profile, error) {
	r, err := s.st.GetProfile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, err
	}
	return profileOf(r), nil
}

// CreateProfile adds a shared profile, or a custom one when p.Owner is set.
func (s *Service) CreateProfile(ctx context.Context, p Profile) (Profile, error) {
	if !idPattern.MatchString(p.ID) {
		return Profile{}, ErrInvalidID
	}
	if p.UserAgentMode == "" {
		p.UserAgentMode = UAPluginDefault
	}
	plugin, err := s.ownerPlugin(ctx, p.Owner)
	if err != nil {
		return Profile{}, err
	}
	if err := s.checkProfile(ctx, p, profileScope(p.Owner, plugin)); err != nil {
		return Profile{}, err
	}
	params, err := profileParams(p)
	if err != nil {
		return Profile{}, err
	}
	params.OwnerPlugin = text(plugin)
	r, err := s.st.InsertProfile(ctx, params)
	if isUniqueViolation(err) {
		return Profile{}, ErrIDTaken
	}
	if err != nil {
		return Profile{}, err
	}
	s.log.InfoContext(ctx, "network profile added", "component", "audit", "profile", p.ID)
	return profileOf(r), nil
}

// ReplaceProfile replaces a profile's settings (not its ID or owner).
func (s *Service) ReplaceProfile(ctx context.Context, id string, p Profile) (Profile, error) {
	cur, err := s.Profile(ctx, id)
	if err != nil {
		return Profile{}, err
	}
	p.ID, p.Owner = id, cur.Owner
	if p.UserAgentMode == "" {
		p.UserAgentMode = UAPluginDefault
	}
	row, err := s.st.GetProfile(ctx, id)
	if err != nil {
		return Profile{}, err
	}
	if err := s.checkProfile(ctx, p, scope(row.OwnerKind, row.OwnerID, row.OwnerPlugin)); err != nil {
		return Profile{}, err
	}
	in, err := profileParams(p)
	if err != nil {
		return Profile{}, err
	}
	r, err := s.st.UpdateProfile(ctx, db.UpdateProfileParams{
		ID: id, Name: in.Name, ProxyID: in.ProxyID, SolverID: in.SolverID, RateRequests: in.RateRequests,
		RatePeriodMs: in.RatePeriodMs, RateBurst: in.RateBurst, RateScope: in.RateScope, Concurrency: in.Concurrency,
		UserAgentMode: in.UserAgentMode, UserAgent: in.UserAgent, DohUrl: in.DohUrl, Headers: in.Headers,
		SharedCookieJar: in.SharedCookieJar, ConnectTimeoutMs: in.ConnectTimeoutMs, ReadTimeoutMs: in.ReadTimeoutMs,
		CallTimeoutMs: in.CallTimeoutMs,
	})
	if err != nil {
		return Profile{}, err
	}
	s.log.InfoContext(ctx, "network profile changed", "component", "audit", "profile", id)
	s.changed(ctx)
	return profileOf(r), nil
}

// DeleteProfile removes a profile nothing is assigned.
func (s *Service) DeleteProfile(ctx context.Context, id string) error {
	n, err := s.st.DeleteProfile(ctx, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.log.InfoContext(ctx, "network profile removed", "component", "audit", "profile", id)
	return nil
}

// profileScope is the scope of a profile being created.
func profileScope(o *Owner, plugin string) string {
	if o == nil {
		return ""
	}
	return scope(o.Kind, o.ID, text(plugin))
}
