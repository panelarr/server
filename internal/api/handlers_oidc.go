// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/sso"
	"github.com/panelarr/server/internal/users"
)

// OIDCFlowCookie carries a single sign-on flow's sealed secrets from
// /auth/oidc/start to /auth/oidc/callback, and only there.
const OIDCFlowCookie = "panelarr_oidc"

// oidcCookiePath limits the flow cookie to the callback's directory.
const oidcCookiePath = BasePath + "/auth/oidc/"

// ssoErrorPage is where a failed callback sends the browser, with the
// error's code in sso_error.
const ssoErrorPage = "/login"

// GetAuthMethods says how users can sign in.
func (s *Server) GetAuthMethods(_ context.Context, _ apigen.GetAuthMethodsRequestObject) (apigen.GetAuthMethodsResponseObject, error) {
	out := apigen.GetAuthMethods200JSONResponse{Password: apigen.AuthMethodsPasswordAll}
	if s.deps.SSO != nil {
		out.Oidc = &apigen.OidcMethod{Label: s.deps.SSO.Label()}
		if s.deps.Auth.PasswordAdminsOnly() {
			out.Password = apigen.AuthMethodsPasswordAdmins
		}
	}
	return out, nil
}

// errSSOOff answers the single sign-on endpoints when it isn't configured,
// as if they didn't exist.
var errSSOOff = NewProblem(http.StatusNotFound, CodeNotFound, "no such endpoint")

// StartOidcLogin sends the browser to the identity provider: to sign in,
// to link the login to the signed-in user, or to accept an invite.
func (s *Server) StartOidcLogin(ctx context.Context, req apigen.StartOidcLoginRequestObject) (apigen.StartOidcLoginResponseObject, error) {
	if s.deps.SSO == nil {
		return nil, errSSOOff
	}
	returnTo := "/"
	if req.Params.ReturnTo != nil {
		var ok bool
		if returnTo, ok = localPath(*req.Params.ReturnTo); !ok {
			return nil, NewProblem(http.StatusBadRequest, "invalid_return_to", "return_to must be a path on this server, like /library")
		}
	}
	st := sso.Start{ReturnTo: returnTo, Intent: sso.IntentLogin}
	invite := deref(req.Params.Invite)
	if req.Params.Intent != nil && *req.Params.Intent == apigen.StartOidcLoginParamsIntentLink {
		if invite != "" {
			return nil, fieldProblem("invite", "can't be combined with intent=link")
		}
		p := auth.FromContext(ctx)
		if p == nil {
			return nil, NewProblem(http.StatusUnauthorized, "unauthenticated", "sign in first to link a login")
		}
		if p.Method != auth.MethodSession {
			return nil, NewProblem(http.StatusForbidden, "session_required", "linking a login needs a signed-in browser session, not an API key")
		}
		// Linking changes the account, and this is a GET: only a
		// navigation from Panelarr itself may start it, never one
		// another site makes the browser do.
		if site := requestHeader(ctx, "Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			return nil, NewProblem(http.StatusForbidden, "cross_site", "start linking from your account page")
		}
		st.Intent, st.UserID, st.SessionID = sso.IntentLink, p.User.ID.String(), p.SessionID.String()
	} else if invite != "" {
		if _, err := s.deps.Users.CheckInvite(ctx, invite); err != nil {
			return nil, domainError(err)
		}
		st.Intent, st.Invite = sso.IntentInvite, invite
	}
	authURL, cookie, err := s.deps.SSO.Begin(ctx, st)
	if errors.Is(err, sso.ErrUnavailable) {
		return nil, NewProblem(http.StatusServiceUnavailable, "oidc_unavailable", "the identity provider can't be reached; try again later or sign in with a password")
	}
	if err != nil {
		return nil, err
	}
	if w := responseWriter(ctx); w != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.SetCookie(w, s.flowCookie(cookie, int(sso.FlowTTL.Seconds())))
	}
	return apigen.StartOidcLogin302Response{Headers: apigen.StartOidcLogin302ResponseHeaders{Location: &authURL}}, nil
}

