// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth authenticates callers (browser sessions and personal API
// keys), hashes passwords, limits login attempts and decides which scopes
// a caller holds.
package auth

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/panelarr/server/internal/store/db"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// API key scopes.
const (
	// ScopeRead allows GET requests.
	ScopeRead = "read"
	// ScopeWrite allows requests that change something.
	ScopeWrite = "write"
	// ScopeAdmin allows admin endpoints (admins' keys only).
	ScopeAdmin = "admin"
	// ScopeMetricsRead allows scraping /metrics on the API port (admins'
	// keys only).
	ScopeMetricsRead = "metrics:read"
)

// KnownScopes lists every scope, in display order.
var KnownScopes = []string{ScopeRead, ScopeWrite, ScopeAdmin, ScopeMetricsRead}

// RoleScopes are the scopes a role may hold. A session holds all of them;
// an API key holds the ones it was created with.
func RoleScopes(role string) []string {
	if role == RoleAdmin {
		return slices.Clone(KnownScopes)
	}
	return []string{ScopeRead, ScopeWrite}
}

// Method is how a caller authenticated.
type Method string

const (
	MethodSession Method = "session"
	MethodAPIKey  Method = "api_key"
)

// Principal is an authenticated caller.
type Principal struct {
	User   db.User
	Method Method
	// SessionID and CSRFToken are set for sessions.
	SessionID uuid.UUID
	CSRFToken string
	// SessionAuth is how the session was started: SessionPassword or
	// SessionOIDC.
	SessionAuth string
	// APIKeyID is set for API keys.
	APIKeyID uuid.UUID
	// Scopes the caller holds.
	Scopes []string
}

// IsAdmin reports whether the caller is an admin.
func (p *Principal) IsAdmin() bool { return p != nil && p.User.Role == RoleAdmin }

// HasScope reports whether the caller holds scope.
func (p *Principal) HasScope(scope string) bool {
	return p != nil && slices.Contains(p.Scopes, scope)
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the caller, or nil for anonymous requests.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
