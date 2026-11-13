// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"errors"
	"net/http"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/users"
)

// domainError maps the domain packages' errors to problems. Unknown errors
// pass through and become a 500.
func domainError(err error) error {
	var rl *auth.RateLimitedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rl):
		return &Problem{Status: http.StatusTooManyRequests, Code: "rate_limited", Detail: "too many failed attempts; try again later"}
	case errors.Is(err, auth.ErrInvalidCredentials):
		return NewProblem(http.StatusUnauthorized, "invalid_credentials", "wrong username or password")
	case errors.Is(err, auth.ErrDisabled):
		return NewProblem(http.StatusForbidden, "account_disabled", "this account is disabled")
	case errors.Is(err, auth.ErrScopeNotAllowed):
		return fieldProblem("scopes", "the admin and metrics:read scopes need an admin account")
	case errors.Is(err, auth.ErrExpiryInPast):
		return fieldProblem("expires_at", "must be in the future")
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, users.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, users.ErrUsernameTaken):
		return &Problem{Status: http.StatusConflict, Code: "username_taken", Detail: "that username is taken"}
	case errors.Is(err, users.ErrLastAdmin):
		return NewProblem(http.StatusConflict, "last_admin", "the last enabled admin can't be disabled, demoted or deleted")
	case errors.Is(err, users.ErrCannotChangeSelf):
		return NewProblem(http.StatusConflict, "cannot_change_self", "admins can't disable, demote or delete themselves")
	case errors.Is(err, users.ErrAlreadySetUp):
		return NewProblem(http.StatusConflict, "already_set_up", "the first account already exists")
	case errors.Is(err, users.ErrInviteInvalid):
		return NewProblem(http.StatusNotFound, "invite_invalid", "the invite is unknown, used or expired")
	case errors.Is(err, auth.ErrPasswordAdminsOnly):
		return NewProblem(http.StatusForbidden, "password_login_admins_only", "only admins may sign in with a password; sign in with single sign-on")
	case errors.Is(err, users.ErrLastLoginMethod):
		return NewProblem(http.StatusConflict, "last_login_method", "this is the last way to sign in; set a password first")
	}
	return err
}
