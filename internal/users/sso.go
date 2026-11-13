// SPDX-License-Identifier: GPL-3.0-or-later

package users

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/store/db"
)

// Errors of single sign-on, linking and unlinking.
var (
	// ErrAlreadyLinked: the login is linked to another user.
	ErrAlreadyLinked = errors.New("users: this login is linked to another account")
	// ErrLinkExists: the user already has a login at this provider.
	ErrLinkExists = errors.New("users: a login at this provider is already linked")
	// ErrLastLoginMethod: unlinking would leave the user no way to sign
	// in.
	ErrLastLoginMethod = errors.New("users: this is the user's last way to sign in")
)

// SSOLogin is a sign-in the identity provider confirmed: who it is, and
// what the browser's flow was for.
type SSOLogin struct {
	// Identity is the provider login (issuer and subject).
	Identity auth.Identity
	// Name is the display name the provider reports.
	Name string
	// Usernames are candidates for a new user's username, best first.
	Usernames []string
	// Groups the user is in; GroupsKnown is false when the provider
	// didn't say at all.
	Groups      []string
	GroupsKnown bool

	// LinkTo, when set, links the login to this (signed-in) user
	// instead of signing in.
	LinkTo uuid.UUID
	// InviteToken creates the user an invite allows.
	InviteToken string

	// AutoProvision creates a user for an unknown login.
	AutoProvision bool
	// AdminGroups, when set, decide the user's role (admin for members,
	// user otherwise) on every sign-in.
	AdminGroups []string
}

// SSOOutcome is what a single sign-on did.
type SSOOutcome struct {
	User db.User
	// Session is the new session; nil when the login was linked.
	Session *auth.NewSession
	// Linked: the login was linked to the user just now.
	Linked bool
	// Created: the user was created just now (invite or provisioning).
	Created bool
}

// SignInSSO applies the design's callback rules 3–8 to a login the
// provider confirmed (the flow and allowed groups are checked already):
//
//  3. a linked login signs its user in (refused when disabled);
//  4. with LinkTo, the login is linked to that user;
//  5. with an invite, the invite's user is created and linked;
//  6. with auto-provisioning, a user is created and linked;
//  7. otherwise it is refused (auth.ErrNotLinked);
//  8. with admin groups, the role is synced, never demoting the last
//     enabled admin; the session starts with method "oidc".
//
// Linking never signs in as another user: a login linked to someone else
// is refused with ErrAlreadyLinked. Everything runs in one transaction
// under the accounts lock, so the "last admin" count and username choice
// can't race.
func (s *Service) SignInSSO(ctx context.Context, in SSOLogin, ci auth.ClientInfo) (SSOOutcome, error) {
	var (
		out      SSOOutcome
		identity uuid.UUID
		synced   string
	)
	in.Identity.Email = cleanText(in.Identity.Email, 320)
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockAccounts(ctx); err != nil {
			return err
		}
		row, err := q.GetUserByIdentity(ctx, db.GetUserByIdentityParams{Issuer: in.Identity.Issuer, Subject: in.Identity.Subject})
		linked := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var u db.User
		switch {
		case in.LinkTo != uuid.Nil:
			if linked {
				if row.User.ID != in.LinkTo {
					return ErrAlreadyLinked
				}
				out.User = row.User // linked already: nothing to do
				return nil
			}
			u, err = q.GetUserForUpdate(ctx, in.LinkTo)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if u.Disabled {
				return auth.ErrDisabled
			}
			n, err := q.CountUserIdentitiesAtIssuer(ctx, db.CountUserIdentitiesAtIssuerParams{UserID: u.ID, Issuer: in.Identity.Issuer})
			if err != nil {
				return err
			}
			if n > 0 {
				return ErrLinkExists
			}
			if identity, err = s.link(ctx, q, u.ID, in.Identity); err != nil {
				return err
			}
			out.User, out.Linked = u, true
			return nil
		case linked:
			u, identity = row.User, row.UserIdentity.ID
			if u.Disabled {
				return auth.ErrDisabled
			}
			if err := q.TouchUserIdentity(ctx, db.TouchUserIdentityParams{ID: identity, Email: in.Identity.Email}); err != nil {
				return err
			}
		case in.InviteToken != "":
			inv, err := q.GetOpenInviteByTokenHashForUpdate(ctx, auth.HashInviteToken(in.InviteToken))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInviteInvalid
			}
			if err != nil {
				return err
			}
			g, err := InviteGrants(inv)
			if err != nil {
				return err
			}
			if u, identity, err = s.provision(ctx, q, in, inv.Role, g); err != nil {
				return err
			}
			if err := q.MarkInviteUsed(ctx, db.MarkInviteUsedParams{ID: inv.ID, UsedBy: uuid.NullUUID{UUID: u.ID, Valid: true}}); err != nil {
				return err
			}
			s.audit(ctx, "invite accepted", "method", auth.SessionOIDC, "user_id", u.ID, "invite_id", inv.ID, "role", u.Role, "ip", ci.IP)
			out.Created = true
		case in.AutoProvision:
			role := auth.RoleUser
			if len(in.AdminGroups) > 0 && in.GroupsKnown && inAny(in.Groups, in.AdminGroups) {
				role = auth.RoleAdmin
			}
			if u, identity, err = s.provision(ctx, q, in, role, DefaultGrants()); err != nil {
				return err
			}
			s.audit(ctx, "user provisioned", "user_id", u.ID, "identity_id", identity, "role", u.Role, "ip", ci.IP)
			out.Created = true
		default:
			return auth.ErrNotLinked
		}
		if u, synced, err = s.syncRole(ctx, q, u, in); err != nil {
			return err
		}
		sess, err := s.auth.StartSSOSession(ctx, q, u, ci)
		if err != nil {
			return err
		}
		out.User, out.Session = u, &sess
		return nil
	})
	if err != nil {
		return SSOOutcome{}, err
	}
	switch {
	case out.Linked:
		s.audit(ctx, "identity linked", "user_id", out.User.ID, "identity_id", identity, "issuer", in.Identity.Issuer, "ip", ci.IP)
	case out.Session != nil:
		if synced != "" {
			s.audit(ctx, "role synced from groups", "user_id", out.User.ID, "from", synced, "to", out.User.Role)
		}
		s.audit(ctx, "login", "method", auth.SessionOIDC, "user_id", out.User.ID, "identity_id", identity, "ip", ci.IP)
	}
	return out, nil
}

