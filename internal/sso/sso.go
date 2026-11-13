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
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
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
	return &Provider{cfg: cfg, sealer: sealer, client: client, log: log, now: time.Now, retryWait: retryMin, nonces: newNonceSet()}
}

// Label is the sign-in button's label.
func (p *Provider) Label() string { return p.cfg.Label }

// Issuer is the provider's issuer, which with a subject identifies a
// user.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

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
// when it meets an unknown key.
func (p *Provider) discover(ctx context.Context, force bool) (*discovered, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disc != nil {
		return p.disc, nil
	}
	now := p.now()
	if !force && p.lastErr != nil && now.Sub(p.lastTry) < p.retryWait {
		return nil, p.lastErr
	}
	p.lastTry = now
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, p.client), requestTimeout)
	defer cancel()
	prov, err := oidc.NewProvider(ctx, p.cfg.Issuer)
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
}

// Begin starts a sign-in: it returns the provider's authorization URL to
// send the browser to and the sealed flow cookie value. returnTo must
// already be checked to be a local path.
func (p *Provider) Begin(ctx context.Context, returnTo string) (authURL, cookie string, err error) {
	d, err := p.discover(ctx, false)
	if err != nil {
		return "", "", err
	}
	f := flow{
		State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo, Expires: p.now().Add(FlowTTL),
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
	Email  string
	Groups []string
}

// Result is a finished sign-in.
type Result struct {
	Identity Identity
	// ReturnTo is where the browser asked to go.
	ReturnTo string
}

// Finish completes a sign-in from the callback: cookie is the flow
// cookie's value, state and code the callback's parameters.
func (p *Provider) Finish(ctx context.Context, cookie, state, code string) (Result, error) {
	f, err := p.openFlow(cookie)
	if err != nil {
		return Result{}, err
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(f.State)) != 1 {
		return Result{}, ErrStateMismatch
	}
	// Used up from here on, whatever happens next: a flow is finished at
	// most once.
	if !p.nonces.use(f.Nonce, f.Expires, p.now()) {
		return Result{}, ErrNonceReused
	}
	if code == "" {
		return Result{}, fmt.Errorf("%w: no code", ErrProvider)
	}
	d, err := p.discover(ctx, false)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := d.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return Result{}, fmt.Errorf("%w: exchange the code: %w", ErrProvider, redactExchangeError(err))
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return Result{}, fmt.Errorf("%w: no ID token in the response", ErrInvalidToken)
	}
	idt, err := d.verifier.Verify(oidc.ClientContext(ctx, p.client), rawID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if idt.Nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return Result{}, fmt.Errorf("%w: nonce mismatch", ErrInvalidToken)
	}
	if idt.Subject == "" {
		return Result{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	id := Identity{Issuer: p.cfg.Issuer, Subject: idt.Subject, Email: stringClaim(claims, "email")}
	groups, hasGroups := listClaim(claims, p.cfg.GroupsClaim)
	if !hasGroups && len(p.cfg.AllowedGroups) > 0 {
		// Many providers put groups only in userinfo.
		groups, err = p.userinfoGroups(ctx, d, tok, idt.Subject)
		if err != nil {
			return Result{}, err
		}
	}
	id.Groups = groups
	if len(p.cfg.AllowedGroups) > 0 && !slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(p.cfg.AllowedGroups, g) }) {
		return Result{Identity: id}, ErrNotAllowed
	}
	return Result{Identity: id, ReturnTo: f.ReturnTo}, nil
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

// userinfoGroups reads the groups claim from the userinfo endpoint. Its
// subject must be the ID token's (OpenID Connect Core 5.3.2).
func (p *Provider) userinfoGroups(ctx context.Context, d *discovered, tok *oauth2.Token, subject string) ([]string, error) {
	if d.provider.UserInfoEndpoint() == "" {
		return nil, nil
	}
	ui, err := d.provider.UserInfo(oidc.ClientContext(ctx, p.client), oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %w", ErrProvider, err)
	}
	if ui.Subject != subject {
		return nil, fmt.Errorf("%w: userinfo is about another subject", ErrInvalidToken)
	}
	var claims map[string]any
	if err := ui.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: userinfo: %w", ErrProvider, err)
	}
	groups, _ := listClaim(claims, p.cfg.GroupsClaim)
	return groups, nil
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
