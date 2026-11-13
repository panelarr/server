// SPDX-License-Identifier: GPL-3.0-or-later

// Package sso signs users in with an OpenID Connect provider: the
// authorization code flow with PKCE (S256), state and nonce, and a
// verified ID token. It finds out who the provider says the user is;
// which Panelarr user that is, is up to the caller (by issuer and
// subject, never by name or email).
//
// The flow keeps no server-side state: its secrets (state, nonce, the PKCE
// verifier, where to return) travel in a short-lived cookie the server
// encrypts and authenticates with its secret key. Nonces are remembered
// until their flow expires, so a flow can't be finished twice.
package sso

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// Errors of a flow. Each maps to a code the login page explains.
var (
	// ErrUnavailable: the provider's discovery document couldn't be
	// fetched (yet).
	ErrUnavailable = errors.New("sso: the identity provider is unavailable")
	// ErrFlowExpired: no flow cookie, a cookie that doesn't open, or one
	// that expired.
	ErrFlowExpired = errors.New("sso: no valid sign-in flow")
	// ErrStateMismatch: the state the provider sent back isn't the flow's.
	ErrStateMismatch = errors.New("sso: state mismatch")
	// ErrNonceReused: this flow was finished before.
	ErrNonceReused = errors.New("sso: flow already used")
	// ErrProvider: the provider refused or failed to exchange the code.
	ErrProvider = errors.New("sso: the identity provider refused the sign-in")
	// ErrInvalidToken: the ID token failed verification (signature,
	// issuer, audience, expiry, nonce).
	ErrInvalidToken = errors.New("sso: invalid ID token")
	// ErrNotAllowed: the user isn't in an allowed group.
	ErrNotAllowed = errors.New("sso: not in an allowed group")
)

// Timing.
const (
	// FlowTTL is how long a started sign-in may take.
	FlowTTL = 10 * time.Minute
	// clockSkew is how far the provider's clock may be ahead of ours
	// when checking the ID token's expiry.
	clockSkew = 2 * time.Minute
	// Discovery retries back off from retryMin to retryMax.
	retryMin = 5 * time.Second
	retryMax = 5 * time.Minute
	// requestTimeout bounds each call to the provider.
	requestTimeout = 15 * time.Second
)

// Config configures a provider.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	// RedirectURL is <base URL>/api/v1/auth/oidc/callback.
	RedirectURL string
	// Label is the sign-in button's label.
	Label string
	// GroupsClaim names the claim listing the user's groups.
	GroupsClaim string
	// AllowedGroups, when set, lets only their members sign in.
	AllowedGroups []string
	// UsernameClaim names the claim a new user's username comes from.
	UsernameClaim string
	// AdminGroups, when set, decides linked users' roles on sign-in.
	AdminGroups []string
	// AutoProvision creates users for unknown logins.
	AutoProvision bool
	// PasswordAdminsOnly limits password login to admins.
	PasswordAdminsOnly bool
	// ProviderLogout also ends the session at the provider on logout
	// (RP-initiated logout), returning to PostLogoutRedirectURL.
	ProviderLogout        bool
	PostLogoutRedirectURL string
}

// Sealer encrypts the flow cookie (secret.Box).
type Sealer interface {
	SealFor(purpose, plain string) (string, error)
	OpenFor(purpose, sealed string) (string, error)
}

// cookiePurpose binds sealed flow cookies to this use.
const cookiePurpose = "panelarr.oidc.flow.v1"

// Provider is one configured OpenID Connect provider.
type Provider struct {
	cfg    Config
	sealer Sealer
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	fetches singleflight.Group

	mu        sync.Mutex
	disc      *discovered
	lastErr   error
	lastTry   time.Time
	retryWait time.Duration

	nonces *nonceSet
}

type discovered struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// New returns a provider. Nothing is fetched until it is used or Run
// starts. client is used for every call to the provider (nil: a default
// client with timeouts).
func New(cfg Config, sealer Sealer, client *http.Client, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.UsernameClaim == "" {
		cfg.UsernameClaim = "preferred_username"
	}
	return &Provider{cfg: cfg, sealer: sealer, client: client, log: log, now: time.Now, retryWait: retryMin, nonces: newNonceSet()}
}

// Label is the sign-in button's label.
func (p *Provider) Label() string { return p.cfg.Label }

// Issuer is the provider's issuer, which with a subject identifies a
// user.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// Settings returns the provider's configuration without the client
// secret, for the admins' status page and the sign-in rules.
func (p *Provider) Settings() Config {
	c := p.cfg
	c.ClientSecret = ""
	c.Scopes = slices.Clone(c.Scopes)
	c.AllowedGroups = slices.Clone(c.AllowedGroups)
	c.AdminGroups = slices.Clone(c.AdminGroups)
	return c
}

