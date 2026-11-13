// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors callers map to responses.
var (
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	ErrDisabled           = errors.New("auth: account disabled")
	ErrInvalidAPIKey      = errors.New("auth: invalid or expired API key")
	ErrNotFound           = errors.New("auth: not found")
	ErrScopeNotAllowed    = errors.New("auth: scope not allowed for this account")
	ErrExpiryInPast       = errors.New("auth: expiry is in the past")
	// ErrNotLinked: no user is linked to a provider login.
	ErrNotLinked = errors.New("auth: no account is linked to this login")
)

// NoPassword is the password hash of a user without a password (created
// by single sign-on). It is no valid hash, so no password matches it.
const NoPassword = "!none"

// HasPassword reports whether a stored password hash is a real one.
func HasPassword(hash string) bool { return hash != NoPassword && hash != "" }

// How a session was started (sessions.auth_method).
const (
	SessionPassword = "password"
	SessionOIDC     = "oidc"
)

// RateLimitedError says a login was refused because of too many failures.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: too many failed logins, try again in %s", e.RetryAfter.Round(time.Second))
}

// Login attempt limits: per username and per client address.
const (
	userFailures = 5
	ipFailures   = 30
	failWindow   = 15 * time.Minute
)

// How often a session's last use and an API key's last use are written.
const (
	sessionTouchEvery = 5 * time.Minute
	apiKeyTouchEvery  = time.Minute
)

// DefaultSessionTTL is how long a session lasts without being used.
const DefaultSessionTTL = 30 * 24 * time.Hour

// Service signs users in and authenticates requests.
type Service struct {
	st         *store.Store
	hasher     *Hasher
	log        *slog.Logger
	now        func() time.Time
	sessionTTL time.Duration
	userFails  *FailureLimiter
	ipFails    *FailureLimiter
	// ssoFails counts failed single sign-ons per client address, apart
	// from password logins: the callback is a GET any website can make a
	// browser send, so it must never lock anyone out of password login.
	ssoFails *FailureLimiter
	// passwordAdminsOnly limits password login to admins.
	passwordAdminsOnly atomic.Bool
}

// SetPasswordAdminsOnly limits password login to admins (with single
// sign-on for everyone else). Admins can always sign in with a password:
// that is the way in when the identity provider is down.
func (s *Service) SetPasswordAdminsOnly(on bool) { s.passwordAdminsOnly.Store(on) }

// PasswordAdminsOnly reports whether password login is limited to admins.
func (s *Service) PasswordAdminsOnly() bool { return s.passwordAdminsOnly.Load() }

// PasswordUsable reports whether u can sign in with a password: they have
// one, and password login isn't limited to admins unless they are one.
func (s *Service) PasswordUsable(u db.User) bool {
	return HasPassword(u.PasswordHash) && (u.Role == RoleAdmin || !s.PasswordAdminsOnly())
}

// NewService returns the auth service.
func NewService(st *store.Store, hasher *Hasher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		st: st, hasher: hasher, log: log, now: time.Now, sessionTTL: DefaultSessionTTL,
		userFails: NewFailureLimiter(userFailures, failWindow),
		ipFails:   NewFailureLimiter(ipFailures, failWindow),
		ssoFails:  NewFailureLimiter(ipFailures, failWindow),
	}
}

// Hasher returns the password hasher.
func (s *Service) Hasher() *Hasher { return s.hasher }

// ClientInfo describes where a request came from.
type ClientInfo struct {
	IP        string
	UserAgent string
}

// NewSession is a freshly created session with its secrets, which exist
// only here: the database keeps a hash of the token.
type NewSession struct {
	Session   db.Session
	Token     string
	CSRFToken string
}

