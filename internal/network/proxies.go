// SPDX-License-Identifier: GPL-3.0-or-later

package network

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store/db"
)

// Proxy is an HTTP, HTTPS or SOCKS5 proxy, e.g. a VPN container's. Its
// password is kept encrypted and never shown.
type Proxy struct {
	ID, Name, URL, Username string
	HasPassword             bool
	CreatedAt, UpdatedAt    time.Time
}

// ProxyInput is a proxy as given: Password is plain.
type ProxyInput struct {
	ID, Name, URL, Username, Password string
}

// ProxyPatch changes some of a proxy's fields. An empty Password keeps the
// stored one; an empty Username removes the credentials.
type ProxyPatch struct {
	Name, URL, Username, Password *string
}

func proxyOf(r db.NetworkProxy) Proxy {
	return Proxy{
		ID: r.ID, Name: r.Name, URL: r.Url, Username: r.Username, HasPassword: r.PasswordSealed != "",
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func checkProxy(in ProxyInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return invalid("name", "give it a name")
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" || u.Port() == "" {
		return invalid("url", "must be http://, https:// or socks5://host:port")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return invalid("url", "must be http://, https:// or socks5://host:port")
	}
	if u.User != nil {
		return invalid("url", "give the credentials as username and password, not in the URL")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return invalid("url", "no path or query")
	}
	if in.Password != "" && in.Username == "" {
		return invalid("username", "a password needs a username")
	}
	return nil
}

// Proxies lists the proxies.
func (s *Service) Proxies(ctx context.Context) ([]Proxy, error) {
	rows, err := s.st.ListProxies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Proxy, len(rows))
	for i, r := range rows {
		out[i] = proxyOf(r)
	}
	return out, nil
}

// Proxy returns one proxy.
func (s *Service) Proxy(ctx context.Context, id string) (Proxy, error) {
	r, err := s.st.GetProxy(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proxy{}, ErrNotFound
	}
	if err != nil {
		return Proxy{}, err
	}
	return proxyOf(r), nil
}

// proxyInput is a stored proxy with its password opened.
func (s *Service) proxyInput(ctx context.Context, id string) (ProxyInput, error) {
	r, err := s.st.GetProxy(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProxyInput{}, ErrNotFound
	}
	if err != nil {
		return ProxyInput{}, err
	}
	in := ProxyInput{ID: r.ID, Name: r.Name, URL: r.Url, Username: r.Username}
	if r.PasswordSealed != "" {
		if in.Password, err = s.box.Open(r.PasswordSealed); err != nil {
			return ProxyInput{}, err
		}
	}
	return in, nil
}

func (s *Service) seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return s.box.Seal(plain)
}

// CreateProxy adds a proxy.
func (s *Service) CreateProxy(ctx context.Context, in ProxyInput) (Proxy, error) {
	if !idPattern.MatchString(in.ID) {
		return Proxy{}, ErrInvalidID
	}
	if err := checkProxy(in); err != nil {
		return Proxy{}, err
	}
	sealed, err := s.seal(in.Password)
	if err != nil {
		return Proxy{}, err
	}
	r, err := s.st.InsertProxy(ctx, db.InsertProxyParams{
		ID: in.ID, Name: strings.TrimSpace(in.Name), Url: in.URL, Username: in.Username, PasswordSealed: sealed,
	})
	if isUniqueViolation(err) {
		return Proxy{}, ErrIDTaken
	}
	if err != nil {
		return Proxy{}, err
	}
	s.log.InfoContext(ctx, "proxy added", "component", "audit", "proxy", in.ID)
	return proxyOf(r), nil
}

// UpdateProxy changes a proxy.
func (s *Service) UpdateProxy(ctx context.Context, id string, p ProxyPatch) (Proxy, error) {
	cur, err := s.proxyInput(ctx, id)
	if err != nil {
		return Proxy{}, err
	}
	set(&cur.Name, p.Name)
	set(&cur.URL, p.URL)
	set(&cur.Username, p.Username)
	if p.Password != nil && *p.Password != "" {
		cur.Password = *p.Password
	}
	if cur.Username == "" {
		cur.Password = ""
	}
	if err := checkProxy(cur); err != nil {
		return Proxy{}, err
	}
	sealed, err := s.seal(cur.Password)
	if err != nil {
		return Proxy{}, err
	}
	r, err := s.st.UpdateProxy(ctx, db.UpdateProxyParams{
		ID: id, Name: strings.TrimSpace(cur.Name), Url: cur.URL, Username: cur.Username, PasswordSealed: sealed,
	})
	if err != nil {
		return Proxy{}, err
	}
	s.log.InfoContext(ctx, "proxy changed", "component", "audit", "proxy", id)
	return proxyOf(r), nil
}

// DeleteProxy removes a proxy no profile uses.
func (s *Service) DeleteProxy(ctx context.Context, id string) error {
	n, err := s.st.DeleteProxy(ctx, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.log.InfoContext(ctx, "proxy removed", "component", "audit", "proxy", id)
	return nil
}

// TestProxy checks a proxy: with target, a request through it; without,
// that it accepts connections. in may be unsaved; a saved proxy's stored
// password is used when in.Password is empty and in.ID names it.
func (s *Service) TestProxy(ctx context.Context, in ProxyInput, target string) (TestResult, error) {
	if in.ID != "" && in.Password == "" {
		if stored, err := s.proxyInput(ctx, in.ID); err == nil && stored.Username == in.Username {
			in.Password = stored.Password
		}
	}
	if err := checkProxy(in); err != nil {
		return TestResult{}, err
	}
	if target != "" {
		if err := checkHTTPURL(target); err != nil {
			return TestResult{}, invalid("url", err.Error())
		}
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	pu, _ := url.Parse(in.URL)
	start := time.Now()
	if target == "" {
		conn, err := s.dial(ctx, "tcp", pu.Host)
		took := time.Since(start)
		if err != nil {
			return TestResult{Message: "can't connect: " + err.Error(), Latency: took}, nil
		}
		_ = conn.Close()
		return TestResult{OK: true, Message: "accepts connections (give a URL to test a request through it)", Latency: took}, nil
	}
	if in.Username != "" {
		pu.User = url.UserPassword(in.Username, in.Password)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu), DialContext: s.dial, TLSHandshakeTimeout: testTimeout}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return TestResult{}, err
	}
	res, err := (&http.Client{Transport: tr, Timeout: testTimeout}).Do(req)
	took := time.Since(start)
	if err != nil {
		// The error may quote the proxy URL; never its password.
		msg := strings.ReplaceAll(err.Error(), in.Password, "***")
		if in.Password == "" {
			msg = err.Error()
		}
		return TestResult{Message: "request failed: " + msg, Latency: took}, nil
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusProxyAuthRequired {
		return TestResult{Message: "the proxy wants (other) credentials", Latency: took}, nil
	}
	return TestResult{OK: true, Message: fmt.Sprintf("got HTTP %d through the proxy", res.StatusCode), Latency: took}, nil
}
