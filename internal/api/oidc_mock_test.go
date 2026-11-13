// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockOP is an in-process OpenID provider: discovery, keys, an
// authorization step the test drives directly, a token endpoint that
// checks PKCE and the client secret, and userinfo.
type mockOP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	client string
	secret string

	mu    sync.Mutex
	codes map[string]*mockGrant
	// next is used for the next authorization.
	next mockClaims
	// tokenCalls counts token endpoint calls.
	tokenCalls int
}

// mockClaims shapes the ID token of the next sign-in.
type mockClaims struct {
	Subject string
	Email   string
	// Groups go in the ID token's groups claim (when not nil).
	Groups []string
	// UserinfoGroups go in userinfo's groups claim (when not nil).
	UserinfoGroups []string
	// Overrides of what a correct token says.
	Issuer   string
	Audience string
	Nonce    string
	Expiry   time.Duration // from now; 0 means an hour
	// SignWith signs the token with another key.
	SignWith *rsa.PrivateKey
}

type mockGrant struct {
	challenge   string
	nonce       string
	redirectURI string
	claims      mockClaims
	used        bool
}

const (
	mockClientID     = "panelarr-test"
	mockClientSecret = "test-client-secret"
	// mockRedirectURI is Panelarr's callback as configured; the test
	// client calls its path on the test server.
	mockRedirectURI = "https://panelarr.example.com/api/v1/auth/oidc/callback"
)

func newMockOP(t *testing.T) *mockOP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockOP{t: t, key: key, client: mockClientID, secret: mockClientSecret, codes: map[string]*mockGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                m.srv.URL,
			"authorization_endpoint":                m.srv.URL + "/authorize",
			"token_endpoint":                        m.srv.URL + "/token",
			"jwks_uri":                              m.srv.URL + "/keys",
			"userinfo_endpoint":                     m.srv.URL + "/userinfo",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(m.key.N.Bytes()), "e": b64(big.NewInt(int64(m.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", m.token)
	mux.HandleFunc("GET /userinfo", m.userinfo)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// authorize plays the provider's login page: it checks the request
// Panelarr sent the browser with, and returns the callback path with a
// fresh code, as the provider would redirect.
func (m *mockOP) authorize(authURL string) string {
	m.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		m.t.Fatal(err)
	}
	if !strings.HasPrefix(authURL, m.srv.URL+"/authorize?") {
		m.t.Fatalf("not sent to the provider: %s", authURL)
	}
	q := u.Query()
	for k, want := range map[string]string{"client_id": m.client, "response_type": "code", "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			m.t.Fatalf("authorize %s = %q, want %q (%s)", k, q.Get(k), want, authURL)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 32 {
			m.t.Fatalf("authorize %s missing or short: %q", k, q.Get(k))
		}
	}
	if q.Get("redirect_uri") != mockRedirectURI {
		m.t.Fatalf("redirect_uri %q", q.Get("redirect_uri"))
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		m.t.Fatalf("scope without openid: %q", q.Get("scope"))
	}
	m.mu.Lock()
	code := b64(randomBytes(m.t))
	m.codes[code] = &mockGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirectURI: q.Get("redirect_uri"), claims: m.next}
	m.mu.Unlock()
	cb, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		m.t.Fatal(err)
	}
	// Providers may add parameters of their own.
	return cb.Path + "?" + url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {m.srv.URL}}.Encode()
}

func randomBytes(t *testing.T) []byte {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (m *mockOP) token(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenCalls++
	fail := func(code string) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
	}
	if err := r.ParseForm(); err != nil {
		fail("invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != m.client || subtle.ConstantTimeCompare([]byte(secret), []byte(m.secret)) != 1 {
		fail("invalid_client")
		return
	}
	g := m.codes[r.PostForm.Get("code")]
	if g == nil || g.used || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirectURI {
		fail("invalid_grant")
		return
	}
	g.used = true
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if b64(sum[:]) != g.challenge {
		fail("invalid_grant") // PKCE: wrong or missing verifier
		return
	}
	c := g.claims
	claims := map[string]any{
		"iss": m.srv.URL, "aud": m.client, "sub": c.Subject, "nonce": g.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if c.Email != "" {
		claims["email"] = c.Email
	}
	if c.Groups != nil {
		claims["groups"] = c.Groups
	}
	if c.Issuer != "" {
		claims["iss"] = c.Issuer
	}
	if c.Audience != "" {
		claims["aud"] = c.Audience
	}
	if c.Nonce != "" {
		claims["nonce"] = c.Nonce
	}
	if c.Expiry != 0 {
		claims["exp"] = time.Now().Add(c.Expiry).Unix()
	}
	key := m.key
	if c.SignWith != nil {
		key = c.SignWith
	}
	access := "access-" + b64(randomBytes(m.t))
	m.codes["access:"+access] = g
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 3600,
		"id_token": m.sign(key, claims),
	})
}

// calls is how often the token endpoint was called.
func (m *mockOP) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokenCalls
}

func (m *mockOP) userinfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	g := m.codes["access:"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	m.mu.Unlock()
	if g == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	out := map[string]any{"sub": g.claims.Subject}
	if g.claims.UserinfoGroups != nil {
		out["groups"] = g.claims.UserinfoGroups
	}
	writeJSON(w, http.StatusOK, out)
}

// sign makes an RS256 JWT.
func (m *mockOP) sign(key *rsa.PrivateKey, claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	payload, err := json.Marshal(claims)
	if err != nil {
		m.t.Fatal(err)
	}
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		m.t.Fatal(err)
	}
	return signing + "." + b64(sig)
}
