// SPDX-License-Identifier: GPL-3.0-or-later

// Package users manages accounts: creating, changing and deleting users,
// their grants, invites and the first-run setup that creates the admin.
package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/store/db"
)

// Errors callers map to responses.
var (
	ErrNotFound         = errors.New("users: not found")
	ErrUsernameTaken    = errors.New("users: username taken")
	ErrLastAdmin        = errors.New("users: the last enabled admin can't be disabled, demoted or deleted")
	ErrCannotChangeSelf = errors.New("users: admins can't disable, demote or delete themselves")
	ErrAlreadySetUp     = errors.New("users: the first account already exists")
	ErrInviteInvalid    = errors.New("users: invite unknown, used or expired")
)

// Content ratings, mildest first.
var ContentRatings = []string{"safe", "suggestive", "erotic", "pornographic"}

// Grants say what a user may do. Admins may do everything regardless.
type Grants struct {
	MayAdd               bool     `json:"may_add"`
	MayDownload          bool     `json:"may_download"`
	MayManageMatches     bool     `json:"may_manage_matches"`
	ContentRatingCeiling string   `json:"content_rating_ceiling"`
	AllSources           bool     `json:"all_sources"`
	AllowedSources       []string `json:"allowed_sources"`
}

// DefaultGrants are a new user's grants unless the admin sets others:
// browse every source, add to their own library and read, up to
// suggestive content; no downloads and no match fixing.
func DefaultGrants() Grants {
	return Grants{MayAdd: true, ContentRatingCeiling: "suggestive", AllSources: true, AllowedSources: []string{}}
}

// AdminGrants are what an admin may do: everything.
func AdminGrants() Grants {
	return Grants{MayAdd: true, MayDownload: true, MayManageMatches: true, ContentRatingCeiling: "pornographic", AllSources: true, AllowedSources: []string{}}
}

// GrantsPatch changes some grants; nil fields stay.
type GrantsPatch struct {
	MayAdd               *bool
	MayDownload          *bool
	MayManageMatches     *bool
	ContentRatingCeiling *string
	AllSources           *bool
	AllowedSources       *[]string
}

// Apply returns g with p applied.
func (p *GrantsPatch) Apply(g Grants) Grants {
	if p == nil {
		return g
	}
	set := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	set(&g.MayAdd, p.MayAdd)
	set(&g.MayDownload, p.MayDownload)
	set(&g.MayManageMatches, p.MayManageMatches)
	set(&g.AllSources, p.AllSources)
	if p.ContentRatingCeiling != nil {
		g.ContentRatingCeiling = *p.ContentRatingCeiling
	}
	if p.AllowedSources != nil {
		g.AllowedSources = slices.Compact(slices.Sorted(slices.Values(*p.AllowedSources)))
	}
	if g.AllowedSources == nil {
		g.AllowedSources = []string{}
	}
	return g
}

// Validate checks the grants' values.
func (g Grants) Validate() error {
	if !slices.Contains(ContentRatings, g.ContentRatingCeiling) {
		return fmt.Errorf("users: unknown content rating %q", g.ContentRatingCeiling)
	}
	return nil
}

// GrantsOf returns u's stored grants.
func GrantsOf(u db.User) Grants {
	allowed := u.AllowedSources
	if allowed == nil {
		allowed = []string{}
	}
	return Grants{
		MayAdd: u.MayAdd, MayDownload: u.MayDownload, MayManageMatches: u.MayManageMatches,
		ContentRatingCeiling: u.ContentRatingCeiling, AllSources: u.AllSources, AllowedSources: allowed,
	}
}

// Effective returns what u may actually do: admins get everything.
func Effective(u db.User) Grants {
	if u.Role == auth.RoleAdmin {
		return AdminGrants()
	}
	return GrantsOf(u)
}

// Service manages accounts.
type Service struct {
	st     *store.Store
	auth   *auth.Service
	log    *slog.Logger
	now    func() time.Time
	hasAny atomic.Bool // once true, stays true: the last admin can't be deleted
}

// NewService returns the users service.
func NewService(st *store.Store, a *auth.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{st: st, auth: a, log: log, now: time.Now}
}

// NewUser is a user to create.
type NewUser struct {
	Username    string
	Password    string
	DisplayName string
	Role        string
	Grants      Grants
}

func (s *Service) create(ctx context.Context, q *db.Queries, n NewUser) (db.User, error) {
	if n.Role == "" {
		n.Role = auth.RoleUser
	}
	if n.Role != auth.RoleAdmin && n.Role != auth.RoleUser {
		return db.User{}, fmt.Errorf("users: unknown role %q", n.Role)
	}
	if err := n.Grants.Validate(); err != nil {
		return db.User{}, err
	}
	hash, err := s.auth.Hasher().Hash(ctx, n.Password)
	if err != nil {
		return db.User{}, err
	}
	g := n.Grants
	u, err := q.CreateUser(ctx, db.CreateUserParams{
		Username: n.Username, DisplayName: strings.TrimSpace(n.DisplayName), PasswordHash: hash, Role: n.Role,
		MayAdd: g.MayAdd, MayDownload: g.MayDownload, MayManageMatches: g.MayManageMatches,
		ContentRatingCeiling: g.ContentRatingCeiling, AllSources: g.AllSources, AllowedSources: g.AllowedSources,
	})
	if isUniqueViolation(err) {
		return db.User{}, ErrUsernameTaken
	}
	if err != nil {
		return db.User{}, err
	}
	s.hasAny.Store(true)
	return u, nil
}