// PublicClient reports whether the client has no secret (PKCE only).
func (p *Provider) PublicClient() bool { return p.cfg.ClientSecret == "" }

// LogoutURL is where to send the browser to end its session at the
// provider too (RP-initiated logout), with the client ID and the
// post-logout redirect. ok is false when provider logout is off or the
// provider has no end-session endpoint (or can't be reached): then only
// Panelarr's session ends.
func (p *Provider) LogoutURL(ctx context.Context) (string, bool) {
	if !p.cfg.ProviderLogout {
		return "", false
	}
	d, err := p.discover(ctx, false)
	if err != nil {
		return "", false
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := d.provider.Claims(&meta); err != nil || meta.EndSession == "" {
		return "", false
	}
	u, err := url.Parse(meta.EndSession)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false
	}
	q := u.Query()
	q.Set("client_id", p.cfg.ClientID)
	if p.cfg.PostLogoutRedirectURL != "" {
		q.Set("post_logout_redirect_uri", p.cfg.PostLogoutRedirectURL)
	}
	u.RawQuery = q.Encode()
	return u.String(), true
}

// Run fetches the discovery document, retrying with a backoff until it
// succeeds or ctx ends, so a provider that is down at startup doesn't stop
// the server.
func (p *Provider) Run(ctx context.Context) {
	wait := retryMin
	for {
		_, err := p.discover(ctx, true)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, retryMax)
	}
}

// Check reports whether the provider can be used (for the health page).
func (p *Provider) Check(ctx context.Context) error {
	_, err := p.discover(ctx, false)
	return err
}

// discover returns the discovery result, fetching it when there is none.
// Fetching again after a failure waits for the backoff, unless force, so
// sign-in attempts and health checks can't hammer a provider that's down.
// A successful result is kept: go-oidc refreshes the signing keys itself
// when it meets an unknown key. The fetch runs outside p.mu, and callers
// arriving during it share it.
func (p *Provider) discover(ctx context.Context, force bool) (*discovered, error) {
	p.mu.Lock()
	if p.disc != nil {
		d := p.disc
		p.mu.Unlock()
		return d, nil
	}
	if !force && p.lastErr != nil && p.now().Sub(p.lastTry) < p.retryWait {
		err := p.lastErr
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()
	// The shared fetch must outlive a caller that gives up, so it gets
	// its own deadline instead of the caller's context.
	ch := p.fetches.DoChan("discover", func() (any, error) {
		return p.fetch(context.WithoutCancel(ctx))
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*discovered), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
	}
}

// fetch reads the discovery document and records the outcome.
func (p *Provider) fetch(ctx context.Context) (*discovered, error) {
	p.mu.Lock()
	if p.disc != nil {
		d := p.disc
		p.mu.Unlock()
		return d, nil
	}
	p.lastTry = p.now()
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, p.client), requestTimeout)
	defer cancel()
	prov, err := oidc.NewProvider(ctx, p.cfg.Issuer)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if p.lastErr == nil {
			p.log.WarnContext(ctx, "OIDC discovery failed; retrying", "issuer", p.cfg.Issuer, "err", err)
		}
		p.lastErr = fmt.Errorf("%w: %w", ErrUnavailable, err)
		p.retryWait = min(max(p.retryWait*2, retryMin), retryMax)
		return nil, p.lastErr
	}
	if p.lastErr != nil {
		p.log.InfoContext(ctx, "OIDC discovery succeeded", "issuer", p.cfg.Issuer)
	}
	p.lastErr, p.retryWait = nil, retryMin
	p.disc = &discovered{
		provider: prov,
		// The verifier fetches the signing keys with p.client too.
		verifier: prov.Verifier(&oidc.Config{
			ClientID: p.cfg.ClientID,
			// The token may come from a provider whose clock runs a
			// little ahead: allow for it when checking the expiry.
			Now: func() time.Time { return p.now().Add(-clockSkew) },
		}),
		oauth: &oauth2.Config{
			ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret,
			Endpoint: prov.Endpoint(), RedirectURL: p.cfg.RedirectURL, Scopes: p.cfg.Scopes,
		},
	}
	return p.disc, nil
}

// flow is what the flow cookie carries.
type flow struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	ReturnTo string    `json:"r"`
	Expires  time.Time `json:"e"`
	// What the flow is for (Start).
	Intent    string `json:"i,omitempty"`
	Invite    string `json:"t,omitempty"`
	UserID    string `json:"u,omitempty"`
	SessionID string `json:"x,omitempty"`
}

