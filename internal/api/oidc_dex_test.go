// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/bcrypt"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/sso"
)

// dexImage is a real OpenID provider for the integration test, pinned to
// a release and its digest.
const dexImage = "ghcr.io/dexidp/dex:v2.41.1@sha256:bc7cfce7c17f52864e2bb2a4dc1d2f86a41e3019f6d42e81d92a301fad0c8a1d"

// dexIssuer is the issuer Dex is configured with. Every request to it
// (Panelarr's and the test browser's) goes through dexTransport, which
// dials the container's mapped port, so no fixed host port is needed.
const (
	dexHost         = "dex.test:5556"
	dexIssuer       = "http://" + dexHost + "/dex"
	dexClientID     = "panelarr"
	dexClientSecret = "dex-client-secret"
	dexEmail        = "alice@example.com"
	dexPassword     = "alice password 1"
)

func dexConfig(t *testing.T) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(dexPassword), 10) // Dex refuses cheaper hashes
	if err != nil {
		t.Fatal(err)
	}
	return `issuer: ` + dexIssuer + `
storage:
  type: memory
web:
  http: 0.0.0.0:5556
oauth2:
  skipApprovalScreen: true
staticClients:
  - id: ` + dexClientID + `
    name: Panelarr
    secret: ` + dexClientSecret + `
    redirectURIs:
      - ` + mockRedirectURI + `
enablePasswordDB: true
staticPasswords:
  - email: ` + dexEmail + `
    hash: "` + string(hash) + `"
    username: alice
    userID: 1f3a5c64-0c0c-4e2a-9e57-6d1f1c0f2a11
`
}

// startDex runs Dex and returns a transport that reaches it as dexHost.
func startDex(t *testing.T) *http.Transport {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.Run(ctx, dexImage,
		testcontainers.WithExposedPorts("5556/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: strings.NewReader(dexConfig(t)), ContainerFilePath: "/etc/dex/panelarr.yaml", FileMode: 0o644,
		}),
		testcontainers.WithCmd("dex", "serve", "/etc/dex/panelarr.yaml"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/dex/.well-known/openid-configuration").WithPort("5556/tcp").WithStartupTimeout(2*time.Minute)),
		testcontainers.WithLogger(tclog.TestLogger(t)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start Dex: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "5556/tcp")
	if err != nil {
		t.Fatal(err)
	}
	target := net.JoinHostPort(host, port.Port())
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != dexHost {
			// Only Dex is reachable: anything else is a test bug.
			return nil, &net.AddrError{Err: "not Dex", Addr: addr}
		}
		return d.DialContext(ctx, network, target)
	}}
}

