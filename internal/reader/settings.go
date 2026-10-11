// SPDX-License-Identifier: GPL-3.0-or-later

package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/server/internal/store/db"
)

// MaxSettingsBytes caps one level of reader settings or the preferences.
const MaxSettingsBytes = 16 << 10

// ErrBadScope is a settings level that isn't global, source:<id> or
// work:<id>.
var ErrBadScope = errors.New("reader: the level must be global, source:<id> or work:<id>")

// ErrTooLarge is settings over MaxSettingsBytes.
var ErrTooLarge = fmt.Errorf("reader: settings larger than %d bytes", MaxSettingsBytes)

// Scope names a settings level.
func Scope(sourceID string, workID *uuid.UUID) string {
	switch {
	case workID != nil:
		return "work:" + workID.String()
	case sourceID != "":
		return "source:" + sourceID
	}
	return "global"
}

// ValidScope checks a settings level's name.
func ValidScope(scope string) error {
	switch {
	case scope == "global":
		return nil
	case strings.HasPrefix(scope, "source:") && len(scope) > len("source:") && len(scope) <= 300:
		return nil
	case strings.HasPrefix(scope, "work:"):
		if _, err := uuid.Parse(strings.TrimPrefix(scope, "work:")); err == nil {
			return nil
		}
	}
	return ErrBadScope
}

// Settings are a user's reader settings for one place: each level, and
// the effective result (global, then the source's, then the work's, each
// key of a more specific level winning).
type Settings struct {
	Global, Source, Work map[string]any
	Effective            map[string]any
}

// GetSettings returns the settings that apply to reading a work from a
// source (both optional).
func (s *Service) GetSettings(ctx context.Context, userID uuid.UUID, sourceID string, workID *uuid.UUID) (Settings, error) {
	scopes := []string{"global"}
	if sourceID != "" {
		scopes = append(scopes, Scope(sourceID, nil))
	}
	if workID != nil {
		scopes = append(scopes, Scope("", workID))
	}
	rows, err := s.st.ListReaderSettings(ctx, db.ListReaderSettingsParams{UserID: userID, Scopes: scopes})
	if err != nil {
		return Settings{}, err
	}
	out := Settings{Effective: map[string]any{}}
	levels := map[string]map[string]any{}
	for _, r := range rows {
		var m map[string]any
		if err := json.Unmarshal(r.Settings, &m); err != nil {
			return Settings{}, err
		}
		levels[r.Scope] = m
	}
	for _, sc := range scopes {
		maps.Copy(out.Effective, levels[sc])
	}
	out.Global = levels["global"]
	if sourceID != "" {
		out.Source = levels[Scope(sourceID, nil)]
	}
	if workID != nil {
		out.Work = levels[Scope("", workID)]
	}
	return out, nil
}

// PutSettings replaces one level.
func (s *Service) PutSettings(ctx context.Context, userID uuid.UUID, scope string, settings map[string]any) error {
	if err := ValidScope(scope); err != nil {
		return err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if len(raw) > MaxSettingsBytes {
		return ErrTooLarge
	}
	_, err = s.st.PutReaderSettings(ctx, db.PutReaderSettingsParams{UserID: userID, Scope: scope, Settings: raw})
	return err
}

// DeleteSettings removes one level (back to the less specific ones).
func (s *Service) DeleteSettings(ctx context.Context, userID uuid.UUID, scope string) error {
	if err := ValidScope(scope); err != nil {
		return err
	}
	n, err := s.st.DeleteReaderSettings(ctx, db.DeleteReaderSettingsParams{UserID: userID, Scope: scope})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Preferences returns a user's app preferences ({} when none).
func (s *Service) Preferences(ctx context.Context, userID uuid.UUID) (map[string]any, error) {
	row, err := s.st.GetPreferences(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(row.Preferences, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SetPreferences replaces a user's app preferences.
func (s *Service) SetPreferences(ctx context.Context, userID uuid.UUID, prefs map[string]any) error {
	raw, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	if len(raw) > MaxSettingsBytes {
		return ErrTooLarge
	}
	_, err = s.st.PutPreferences(ctx, db.PutPreferencesParams{UserID: userID, Preferences: raw})
	return err
}
