// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"

	"github.com/panelarr/server/internal/auth"
)

// Cookie and header names.
const (
	SessionCookie = "panelarr_session"
	CSRFCookie    = "panelarr_csrf"
	CSRFHeader    = "X-CSRF-Token"
	APIKeyHeader  = "X-Api-Key" //nolint:gosec // a header name, not a credential
)

// cookieMaxAge is how long browsers keep the session cookies. The session
// itself expires in the database when unused; that is what counts.
const cookieMaxAge = 400 * 24 * time.Hour

// maxBodyBytes caps API request bodies.
const maxBodyBytes = 1 << 20

const (
	clientIPKey ctxKey = iota + 100
	responseWriterKey
	userAgentKey
	acceptLanguageKey
	requestHeaderKey
)

// requestCookie returns the value of the request's cookie name, or "".
func requestCookie(ctx context.Context, name string) string {
	h, _ := ctx.Value(requestHeaderKey).(http.Header)
	if h == nil {
		return ""
	}
	c, err := (&http.Request{Header: h}).Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// requestHeader returns the request's header name, or "".
func requestHeader(ctx context.Context, name string) string {
	h, _ := ctx.Value(requestHeaderKey).(http.Header)
	return h.Get(name)
}

// ClientIP returns the request's client address.
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// responseWriter gives strict handlers the response writer, to set cookies
// and headers before the generated code writes the body.
func responseWriter(ctx context.Context) http.ResponseWriter {
	w, _ := ctx.Value(responseWriterKey).(http.ResponseWriter)
	return w
}

// clientIP finds the client's address: the connection's peer, or, when the
// peer is a trusted proxy, the last untrusted address in X-Forwarded-For.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !isTrusted(addr) {
		return addr.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		a, err := netip.ParseAddr(h)
		if err != nil {
			break
		}
		addr = a.Unmap()
		if !isTrusted(addr) {
			break
		}
	}
	return addr.String()
}

// limitedBody is a request body under the default limit that remembers
// the body underneath, so an upload operation can raise the limit before
// anything is read.
type limitedBody struct {
	io.ReadCloser
	raw io.ReadCloser
	w   http.ResponseWriter
}

// raiseBodyLimit replaces the default body limit with n. Call it before
// the body is read.
func raiseBodyLimit(r *http.Request, n int64) {
	if lb, ok := r.Body.(*limitedBody); ok {
		r.Body = http.MaxBytesReader(lb.w, lb.raw, n)
	}
}

// apiKeyFrom returns the API key a request sends, if any.
func apiKeyFrom(r *http.Request) string {
	if k := r.Header.Get(APIKeyHeader); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// authenticate resolves the caller from an API key or the session cookie
// and puts it in the context. Requests without credentials go on as
// anonymous; each route decides whether that's enough. A request that
// sends an invalid API key is refused outright.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey, clientIP(r, s.deps.TrustedProxies))
		ctx = context.WithValue(ctx, responseWriterKey, w)
		ctx = context.WithValue(ctx, userAgentKey, r.UserAgent())
		ctx = context.WithValue(ctx, acceptLanguageKey, r.Header.Get("Accept-Language"))
		ctx = context.WithValue(ctx, requestHeaderKey, r.Header)
		if r.Body != nil {
			r.Body = &limitedBody{ReadCloser: http.MaxBytesReader(w, r.Body, maxBodyBytes), raw: r.Body, w: w}
		}
		r = r.WithContext(ctx)
		if s.deps.Auth == nil {
			next.ServeHTTP(w, r)
			return
		}
		var (
			p   *auth.Principal
			err error
		)
		if key := apiKeyFrom(r); key != "" {
			p, err = s.deps.Auth.AuthenticateAPIKey(ctx, key)
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				writeProblem(w, r, NewProblem(http.StatusUnauthorized, "invalid_api_key", "the API key is unknown, expired or its user is disabled"))
				return
			}
		} else if c, cerr := r.Cookie(SessionCookie); cerr == nil {
			p, err = s.deps.Auth.AuthenticateSession(ctx, c.Value)
		}
		if err != nil {
			writeError(s.deps.Log)(w, r, err)
			return
		}
		if p != nil {
			r = r.WithContext(auth.WithPrincipal(ctx, p))
		}
		next.ServeHTTP(w, r)
	})
}