// Intents of a flow.
const (
	// IntentLogin signs in (or provisions a user).
	IntentLogin = "login"
	// IntentLink links the login to the signed-in user.
	IntentLink = "link"
	// IntentInvite creates the user an invite allows.
	IntentInvite = "invite"
)

// Start describes a sign-in to begin.
type Start struct {
	// ReturnTo must already be checked to be a local path.
	ReturnTo string
	// Intent is IntentLogin (default), IntentLink or IntentInvite.
	Intent string
	// Invite is the invite token (IntentInvite). It travels sealed in
	// the flow cookie, never to the provider.
	Invite string
	// UserID and SessionID are the signed-in user's (IntentLink): the
	// callback links only while that session is still the caller's.
	UserID    string
	SessionID string
}

// Begin starts a sign-in: it returns the provider's authorization URL to
// send the browser to and the sealed flow cookie value.
func (p *Provider) Begin(ctx context.Context, st Start) (authURL, cookie string, err error) {
	d, err := p.discover(ctx, false)
	if err != nil {
		return "", "", err
	}
	if st.Intent == "" {
		st.Intent = IntentLogin
	}
	f := flow{
		State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: st.ReturnTo, Expires: p.now().Add(FlowTTL),
		Intent: st.Intent, Invite: st.Invite, UserID: st.UserID, SessionID: st.SessionID,
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return "", "", err
	}
	cookie, err = p.sealer.SealFor(cookiePurpose, string(raw))
	if err != nil {
		return "", "", fmt.Errorf("sso: seal the flow: %w", err)
	}
	authURL = d.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier))
	return authURL, cookie, nil
}

// Identity is who the provider says signed in.
type Identity struct {
	Issuer  string
	Subject string
	// Email as the provider reports it, for display only.
	Email string
	// Name is the user's display name, if the provider says.
	Name string
	// Usernames are candidates for a new user's username, best first:
	// the username claim, the email's local part, the subject.
	Usernames []string
	// Groups the user is in. GroupsKnown is false when neither the ID
	// token nor userinfo had the groups claim at all (then roles aren't
	// synced: a missing claim is a configuration problem, not "no
	// groups").
	Groups      []string
	GroupsKnown bool
}

// Result is a finished sign-in.
type Result struct {
	Identity Identity
	// ReturnTo is where the browser asked to go.
	ReturnTo string
	// Intent, Invite, UserID and SessionID are the flow's (Start).
	Intent    string
	Invite    string
	UserID    string
	SessionID string
}

// Finish completes a sign-in from the callback: cookie is the flow
// cookie's value, state and code the callback's parameters. Once the flow
// cookie opened and the state matched, the result carries the flow's
// fields (ReturnTo, Intent…) even with an error.
func (p *Provider) Finish(ctx context.Context, cookie, state, code string) (Result, error) {
	f, err := p.claim(cookie, state)
	if err != nil {
		return Result{}, err
	}
	// From here on the flow is known: failures carry it, so the caller
	// can send a link flow's errors back where it started.
	r := Result{ReturnTo: f.ReturnTo, Intent: f.Intent, Invite: f.Invite, UserID: f.UserID, SessionID: f.SessionID}
	if code == "" {
		return r, fmt.Errorf("%w: no code", ErrProvider)
	}
	d, err := p.discover(ctx, false)
	if err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := d.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return r, fmt.Errorf("%w: exchange the code: %w", ErrProvider, redactExchangeError(err))
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return r, fmt.Errorf("%w: no ID token in the response", ErrInvalidToken)
	}
	idt, err := d.verifier.Verify(oidc.ClientContext(ctx, p.client), rawID)
	if err != nil {
		return r, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if idt.Nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return r, fmt.Errorf("%w: nonce mismatch", ErrInvalidToken)
	}
	if idt.Subject == "" {
		return r, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return r, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if err := p.checkAZP(idt.Audience, claims); err != nil {
		return r, err
	}
	id := Identity{
		Issuer: p.cfg.Issuer, Subject: idt.Subject, Email: stringClaim(claims, "email"), Name: stringClaim(claims, "name"),
	}
	email := id.Email
	if u := stringClaim(claims, p.cfg.UsernameClaim); u != "" {
		if p.cfg.UsernameClaim == "email" {
			u, _, _ = strings.Cut(u, "@")
		}
		id.Usernames = append(id.Usernames, u)
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		id.Usernames = append(id.Usernames, local)
	}
	id.Usernames = append(id.Usernames, idt.Subject)
	groups, hasGroups := listClaim(claims, p.cfg.GroupsClaim)
	if !hasGroups && (len(p.cfg.AllowedGroups) > 0 || len(p.cfg.AdminGroups) > 0) {
		// Many providers put groups only in userinfo.
		groups, hasGroups, err = p.userinfoGroups(ctx, d, tok, idt.Subject)
		if err != nil {
			return r, err
		}
	}
	id.Groups, id.GroupsKnown = groups, hasGroups
	r.Identity = id
	if len(p.cfg.AllowedGroups) > 0 && !InAny(groups, p.cfg.AllowedGroups) {
		return r, ErrNotAllowed
	}
	return r, nil
}