// HasUsers reports whether any account exists (the first-run gate).
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	if s.hasAny.Load() {
		return true, nil
	}
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		s.hasAny.Store(true)
	}
	return n > 0, nil
}

// CreateUser creates a user (an admin's action).
func (s *Service) CreateUser(ctx context.Context, actor uuid.UUID, n NewUser) (db.User, error) {
	u, err := s.create(ctx, s.st.Queries, n)
	if err != nil {
		return db.User{}, err
	}
	s.audit(ctx, "user created", "actor", actor, "user_id", u.ID, "role", u.Role)
	return u, nil
}

// GetUser returns a user.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (db.User, error) {
	u, err := s.st.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, ErrNotFound
	}
	return u, err
}

// ListUsers returns up to limit users after the cursor (a lowercased
// username), and the cursor of the next page ("" on the last).
func (s *Service) ListUsers(ctx context.Context, after string, limit int) ([]db.User, string, error) {
	rows, err := s.st.ListUsers(ctx, db.ListUsersParams{After: after, Lim: int32(limit + 1)})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = strings.ToLower(rows[len(rows)-1].Username)
	}
	return rows, next, nil
}

// Patch changes some of a user's fields; nil fields stay.
type Patch struct {
	DisplayName *string
	Role        *string
	Disabled    *bool
	Password    *string
	Grants      *GrantsPatch
}

// UpdateUser changes a user (an admin's action, or the user's own display
// name). Disabling or resetting the password ends the user's sessions.
func (s *Service) UpdateUser(ctx context.Context, actor, id uuid.UUID, p Patch) (db.User, error) {
	var hash string
	if p.Password != nil {
		h, err := s.auth.Hasher().Hash(ctx, *p.Password)
		if err != nil {
			return db.User{}, err
		}
		hash = h
	}
	var out db.User
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockAccounts(ctx); err != nil {
			return err
		}
		u, err := q.GetUserForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		role, disabled := u.Role, u.Disabled
		if p.Role != nil {
			role = *p.Role
		}
		if p.Disabled != nil {
			disabled = *p.Disabled
		}
		if role != auth.RoleAdmin && role != auth.RoleUser {
			return fmt.Errorf("users: unknown role %q", role)
		}
		losesAdmin := u.Role == auth.RoleAdmin && !u.Disabled && (role != auth.RoleAdmin || disabled)
		if losesAdmin {
			if id == actor {
				return ErrCannotChangeSelf
			}
			n, err := q.CountEnabledAdmins(ctx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		g := p.Grants.Apply(GrantsOf(u))
		if err := g.Validate(); err != nil {
			return err
		}
		name := u.DisplayName
		if p.DisplayName != nil {
			name = strings.TrimSpace(*p.DisplayName)
		}
		out, err = q.UpdateUser(ctx, db.UpdateUserParams{
			ID: id, DisplayName: name, Role: role, Disabled: disabled,
			MayAdd: g.MayAdd, MayDownload: g.MayDownload, MayManageMatches: g.MayManageMatches,
			ContentRatingCeiling: g.ContentRatingCeiling, AllSources: g.AllSources, AllowedSources: g.AllowedSources,
		})
		if err != nil {
			return err
		}
		if hash != "" {
			if err := q.SetUserPassword(ctx, db.SetUserPasswordParams{ID: id, PasswordHash: hash}); err != nil {
				return err
			}
		}
		if hash != "" || (disabled && !u.Disabled) {
			return q.DeleteUserSessions(ctx, id)
		}
		return nil
	})
	if err != nil {
		return db.User{}, err
	}
	if actor != id || p.Role != nil || p.Disabled != nil || p.Password != nil || p.Grants != nil {
		s.audit(ctx, "user updated", "actor", actor, "user_id", id, "fields", patchFields(p))
	}
	return out, nil
}

func patchFields(p Patch) string {
	var f []string
	if p.DisplayName != nil {
		f = append(f, "display_name")
	}
	if p.Role != nil {
		f = append(f, "role")
	}
	if p.Disabled != nil {
		f = append(f, "disabled")
	}
	if p.Password != nil {
		f = append(f, "password")
	}
	if p.Grants != nil {
		f = append(f, "grants")
	}
	return strings.Join(f, ",")
}

// DeleteUser deletes a user with their sessions and keys.
func (s *Service) DeleteUser(ctx context.Context, actor, id uuid.UUID) error {
	if actor == id {
		return ErrCannotChangeSelf
	}
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockAccounts(ctx); err != nil {
			return err
		}
		u, err := q.GetUserForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.Role == auth.RoleAdmin && !u.Disabled {
			n, err := q.CountEnabledAdmins(ctx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		_, err = q.DeleteUser(ctx, id)
		return err
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "user deleted", "actor", actor, "user_id", id)
	return nil
}

func (s *Service) audit(ctx context.Context, msg string, args ...any) {
	s.log.InfoContext(ctx, msg, append([]any{"component", "audit"}, args...)...)
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