// authorize enforces an operation's policy from the spec, then validates
// the request against the spec, refusing text with NUL characters the
// database can't store. It runs on the API subrouter and finds the
// route itself, so it decides before the generated code parses parameters
// or the body: an unauthorised caller learns nothing about them.
func (s *Server) authorize(routes chi.Routes) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := chi.NewRouteContext()
			pattern := routes.Find(rc, r.Method, strings.TrimPrefix(r.URL.Path, BasePath))
			pol := s.policies.lookup(r.Method, BasePath+pattern)
			if pattern == "" || pol == nil {
				// Not an operation (404, 405, the spec document): routing
				// answers.
				next.ServeHTTP(w, r)
				return
			}
			if prob := s.checkAccess(r, pol); prob != nil {
				writeProblem(w, r, prob)
				return
			}
			if pol.MaxBody > 0 {
				// Only now, for a caller allowed to call it.
				raiseBodyLimit(r, pol.MaxBody)
			}
			if prob := rejectNUL(r, rc, pol.MaxBody == 0); prob != nil {
				writeProblem(w, r, prob)
				return
			}
			if err := validateRequest(r, pol, rc); err != nil {
				writeProblem(w, r, validationProblem(err))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) checkAccess(r *http.Request, pol *policy) *Problem {
	ctx := r.Context()
	if !pol.Setup {
		has, err := s.deps.Users.HasUsers(ctx)
		if err != nil {
			s.deps.Log.ErrorContext(ctx, "setup state", "err", err)
			return NewProblem(http.StatusInternalServerError, CodeInternal, "")
		}
		if !has {
			return NewProblem(http.StatusConflict, "setup_required", "create the first account with POST /setup/admin")
		}
	}
	if pol.Access == AccessPublic {
		return nil
	}
	p := auth.FromContext(ctx)
	if p == nil {
		return NewProblem(http.StatusUnauthorized, "unauthenticated", "sign in or send an API key")
	}
	if pol.Access == AccessAdmin && !p.IsAdmin() {
		return NewProblem(http.StatusForbidden, "forbidden", "admins only")
	}
	switch p.Method {
	case auth.MethodAPIKey:
		if pol.SessionOnly {
			return NewProblem(http.StatusForbidden, "session_required", "this needs a signed-in browser session, not an API key")
		}
		need := auth.ScopeWrite
		if safeMethod(r.Method) || pol.ReadOnly {
			need = auth.ScopeRead
		}
		if !p.HasScope(need) || (pol.Access == AccessAdmin && !p.HasScope(auth.ScopeAdmin)) {
			return NewProblem(http.StatusForbidden, "insufficient_scope", "the API key's scopes don't allow this")
		}
	case auth.MethodSession:
		if !safeMethod(r.Method) {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || !p.CheckCSRF(r.Header.Get(CSRFHeader)) {
				return NewProblem(http.StatusForbidden, "csrf_failed", "send the CSRF token in the "+CSRFHeader+" header")
			}
		}
	}
	return nil
}

func validateRequest(r *http.Request, pol *policy, rc *chi.Context) error {
	params := make(map[string]string, len(rc.URLParams.Keys))
	for i, k := range rc.URLParams.Keys {
		params[k] = rc.URLParams.Values[i]
	}
	if pol.MaxBody > 0 {
		// An upload streams to its handler: the validator must not read
		// it (its security check buffers the whole body).
		shallow := *r
		shallow.Body = http.NoBody
		r = &shallow
	}
	return openapi3filter.ValidateRequest(r.Context(), &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: params,
		Route:      pol.Route,
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, // checked by checkAccess
			MultiError:         true,
			// Uploads stream to their handler, which checks them.
			ExcludeRequestBody: pol.MaxBody > 0,
		},
	})
}

// setSessionCookies hands a new session to the browser. Secure follows the
// public URL: plain-http installs on a home network must still work. The
// CSRF cookie is readable by scripts on purpose: the web app echoes it in
// the X-CSRF-Token header.
func (s *Server) setSessionCookies(ctx context.Context, sess auth.NewSession) {
	w := responseWriter(ctx)
	if w == nil {
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the base URL (see above)
		Name: SessionCookie, Value: sess.Token, Path: "/", MaxAge: int(cookieMaxAge.Seconds()),
		HttpOnly: true, Secure: s.deps.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // readable on purpose (see above)
		Name: CSRFCookie, Value: sess.CSRFToken, Path: "/", MaxAge: int(cookieMaxAge.Seconds()),
		Secure: s.deps.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookies removes the session cookies.
func (s *Server) clearSessionCookies(ctx context.Context) {
	w := responseWriter(ctx)
	if w == nil {
		return
	}
	for _, c := range []*http.Cookie{
		{Name: SessionCookie, HttpOnly: true}, //nolint:gosec // expires it; attributes set below
		{Name: CSRFCookie},                    //nolint:gosec // expires it; attributes set below
	} {
		c.Path, c.MaxAge, c.Secure, c.SameSite = "/", -1, s.deps.SecureCookies, http.SameSiteLaxMode
		http.SetCookie(w, c)
	}
}