// dexLogin plays the browser at Dex: it follows the authorization URL to
// Dex's login form, signs in, and returns the callback Dex redirects to
// (path and query, for Panelarr's test server).
func dexLogin(t *testing.T, tr *http.Transport, authURL string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host != dexHost {
			return http.ErrUseLastResponse // back to Panelarr
		}
		return nil
	}}
	res, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Request.URL.Path, "/auth/local") {
		t.Fatalf("Dex login form: %d at %s", res.StatusCode, res.Request.URL)
	}
	res, err = c.PostForm(res.Request.URL.String(), url.Values{"login": {dexEmail}, "password": {dexPassword}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	loc, err := res.Location()
	if err != nil || loc.Host != "panelarr.example.com" {
		t.Fatalf("Dex sign-in: %d %v: %.2000s", res.StatusCode, err, body)
	}
	return loc.Path + "?" + loc.RawQuery
}

// TestOIDCWithDex runs the whole redirect flow against a real provider
// (Dex in a container): discovery, the authorization request with PKCE
// (S256), state and nonce, Dex's login, the code exchange with the
// verifier and the client secret, the ID token checked against Dex's
// keys, and provisioning a user from Dex's claims.
func TestOIDCWithDex(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	if os.Getenv("CI") == "" {
		// Locally, without Docker, skip; in CI it must run.
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	t.Parallel()
	tr := startDex(t)
	provider := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	// The discovery document is Dex's own, and says it does S256.
	res, err := provider.Get(dexIssuer + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	var disc struct {
		Issuer  string   `json:"issuer"`
		Methods []string `json:"code_challenge_methods_supported"`
		Token   string   `json:"token_endpoint"`
	}
	err = json.NewDecoder(res.Body).Decode(&disc)
	_ = res.Body.Close()
	if err != nil || disc.Issuer != dexIssuer || !strings.Contains(strings.Join(disc.Methods, " "), "S256") {
		t.Fatalf("Dex discovery: %+v %v", disc, err)
	}

	h := newHarness(t, func(d *Deps) {
		box := secret.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
		d.SSO = sso.New(sso.Config{
			Issuer: dexIssuer, ClientID: dexClientID, ClientSecret: dexClientSecret,
			Scopes: []string{"openid", "profile", "email"}, RedirectURL: mockRedirectURI,
			Label: "Sign in with Dex", AutoProvision: true,
		}, box, provider, nil)
	})
	admin, _ := h.setupAdmin()

	b := h.browser()
	authURL := b.startSSO("/library")
	q := mustURL(t, authURL).Query()
	if !strings.HasPrefix(authURL, dexIssuer+"/auth?") || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorization request: %s", authURL)
	}
	me := b.signedIn(b.do(http.MethodGet, dexLogin(t, tr, authURL), nil), "/library")
	if me.User.Username != "alice" || me.User.Role != apigen.RoleUser || !isTrue(me.User.Sso) || isTrue(me.User.HasPassword) {
		t.Fatalf("provisioned from Dex: %+v", me.User)
	}
	ids := call[apigen.IdentityList](b, http.MethodGet, "/api/v1/me/identities", nil, http.StatusOK)
	if len(ids.Identities) != 1 || ids.Identities[0].Issuer != dexIssuer || ids.Identities[0].Email != dexEmail || ids.Identities[0].Subject == "" {
		t.Fatalf("identity: %+v", ids)
	}

	// Signing in again finds the same user through the link.
	b2 := h.browser()
	cb := dexLogin(t, tr, b2.startSSO("/"))
	if got := b2.signedIn(b2.do(http.MethodGet, cb, nil), "/"); got.User.Id != me.User.Id {
		t.Fatalf("second sign-in: %+v", got.User)
	}
	// The same callback again is refused: the flow is used up.
	if res := b2.do(http.MethodGet, cb, nil); res.header.Get("Location") != "/login?sso_error=flow_expired" {
		t.Fatalf("replayed callback: %d %q", res.status, res.header.Get("Location"))
	}

	// Dex enforces PKCE on the code: without the verifier it won't hand
	// out tokens, so a stolen code is worth nothing.
	cb = dexLogin(t, tr, h.browser().startSSO("/"))
	code := mustURL(t, cb).Query().Get("code")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, disc.Token, strings.NewReader(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {mockRedirectURI},
	}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(dexClientID, dexClientSecret)
	res, err = provider.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7636 4.6: a missing verifier is an invalid_grant, so the
	// refusal is PKCE's and not, say, a client authentication failure.
	var tokenErr struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tokenErr); err != nil || res.StatusCode != http.StatusBadRequest ||
		tokenErr.Error != "invalid_grant" || !strings.Contains(tokenErr.Description, "code_verifier") {
		t.Fatalf("token exchange without the verifier: %d %s", res.StatusCode, raw)
	}

	// The admin's status page sees Dex.
	st := call[apigen.SystemAuth](admin, http.MethodGet, "/api/v1/system/auth", nil, http.StatusOK)
	if !st.Oidc.Enabled || !isTrue(st.Oidc.Reachable) || deref(st.Oidc.Issuer) != dexIssuer {
		t.Fatalf("system auth: %+v", st)
	}
}