// FinishOidcLogin finishes a flow at the identity provider: the design's
// callback rules (sso checks the flow, the ID token and the allowed
// groups; users.SignInSSO links, provisions, syncs the role and starts
// the session). Every failure starts no session and sends the browser to
// the login page with a code, or a link flow back where it started.
func (s *Server) FinishOidcLogin(ctx context.Context, req apigen.FinishOidcLoginRequestObject) (apigen.FinishOidcLoginResponseObject, error) {
	if s.deps.SSO == nil {
		return nil, errSSOOff
	}
	w := responseWriter(ctx)
	if w != nil {
		w.Header().Set("Cache-Control", "no-store")
	}
	// The flow cookie is cleared only once the callback is known to
	// belong to it (its state matched) or the cookie is useless anyway:
	// a forged callback must not end a sign-in in progress.
	usedUp := func(err error) {
		if w != nil && !errors.Is(err, sso.ErrStateMismatch) {
			http.SetCookie(w, s.flowCookie("", -1))
		}
	}
	redirect := func(loc string) (apigen.FinishOidcLoginResponseObject, error) {
		return apigen.FinishOidcLogin302Response{Headers: apigen.FinishOidcLogin302ResponseHeaders{Location: &loc}}, nil
	}
	var res sso.Result
	refuse := func(code string) (apigen.FinishOidcLoginResponseObject, error) {
		if res.Intent == sso.IntentLink && res.ReturnTo != "" {
			return redirect(withQuery(res.ReturnTo, "sso_error", code))
		}
		return redirect(ssoErrorPage + "?sso_error=" + url.QueryEscape(code))
	}
	ci := s.clientInfo(ctx)
	// Only a callback that belongs to its flow (the cookie opened and
	// the state matched) counts against the address's single sign-on
	// limit. Anything earlier can come from any website that makes the
	// browser load the callback, so it is logged and refused, not counted.
	fail := func(code string, err error, args ...any) (apigen.FinishOidcLoginResponseObject, error) {
		args = append([]any{"err", err}, args...)
		if counts(err) {
			s.deps.Auth.SSOFailed(ctx, ci, code, args...)
		} else {
			s.deps.Auth.SSORefused(ctx, ci, code, args...)
		}
		return refuse(code)
	}
	if s.deps.Auth.SSOBlocked(ctx, ci) != nil {
		return refuse("rate_limited")
	}
	p := req.Params
	cookie := requestCookie(ctx, OIDCFlowCookie)
	if p.Error != nil {
		err := s.deps.SSO.Cancel(cookie, deref(p.State))
		usedUp(err)
		if err != nil {
			return fail(ssoErrorCode(err), err)
		}
		// The provider's error code only: the description is free text.
		return fail("provider_error", errors.New("provider error: "+printableCode(*p.Error)))
	}
	res, err := s.deps.SSO.Finish(ctx, cookie, deref(p.State), deref(p.Code))
	usedUp(err)
	if err != nil {
		return fail(ssoErrorCode(err), err)
	}
	id := res.Identity
	in := users.SSOLogin{
		Identity: auth.Identity{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email},
		Name:     id.Name, Usernames: id.Usernames, Groups: id.Groups, GroupsKnown: id.GroupsKnown,
	}
	cfg := s.deps.SSO.Settings()
	in.AutoProvision, in.AdminGroups = cfg.AutoProvision, cfg.AdminGroups
	switch res.Intent {
	case sso.IntentLink:
		// Link to the user who started the flow, and only while that
		// same session is still signed in.
		cur := auth.FromContext(ctx)
		if cur == nil || cur.Method != auth.MethodSession || cur.SessionID.String() != res.SessionID || cur.User.ID.String() != res.UserID {
			return fail("session_changed", errSessionChanged)
		}
		in.LinkTo = cur.User.ID
	case sso.IntentInvite:
		in.InviteToken = res.Invite
	}
	out, err := s.deps.Users.SignInSSO(ctx, in, ci)
	var rl *auth.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return refuse("rate_limited")
	case errors.Is(err, auth.ErrNotLinked):
		return fail("not_linked", err, "issuer", id.Issuer, "subject", id.Subject)
	case errors.Is(err, auth.ErrDisabled):
		return fail("account_disabled", err, "issuer", id.Issuer, "subject", id.Subject)
	case errors.Is(err, users.ErrAlreadyLinked):
		return fail("already_linked", err, "issuer", id.Issuer, "subject", id.Subject)
	case errors.Is(err, users.ErrLinkExists):
		return fail("link_exists", err)
	case errors.Is(err, users.ErrInviteInvalid):
		return fail("invite_invalid", err)
	case errors.Is(err, users.ErrNotFound):
		return fail("session_changed", err)
	case err != nil:
		return nil, err
	}
	if out.Session != nil {
		s.setSessionCookies(ctx, *out.Session)
	}
	return redirect(res.ReturnTo)
}