// Login checks a username and password and starts a session.
func (s *Service) Login(ctx context.Context, username, password string, ci ClientInfo) (db.User, NewSession, error) {
	userKey := "u:" + strings.ToLower(username)
	ipKey := "ip:" + ci.IP
	for _, k := range []string{userKey, ipKey} {
		if blocked, wait := s.limiterFor(k).Blocked(k); blocked {
			s.audit(ctx, "login refused: rate limited", "username", username, "ip", ci.IP)
			return db.User{}, NewSession{}, &RateLimitedError{RetryAfter: wait}
		}
	}
	fail := func() (db.User, NewSession, error) {
		s.userFails.Fail(userKey)
		s.ipFails.Fail(ipKey)
		s.log.WarnContext(ctx, "login failed", "component", "audit", "username", username, "ip", ci.IP)
		return db.User{}, NewSession{}, ErrInvalidCredentials
	}

	u, err := s.st.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !HasPassword(u.PasswordHash)) {
		// Unknown, or without a password (single sign-on only): the
		// same time and answer as a wrong password.
		s.hasher.VerifyDummy(ctx, password)
		return fail()
	}
	if err != nil {
		return db.User{}, NewSession{}, err
	}
	ok, rehash, err := s.hasher.Verify(ctx, password, u.PasswordHash)
	if err != nil {
		return db.User{}, NewSession{}, fmt.Errorf("auth: verify password of %s: %w", u.ID, err)
	}
	if u.Role != RoleAdmin && s.PasswordAdminsOnly() {
		// Password login is for admins only. Anyone else gets the
		// answer of a wrong password, whether this one was right or
		// not (and whether the account is disabled), so the answer
		// never confirms a password that can't be used here. The hash
		// was checked above, so the time is the same too.
		if ok {
			s.audit(ctx, "login refused: password login is for admins only", "user_id", u.ID, "ip", ci.IP)
		}
		return fail()
	}
	if !ok {
		return fail()
	}
	if u.Disabled {
		s.audit(ctx, "login refused: account disabled", "user_id", u.ID, "ip", ci.IP)
		return db.User{}, NewSession{}, ErrDisabled
	}
	s.userFails.Reset(userKey)
	if rehash {
		if h, err := s.hasher.Hash(ctx, password); err == nil {
			if err := s.st.SetUserPassword(ctx, db.SetUserPasswordParams{ID: u.ID, PasswordHash: h}); err != nil {
				s.log.WarnContext(ctx, "rehash password", "user_id", u.ID, "err", err)
			}
		}
	}
	sess, err := s.StartSession(ctx, s.st.Queries, u, ci)
	if err != nil {
		return db.User{}, NewSession{}, err
	}
	s.audit(ctx, "login", "user_id", u.ID, "ip", ci.IP)
	return u, sess, nil
}

// Identity is a login at an identity provider: its issuer and subject
// identify it. Email is shown to people, never used to find a user.
type Identity struct {
	Issuer  string
	Subject string
	Email   string
}

// SSOBlocked reports a rate-limit error while the client address has too
// many failed single sign-ons. Failed password logins don't count here,
// and failed single sign-ons don't count against password login.
func (s *Service) SSOBlocked(ctx context.Context, ci ClientInfo) error {
	k := "sso:" + ci.IP
	if blocked, wait := s.ssoFails.Blocked(k); blocked {
		s.audit(ctx, "sso login refused: rate limited", "ip", ci.IP)
		return &RateLimitedError{RetryAfter: wait}
	}
	return nil
}

// SSOFailed records a failed single sign-on that got past the flow's own
// checks (its cookie opened and the state matched), so the browser really
// went through a sign-in: it counts against the client address's single
// sign-on limit. reason is a short code, never a token.
func (s *Service) SSOFailed(ctx context.Context, ci ClientInfo, reason string, args ...any) {
	s.ssoFails.Fail("sso:" + ci.IP)
	s.log.WarnContext(ctx, "sso login failed", append([]any{"component", "audit", "reason", reason, "ip", ci.IP}, args...)...)
}

// SSORefused records a callback refused before it could be a sign-in
// attempt: no flow cookie, one that doesn't open, a state that doesn't
// match, or a provider that can't be reached. Any website can make a
// browser send such a request, so it is logged but never counted.
func (s *Service) SSORefused(ctx context.Context, ci ClientInfo, reason string, args ...any) {
	s.log.InfoContext(ctx, "sso callback refused", append([]any{"component", "audit", "reason", reason, "ip", ci.IP}, args...)...)
}