// link records a login for userID.
func (s *Service) link(ctx context.Context, q *db.Queries, userID uuid.UUID, id auth.Identity) (uuid.UUID, error) {
	row, err := q.CreateUserIdentity(ctx, db.CreateUserIdentityParams{
		UserID: userID, Issuer: id.Issuer, Subject: id.Subject, Email: id.Email,
	})
	if isUniqueViolation(err) {
		return uuid.Nil, ErrAlreadyLinked
	}
	if err != nil {
		return uuid.Nil, err
	}
	// The link was made by signing in with it.
	if err := q.TouchUserIdentity(ctx, db.TouchUserIdentityParams{ID: row.ID, Email: id.Email}); err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// provision creates a user without a password for a provider login and
// links the login.
func (s *Service) provision(ctx context.Context, q *db.Queries, in SSOLogin, role string, g Grants) (db.User, uuid.UUID, error) {
	name, err := freeUsername(ctx, q, in.Usernames)
	if err != nil {
		return db.User{}, uuid.Nil, err
	}
	u, err := s.create(ctx, q, NewUser{
		Username: name, DisplayName: cleanText(in.Name, 100), Role: role, Grants: g, NoPassword: true,
	})
	if err != nil {
		return db.User{}, uuid.Nil, err
	}
	id, err := s.link(ctx, q, u.ID, in.Identity)
	return u, id, err
}

// syncRole sets u's role from the login's groups when admin groups are
// configured: admin for members, user for everyone else. It never demotes
// the last enabled admin, and changes nothing when the provider didn't
// send the groups claim at all (a missing claim is a configuration
// mistake, not "in no group"). It returns the role before a change, or ""
// when nothing changed.
func (s *Service) syncRole(ctx context.Context, q *db.Queries, u db.User, in SSOLogin) (db.User, string, error) {
	if len(in.AdminGroups) == 0 {
		return u, "", nil
	}
	if !in.GroupsKnown {
		s.log.WarnContext(ctx, "role not synced: the provider sent no groups claim", "component", "audit", "user_id", u.ID)
		return u, "", nil
	}
	want := auth.RoleUser
	if inAny(in.Groups, in.AdminGroups) {
		want = auth.RoleAdmin
	}
	if want == u.Role {
		return u, "", nil
	}
	if u.Role == auth.RoleAdmin && !u.Disabled {
		n, err := q.CountEnabledAdmins(ctx)
		if err != nil {
			return u, "", err
		}
		if s.adminsCounted != nil {
			s.adminsCounted()
		}
		if n <= 1 {
			s.log.WarnContext(ctx, "role not synced: the last enabled admin is never demoted", "component", "audit", "user_id", u.ID)
			return u, "", nil
		}
	}
	was := u.Role
	u, err := q.SetUserRole(ctx, db.SetUserRoleParams{ID: u.ID, Role: want})
	if err != nil {
		return u, "", err
	}
	return u, was, nil
}

func inAny(groups, want []string) bool {
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(want, g) })
}