// errSessionChanged: a link flow came back to a browser that is no longer
// signed in with the session that started it.
var errSessionChanged = errors.New("the session that started linking is gone")

// withQuery adds name=value to a local path's query, before its fragment.
func withQuery(local, name, value string) string {
	u, err := url.Parse(local)
	if err != nil {
		return ssoErrorPage + "?" + name + "=" + url.QueryEscape(value)
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// flowCookie is the flow cookie with value, or its removal (maxAge < 0).
// SameSite Lax: the provider sends the browser back with a top-level GET,
// which carries Lax cookies.
func (s *Server) flowCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure follows the base URL, like the session cookie
		Name: OIDCFlowCookie, Value: value, Path: oidcCookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: s.deps.SecureCookies, SameSite: http.SameSiteLaxMode,
	}
}

// ssoErrorCode is the login page's code for a failed flow.
func ssoErrorCode(err error) string {
	switch {
	case errors.Is(err, sso.ErrFlowExpired):
		return "flow_expired"
	case errors.Is(err, sso.ErrStateMismatch):
		return "state_mismatch"
	case errors.Is(err, sso.ErrNonceReused):
		return "nonce_reused"
	case errors.Is(err, sso.ErrUnavailable):
		return "oidc_unavailable"
	case errors.Is(err, sso.ErrInvalidToken):
		return "invalid_token"
	case errors.Is(err, sso.ErrNotAllowed):
		return "not_allowed"
	default:
		return "provider_error"
	}
}

// counts reports whether a failed callback counts against the client's
// single sign-on limit: not when it never matched a flow of this browser,
// nor when the provider can't be reached.
func counts(err error) bool {
	return !errors.Is(err, sso.ErrFlowExpired) && !errors.Is(err, sso.ErrStateMismatch) && !errors.Is(err, sso.ErrUnavailable)
}

// printableCode keeps a provider's error code short and printable for the
// log.
func printableCode(s string) string {
	s = strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// localPath reports whether s is a path on this server, and returns it.
// Anything a browser could take to another origin is refused: absolute
// and scheme-relative URLs ("//host", and "/\host", which browsers read
// as "//host"), backslashes and control characters anywhere. The path is
// returned cleaned (path.Clean: no dot segments or doubled slashes, so
// "/.//host" becomes "/host"), and a cleaned path starting with "//" is
// refused as well, so what the web app gets back can never turn
// scheme-relative if it reuses it as a URL.
func localPath(s string) (string, bool) {
	if s == "" {
		return "/", true
	}
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\") {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return "", false
	}
	// Clean the path as written (still escaped), keeping the query,
	// fragment and a trailing slash.
	p, rest := s, ""
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		p, rest = s[:i], s[i:]
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	if strings.HasPrefix(clean, "//") {
		return "", false
	}
	return clean + rest, true
}
