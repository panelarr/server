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

// settingSetupCompleted holds when an admin finished the first-run wizard.
const settingSetupCompleted = "setup.completed_at"

// SetupStatus is where the first-run wizard stands.
type SetupStatus struct {
	AdminCreated bool
	CompletedAt  *time.Time
}

// SetupStatus reports the wizard's state.
func (s *Service) SetupStatus(ctx context.Context) (SetupStatus, error) {
	has, err := s.HasUsers(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	st := SetupStatus{AdminCreated: has}
	raw, err := s.st.GetSetting(ctx, settingSetupCompleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return SetupStatus{}, err
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return SetupStatus{}, fmt.Errorf("users: setting %s: %w", settingSetupCompleted, err)
	}
	st.CompletedAt = &t
	return st, nil
}

// CreateFirstAdmin creates the first account as an admin and signs it in.
// Once any account exists it fails with ErrAlreadySetUp, also when two
// requests race.
func (s *Service) CreateFirstAdmin(ctx context.Context, n NewUser, ci auth.ClientInfo) (db.User, auth.NewSession, error) {
	n.Role = auth.RoleAdmin
	n.Grants = AdminGrants()
	var (
		u    db.User
		sess auth.NewSession
	)
	err := s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockAccounts(ctx); err != nil {
			return err
		}
		count, err := q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrAlreadySetUp
		}
		if u, err = s.create(ctx, q, n); err != nil {
			return err
		}
		sess, err = s.auth.StartSession(ctx, q, u, ci)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrAlreadySetUp) {
			s.hasAny.Store(true)
		}
		return db.User{}, auth.NewSession{}, err
	}
	s.hasAny.Store(true)
	s.audit(ctx, "first admin created", "user_id", u.ID, "ip", ci.IP)
	return u, sess, nil
}

// CompleteSetup marks the wizard finished. Finishing it again keeps the
// first time.
func (s *Service) CompleteSetup(ctx context.Context, actor uuid.UUID) (SetupStatus, error) {
	st, err := s.SetupStatus(ctx)
	if err != nil || st.CompletedAt != nil {
		return st, err
	}
	now := s.now().UTC().Truncate(time.Second)
	raw, _ := json.Marshal(now)
	if err := s.st.PutSetting(ctx, db.PutSettingParams{Key: settingSetupCompleted, Value: raw}); err != nil {
		return SetupStatus{}, err
	}
	s.audit(ctx, "setup completed", "actor", actor)
	st.CompletedAt = &now
	return st, nil
}