// Username rules (the users table's check): a letter or digit, then
// 1–63 letters, digits, dots, underscores or dashes.
const (
	maxUsername = 64
	// suffixRoom leaves space for "-NNNN" when a name is taken.
	suffixRoom   = 6
	maxNameTries = 1000
)

// usernameFrom cuts s to the characters a username allows: other
// characters are dropped, and the result starts with a letter or digit.
// It returns "" when fewer than two characters remain.
func usernameFrom(s string) string {
	var b strings.Builder
	for _, r := range s {
		ok := r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-')
		if !ok || (b.Len() == 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > maxUsername-suffixRoom {
		out = out[:maxUsername-suffixRoom]
	}
	if len(out) < 2 {
		return ""
	}
	return out
}

// freeUsername picks the first usable candidate and adds "-2", "-3"… while
// the name is taken. The caller holds the accounts lock.
func freeUsername(ctx context.Context, q *db.Queries, candidates []string) (string, error) {
	base := ""
	for _, c := range candidates {
		if base = usernameFrom(c); base != "" {
			break
		}
	}
	if base == "" {
		base = "user"
	}
	for i := 1; i <= maxNameTries; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		_, err := q.GetUserByUsername(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("users: no free username for %q", base)
}

// cleanText keeps a provider's text storable and short: valid UTF-8,
// no control characters, at most n characters.
func cleanText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

// ListIdentities lists a user's linked logins, oldest first.
func (s *Service) ListIdentities(ctx context.Context, userID uuid.UUID) ([]db.UserIdentity, error) {
	return s.st.ListUserIdentities(ctx, userID)
}

// LinkedUsers reports which of ids have a linked login.
func (s *Service) LinkedUsers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.st.UsersWithIdentities(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, id := range rows {
		out[id] = true
	}
	return out, nil
}

// Unlink removes one of a user's linked logins. It is refused with
// ErrLastLoginMethod when the user would have no way left to sign in: no
// other login and no password they may use. An admin's unlink
// (endSessions) also ends the user's single sign-on sessions.
func (s *Service) Unlink(ctx context.Context, actor, userID, identityID uuid.UUID, endSessions bool) error {
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockAccounts(ctx); err != nil {
			return err
		}
		u, err := q.GetUserForUpdate(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		ids, err := q.ListUserIdentities(ctx, userID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(ids, func(i db.UserIdentity) bool { return i.ID == identityID }) {
			return ErrNotFound
		}
		if len(ids) == 1 && !s.auth.PasswordUsable(u) {
			return ErrLastLoginMethod
		}
		if _, err := q.DeleteUserIdentity(ctx, db.DeleteUserIdentityParams{ID: identityID, UserID: userID}); err != nil {
			return err
		}
		if endSessions {
			_, err = q.DeleteUserSessionsByMethod(ctx, db.DeleteUserSessionsByMethodParams{UserID: userID, AuthMethod: auth.SessionOIDC})
		}
		return err
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "identity unlinked", "actor", actor, "user_id", userID, "identity_id", identityID)
	return nil
}