// StartSSOSession starts a single sign-on session for u with q (so it
// joins the caller's transaction) and records the login.
func (s *Service) StartSSOSession(ctx context.Context, q *db.Queries, u db.User, ci ClientInfo) (NewSession, error) {
	return s.startSession(ctx, q, u, ci, SessionOIDC)
}

// Audit writes an audit log entry.
func (s *Service) Audit(ctx context.Context, msg string, args ...any) { s.audit(ctx, msg, args...) }

func (s *Service) limiterFor(key string) *FailureLimiter {
	if strings.HasPrefix(key, "ip:") {
		return s.ipFails
	}
	return s.userFails
}

// StartSession creates a password session for u with q (so it can join a
// transaction) and records the login.
func (s *Service) StartSession(ctx context.Context, q *db.Queries, u db.User, ci ClientInfo) (NewSession, error) {
	return s.startSession(ctx, q, u, ci, SessionPassword)
}

func (s *Service) startSession(ctx context.Context, q *db.Queries, u db.User, ci ClientInfo, method string) (NewSession, error) {
	token := newToken("")
	csrf := newToken("")
	sess, err := q.CreateSession(ctx, db.CreateSessionParams{
		TokenHash:  hashToken(token),
		UserID:     u.ID,
		CsrfToken:  csrf,
		UserAgent:  truncate(ci.UserAgent, 300),
		Ip:         ci.IP,
		ExpiresAt:  s.now().Add(s.sessionTTL),
		AuthMethod: method,
	})
	if err != nil {
		return NewSession{}, fmt.Errorf("auth: create session: %w", err)
	}
	if err := q.TouchUserLogin(ctx, u.ID); err != nil {
		return NewSession{}, err
	}
	return NewSession{Session: sess, Token: token, CSRFToken: csrf}, nil
}

// SessionTTL is how long a session lasts without being used.
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// AuthenticateSession resolves a session token. It returns nil without an
// error when the token is unknown or expired, or the user is disabled:
// the request is then anonymous. Sessions slide: using one extends it.
func (s *Service) AuthenticateSession(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, nil
	}
	row, err := s.st.GetSessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.User.Disabled {
		return nil, nil
	}
	if now := s.now(); now.Sub(row.Session.LastSeenAt) > sessionTouchEvery {
		if err := s.st.TouchSession(ctx, db.TouchSessionParams{ID: row.Session.ID, ExpiresAt: now.Add(s.sessionTTL)}); err != nil {
			s.log.WarnContext(ctx, "touch session", "err", err)
		}
	}
	return &Principal{
		User: row.User, Method: MethodSession,
		SessionID: row.Session.ID, CSRFToken: row.Session.CsrfToken, SessionAuth: row.Session.AuthMethod,
		Scopes: RoleScopes(row.User.Role),
	}, nil
}

