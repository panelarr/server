// SPDX-License-Identifier: GPL-3.0-or-later

package network

import (
	"context"
	"regexp"
	"time"

	"github.com/panelarr/server/internal/store/db"
)

// Assignment puts a profile, or the plugin's own settings, at one level:
// global, a plugin, a package ("plugin:package"), a source, all of meta
// or one metadata provider. Without an assignment a level inherits.
type Assignment struct {
	Kind, TargetID string
	// ProfileID empty means the plugin's own settings ("plugin default").
	ProfileID string
	UpdatedAt time.Time
}

// PluginDefault reports whether the assignment opts out to the plugin's
// own settings.
func (a Assignment) PluginDefault() bool { return a.ProfileID == "" }

var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

func assignmentOf(r db.NetworkAssignment) Assignment {
	return Assignment{Kind: r.TargetKind, TargetID: r.TargetID, ProfileID: r.ProfileID.String, UpdatedAt: r.UpdatedAt}
}

// SetChanged sets what runs after a profile or an assignment changed (the
// push to plugins and meta).
func (s *Service) SetChanged(fn func(ctx context.Context)) { s.onChange = fn }

func (s *Service) changed(ctx context.Context) {
	if s.onChange != nil {
		s.onChange(ctx)
	}
}

// Assignments lists every assignment.
func (s *Service) Assignments(ctx context.Context) ([]Assignment, error) {
	rows, err := s.st.ListAssignments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Assignment, len(rows))
	for i, r := range rows {
		out[i] = assignmentOf(r)
	}
	return out, nil
}

// checkTarget checks an assignment's target and returns the plugin it
// belongs to, if any.
func (s *Service) checkTarget(ctx context.Context, kind, id string) (string, error) {
	switch kind {
	case KindGlobal, KindMeta:
		if id != "" {
			return "", invalid("target_id", "none for "+kind)
		}
		return "", nil
	case KindPlugin, KindPackage, KindSource:
		if id == "" {
			return "", invalid("target_id", "say which "+kind)
		}
		return s.pluginOf(ctx, kind, id)
	case KindProvider:
		if !providerPattern.MatchString(id) {
			return "", invalid("target_id", "a metadata provider's ID")
		}
		return "", nil
	}
	return "", invalid("target_kind", "global, plugin, package, source, meta or provider")
}

// Assign puts a profile (empty: the plugin's own settings) at a level.
func (s *Service) Assign(ctx context.Context, kind, id, profileID string) (Assignment, error) {
	plugin, err := s.checkTarget(ctx, kind, id)
	if err != nil {
		return Assignment{}, err
	}
	if profileID != "" {
		p, err := s.Profile(ctx, profileID)
		if err != nil {
			return Assignment{}, invalid("profile_id", "no such profile")
		}
		if p.Owner != nil && (p.Owner.Kind != kind || p.Owner.ID != id) {
			return Assignment{}, ErrNotShared
		}
	}
	r, err := s.st.UpsertAssignment(ctx, db.UpsertAssignmentParams{
		TargetKind: kind, TargetID: id, ProfileID: text(profileID), PluginID: text(plugin),
	})
	if err != nil {
		return Assignment{}, err
	}
	s.log.InfoContext(ctx, "network profile assigned", "component", "audit", "target_kind", kind, "target", id, "profile", profileID)
	s.changed(ctx)
	return assignmentOf(r), nil
}

// Unassign removes the assignment at a level, which then inherits.
func (s *Service) Unassign(ctx context.Context, kind, id string) error {
	n, err := s.st.DeleteAssignment(ctx, db.DeleteAssignmentParams{TargetKind: kind, TargetID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.log.InfoContext(ctx, "network profile unassigned", "component", "audit", "target_kind", kind, "target", id)
	s.changed(ctx)
	return nil
}

// Effective is the profile in force for a source or a provider, and the
// level it comes from ("source", "package", "plugin", "global" for
// sources; "provider", "meta", "global" for providers; "none" when
// nothing is assigned).
type Effective struct {
	Level string
	// ProfileID empty means the plugin's own settings.
	ProfileID string
}

// Resolver resolves effective profiles from one read of the assignments.
type Resolver struct{ byTarget map[string]Assignment }

// Resolver reads the assignments for resolving.
func (s *Service) Resolver(ctx context.Context) (*Resolver, error) {
	list, err := s.Assignments(ctx)
	if err != nil {
		return nil, err
	}
	r := &Resolver{byTarget: map[string]Assignment{}}
	for _, a := range list {
		r.byTarget[a.Kind+"\x00"+a.TargetID] = a
	}
	return r, nil
}

func (r *Resolver) first(levels ...[2]string) Effective {
	for _, l := range levels {
		if a, ok := r.byTarget[l[0]+"\x00"+l[1]]; ok {
			return Effective{Level: l[0], ProfileID: a.ProfileID}
		}
	}
	return Effective{Level: "none"}
}

// Source is a source's effective profile: source, then its package, then
// its plugin, then global; the most specific assignment wins.
func (r *Resolver) Source(sourceID, pluginID, packageID string) Effective {
	levels := [][2]string{{KindSource, sourceID}}
	if packageID != "" {
		levels = append(levels, [2]string{KindPackage, pluginID + ":" + packageID})
	}
	levels = append(levels, [2]string{KindPlugin, pluginID}, [2]string{KindGlobal, ""})
	return r.first(levels...)
}

// Provider is a metadata provider's effective profile: provider, then all
// of meta, then global.
func (r *Resolver) Provider(id string) Effective {
	return r.first([2]string{KindProvider, id}, [2]string{KindMeta, ""}, [2]string{KindGlobal, ""})
}

// Assigned is the assignment at exactly this level, if any.
func (r *Resolver) Assigned(kind, id string) (Assignment, bool) {
	a, ok := r.byTarget[kind+"\x00"+id]
	return a, ok
}