// InAny reports whether any of groups is one of want.
func InAny(groups, want []string) bool {
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(want, g) })
}

// Cancel ends a flow the provider sent back with an error instead of a
// code. Like Finish, it checks the flow cookie and state first: it returns
// ErrFlowExpired or ErrStateMismatch when the callback doesn't belong to
// the flow, and nil when it does (the flow is then used up).
func (p *Provider) Cancel(cookie, state string) error {
	_, err := p.claim(cookie, state)
	return err
}

// claim opens the flow cookie, checks state against it and uses the flow
// up: whatever happens next, a flow is finished at most once.
func (p *Provider) claim(cookie, state string) (flow, error) {
	f, err := p.openFlow(cookie)
	if err != nil {
		return flow{}, err
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(f.State)) != 1 {
		return flow{}, ErrStateMismatch
	}
	if !p.nonces.use(f.Nonce, f.Expires, p.now()) {
		return flow{}, ErrNonceReused
	}
	return f, nil
}

func (p *Provider) openFlow(cookie string) (flow, error) {
	if cookie == "" {
		return flow{}, ErrFlowExpired
	}
	raw, err := p.sealer.OpenFor(cookiePurpose, cookie)
	if err != nil {
		return flow{}, ErrFlowExpired
	}
	var f flow
	if err := json.Unmarshal([]byte(raw), &f); err != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return flow{}, ErrFlowExpired
	}
	if !p.now().Before(f.Expires) {
		return flow{}, ErrFlowExpired
	}
	return f, nil
}

// checkAZP checks the authorized party (OpenID Connect Core 3.1.3.7): a
// token for several audiences must name us as azp, and an azp, when there
// is one, must be us. go-oidc checks only that the audience includes us.
func (p *Provider) checkAZP(aud []string, claims map[string]any) error {
	azp, has := claims["azp"]
	if !has {
		if len(aud) > 1 {
			return fmt.Errorf("%w: several audiences and no azp", ErrInvalidToken)
		}
		return nil
	}
	if s, ok := azp.(string); !ok || s != p.cfg.ClientID {
		return fmt.Errorf("%w: issued to another party (azp)", ErrInvalidToken)
	}
	return nil
}

// userinfoGroups reads the groups claim from the userinfo endpoint. Its
// subject must be the ID token's (OpenID Connect Core 5.3.2).
func (p *Provider) userinfoGroups(ctx context.Context, d *discovered, tok *oauth2.Token, subject string) ([]string, bool, error) {
	if d.provider.UserInfoEndpoint() == "" {
		return nil, false, nil
	}
	ui, err := d.provider.UserInfo(oidc.ClientContext(ctx, p.client), oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, false, fmt.Errorf("%w: userinfo: %w", ErrProvider, err)
	}
	if ui.Subject != subject {
		return nil, false, fmt.Errorf("%w: userinfo is about another subject", ErrInvalidToken)
	}
	var claims map[string]any
	if err := ui.Claims(&claims); err != nil {
		return nil, false, fmt.Errorf("%w: userinfo: %w", ErrProvider, err)
	}
	groups, ok := listClaim(claims, p.cfg.GroupsClaim)
	return groups, ok, nil
}

// redactExchangeError keeps the provider's error code and drops the
// response body, which may echo request values.
func redactExchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := re.ErrorCode
		if code == "" && re.Response != nil {
			code = re.Response.Status
		}
		return fmt.Errorf("token endpoint: %s", code)
	}
	return err
}

func stringClaim(claims map[string]any, name string) string {
	s, _ := claims[name].(string)
	return s
}

// listClaim reads a claim that is a list of strings, or a single string.
func listClaim(claims map[string]any, name string) ([]string, bool) {
	switch v := claims[name].(type) {
	case string:
		return []string{v}, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}

// nonceSet remembers the nonces of finished flows until those flows
// expire; after that, the flow cookie itself is refused.
type nonceSet struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceSet() *nonceSet { return &nonceSet{seen: map[string]time.Time{}} }

// use marks nonce as used and reports whether it was new.
func (n *nonceSet) use(nonce string, expires, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, exp := range n.seen {
		if !now.Before(exp) {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	n.seen[nonce] = expires
	return true
}