// AuthenticateAPIKey resolves a personal API key. Unknown, expired and
// disabled users' keys are ErrInvalidAPIKey: a request that sends a key
// must not silently become anonymous.
func (s *Service) AuthenticateAPIKey(ctx context.Context, key string) (*Principal, error) {
	if !strings.HasPrefix(key, APIKeyPrefix) {
		return nil, ErrInvalidAPIKey
	}
	row, err := s.st.GetAPIKeyByHash(ctx, hashToken(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidAPIKey
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if row.User.Disabled || (row.ApiKey.ExpiresAt != nil && !now.Before(*row.ApiKey.ExpiresAt)) {
		return nil, ErrInvalidAPIKey
	}
	if row.ApiKey.LastUsedAt == nil || now.Sub(*row.ApiKey.LastUsedAt) > apiKeyTouchEvery {
		if err := s.st.TouchAPIKey(ctx, row.ApiKey.ID); err != nil {
			s.log.WarnContext(ctx, "touch api key", "err", err)
		}
	}
	// A key never holds more than its owner's role allows, even if the
	// owner was demoted after creating it.
	allowed := RoleScopes(row.User.Role)
	scopes := slices.DeleteFunc(slices.Clone(row.ApiKey.Scopes), func(sc string) bool { return !slices.Contains(allowed, sc) })
	return &Principal{User: row.User, Method: MethodAPIKey, APIKeyID: row.ApiKey.ID, Scopes: scopes}, nil
}

// CheckCSRF reports whether token is the session's CSRF token.
func (p *Principal) CheckCSRF(token string) bool {
	return p.CSRFToken != "" && subtle.ConstantTimeCompare([]byte(p.CSRFToken), []byte(token)) == 1
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, p *Principal) error {
	if err := s.st.DeleteSession(ctx, p.SessionID); err != nil {
		return err
	}
	s.audit(ctx, "logout", "user_id", p.User.ID)
	return nil
}

// ListSessions lists a user's live sessions.
func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID) ([]db.Session, error) {
	return s.st.ListSessionsByUser(ctx, userID)
}

// DeleteSession ends one of the user's sessions.
func (s *Service) DeleteSession(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.st.DeleteUserSession(ctx, db.DeleteUserSessionParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangePassword changes the caller's password after checking the current
// one, and ends their other sessions. A user without a password (created
// by single sign-on) sets their first one without a current password.
func (s *Service) ChangePassword(ctx context.Context, p *Principal, current, next string) error {
	u, err := s.st.GetUser(ctx, p.User.ID)
	if err != nil {
		return err
	}
	if HasPassword(u.PasswordHash) {
		ok, _, err := s.hasher.Verify(ctx, current, u.PasswordHash)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidCredentials
		}
	}
	h, err := s.hasher.Hash(ctx, next)
	if err != nil {
		return err
	}
	err = s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.SetUserPassword(ctx, db.SetUserPasswordParams{ID: u.ID, PasswordHash: h}); err != nil {
			return err
		}
		return q.DeleteUserSessionsExcept(ctx, db.DeleteUserSessionsExceptParams{UserID: u.ID, Keep: p.SessionID})
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "password changed", "user_id", u.ID)
	return nil
}

// CreateAPIKey creates a key for u. The key itself is returned only here.
func (s *Service) CreateAPIKey(ctx context.Context, u db.User, name string, scopes []string, expiresAt *time.Time) (db.ApiKey, string, error) {
	allowed := RoleScopes(u.Role)
	clean := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if !slices.Contains(allowed, sc) {
			return db.ApiKey{}, "", fmt.Errorf("%w: %s", ErrScopeNotAllowed, sc)
		}
		if !slices.Contains(clean, sc) {
			clean = append(clean, sc)
		}
	}
	if expiresAt != nil && !expiresAt.After(s.now()) {
		return db.ApiKey{}, "", ErrExpiryInPast
	}
	key := newToken(APIKeyPrefix)
	k, err := s.st.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		UserID: u.ID, Name: name, Prefix: key[:apiKeyShownLen], KeyHash: hashToken(key),
		Scopes: clean, ExpiresAt: expiresAt,
	})
	if err != nil {
		return db.ApiKey{}, "", err
	}
	s.audit(ctx, "api key created", "user_id", u.ID, "api_key_id", k.ID, "scopes", strings.Join(clean, ","))
	return k, key, nil
}

// ListAPIKeys lists a user's keys.
func (s *Service) ListAPIKeys(ctx context.Context, userID uuid.UUID) ([]db.ApiKey, error) {
	return s.st.ListAPIKeysByUser(ctx, userID)
}

// DeleteAPIKey revokes one of the user's keys.
func (s *Service) DeleteAPIKey(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.st.DeleteUserAPIKey(ctx, db.DeleteUserAPIKeyParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.audit(ctx, "api key revoked", "user_id", userID, "api_key_id", id)
	return nil
}

// CleanExpired deletes expired sessions.
func (s *Service) CleanExpired(ctx context.Context) (int64, error) {
	return s.st.DeleteExpiredSessions(ctx)
}

func (s *Service) audit(ctx context.Context, msg string, args ...any) {
	s.log.InfoContext(ctx, msg, append([]any{"component", "audit"}, args...)...)
}

// truncate cuts s to at most n bytes of valid UTF-8 (header values may be
// anything; the database only takes valid text).
func truncate(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
