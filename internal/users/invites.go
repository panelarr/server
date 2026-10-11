// SPDX-License-Identifier: GPL-3.0-or-later

package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/store/db"
)

// DefaultInviteTTL is how long an invite stays valid unless set.
const DefaultInviteTTL = 72 * time.Hour

// NewInvite is an invite to create.
type NewInvite struct {
	Role   string
	Grants Grants
	Note   string
	TTL    time.Duration
}

// CreateInvite creates an invite. Its token is returned only here.
func (s *Service) CreateInvite(ctx context.Context, actor uuid.UUID, n NewInvite) (db.Invite, string, error) {
	if n.Role == "" {
		n.Role = auth.RoleUser
	}
	if n.Role != auth.RoleAdmin && n.Role != auth.RoleUser {
		return db.Invite{}, "", fmt.Errorf("users: unknown role %q", n.Role)
	}
	if err := n.Grants.Validate(); err != nil {
		return db.Invite{}, "", err
	}
	if n.TTL <= 0 {
		n.TTL = DefaultInviteTTL
	}
	grants, err := json.Marshal(n.Grants)
	if err != nil {
		return db.Invite{}, "", err
	}
	token, hash := auth.NewInviteToken()
	inv, err := s.st.CreateInvite(ctx, db.CreateInviteParams{
		TokenHash: hash, Role: n.Role, Grants: grants, Note: n.Note,
		CreatedBy: uuid.NullUUID{UUID: actor, Valid: true}, ExpiresAt: s.now().Add(n.TTL),
	})
	if err != nil {
		return db.Invite{}, "", err
	}
	s.audit(ctx, "invite created", "actor", actor, "invite_id", inv.ID, "role", inv.Role)
	return inv, token, nil
}

// InviteGrants decodes an invite's grants.
func InviteGrants(inv db.Invite) (Grants, error) {
	var g Grants
	if err := json.Unmarshal(inv.Grants, &g); err != nil {
		return Grants{}, fmt.Errorf("users: invite %s grants: %w", inv.ID, err)
	}
	if g.AllowedSources == nil {
		g.AllowedSources = []string{}
	}
	return g, nil
}

// ListInvites lists open invites.
func (s *Service) ListInvites(ctx context.Context) ([]db.Invite, error) {
	return s.st.ListOpenInvites(ctx)
}

// DeleteInvite revokes an invite.
func (s *Service) DeleteInvite(ctx context.Context, actor, id uuid.UUID) error {
	n, err := s.st.DeleteInvite(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.audit(ctx, "invite revoked", "actor", actor, "invite_id", id)
	return nil
}

// CheckInvite returns the open invite for token.
func (s *Service) CheckInvite(ctx context.Context, token string) (db.Invite, error) {
	inv, err := s.st.GetOpenInviteByTokenHash(ctx, auth.HashInviteToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Invite{}, ErrInviteInvalid
	}
	return inv, err
}

// AcceptInvite creates the account an invite allows, uses the invite up
// and signs the new user in. n's role and grants are ignored: they come
// from the invite.
func (s *Service) AcceptInvite(ctx context.Context, token string, n NewUser, ci auth.ClientInfo) (db.User, auth.NewSession, error) {
	var (
		u    db.User
		sess auth.NewSession
	)
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		inv, err := q.GetOpenInviteByTokenHashForUpdate(ctx, auth.HashInviteToken(token))
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
		n.Role, n.Grants = inv.Role, g
		if u, err = s.create(ctx, q, n); err != nil {
			return err
		}
		if err := q.MarkInviteUsed(ctx, db.MarkInviteUsedParams{ID: inv.ID, UsedBy: uuid.NullUUID{UUID: u.ID, Valid: true}}); err != nil {
			return err
		}
		sess, err = s.auth.StartSession(ctx, q, u, ci)
		return err
	})
	if err != nil {
		return db.User{}, auth.NewSession{}, err
	}
	s.audit(ctx, "invite accepted", "user_id", u.ID, "role", u.Role, "ip", ci.IP)
	return u, sess, nil
}
