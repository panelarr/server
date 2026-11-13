// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/store/db"
)

// Saved library views: each user names views of their own library (filters,
// a sort and display settings). The server only stores and validates them;
// clients apply them. Every user has exactly one default view as soon as
// they have any, and their views have a gap-free order (positions 0..n-1).

// Limits of saved views.
const (
	MaxViews           = 50
	MaxViewName        = 60   // characters
	MaxViewDefinition  = 4096 // bytes of the stored JSON
	MaxViewValues      = 20   // types or statuses
	MaxViewValueLength = 40   // characters of one type or status
	MaxUpdatedWithin   = 365  // days
)

// viewContentRatings are the content ratings a view may filter by: a
// work's meta rating (the same value the user's ceiling uses), with
// unknown for works meta has no rating for. A view's list only narrows
// the library; works above the caller's ceiling are never listed.
var viewContentRatings = []string{"safe", "suggestive", "erotic", "pornographic", "unknown"}

// Errors of saved views; a view that isn't the caller's is ErrNotFound.
var (
	ErrViewNameTaken   = errors.New("library: you have a view with this name")
	ErrViewLimit       = fmt.Errorf("library: at most %d views", MaxViews)
	ErrDefaultRequired = errors.New("library: one view must be the default; make another view the default instead")
	ErrLastView        = errors.New("library: your only view can't be deleted")
	ErrOrderMismatch   = errors.New("library: the order must list each of your views exactly once")
)

// ViewError is an invalid name or definition.
type ViewError struct {
	// Field is the request field, e.g. "name" or "definition.sort".
	Field  string
	Reason string
}

func (e *ViewError) Error() string { return "library: " + e.Field + ": " + e.Reason }

// ViewsChangedEvent tells the owner's devices to refetch their views.
const ViewsChangedEvent = "library_views.changed"

// View is a saved view.
type View struct {
	ID         uuid.UUID
	Name       string
	Position   int
	Default    bool
	Definition json.RawMessage
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ViewPatch changes a view; nil fields stay.
type ViewPatch struct {
	Name       *string
	Definition json.RawMessage
	// Default true makes the view the default; false is refused on the
	// default view (another view must be made the default instead).
	Default *bool
}

// Tri-state filters, sorts and layouts a definition may use. A new filter
// or sort needs a server release before clients can save it.
var (
	viewFilterValues = []string{"include", "exclude"}
	viewSorts        = []string{"title", "added", "read", "latest", "updated", "unread", "total"}
	viewLayouts      = []string{"compact", "comfortable", "list"}
)

// viewDefinition is the stored shape of a definition (the API's
// LibraryViewDefinition). Everything is optional: clients fill in their
// defaults.
type viewDefinition struct {
	Filters           *viewFilters `json:"filters,omitempty"`
	Types             []string     `json:"types,omitempty"`
	Statuses          []string     `json:"statuses,omitempty"`
	ContentRatings    []string     `json:"content_ratings,omitempty"`
	UpdatedWithinDays *int         `json:"updated_within_days,omitempty"`
	Sort              *string      `json:"sort,omitempty"`
	Reverse           *bool        `json:"reverse,omitempty"`
	Display           *viewDisplay `json:"display,omitempty"`
}

type viewFilters struct {
	Unread     *string `json:"unread,omitempty"`
	Started    *string `json:"started,omitempty"`
	Downloaded *string `json:"downloaded,omitempty"`
	Bookmarked *string `json:"bookmarked,omitempty"`
	New        *string `json:"new,omitempty"`
}

type viewDisplay struct {
	Layout         *string `json:"layout,omitempty"`
	UnreadBadges   *bool   `json:"unread_badges,omitempty"`
	DownloadBadges *bool   `json:"download_badges,omitempty"`
	ContinueButton *bool   `json:"continue_button,omitempty"`
}

// cleanViewName trims a name and checks its length.
func cleanViewName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxViewName {
		return "", &ViewError{Field: "name", Reason: fmt.Sprintf("must be 1 to %d characters", MaxViewName)}
	}
	return name, nil
}

// cleanViewDefinition checks a definition (unknown keys and values are
// refused) and returns it as stored.
func cleanViewDefinition(raw json.RawMessage) ([]byte, error) {
	bad := func(field, reason string) error {
		return &ViewError{Field: "definition" + field, Reason: reason}
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, bad("", "required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d viewDefinition
	if err := dec.Decode(&d); err != nil {
		return nil, bad("", "not a valid view definition: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, bad("", "not a single JSON object")
	}
	oneOf := func(field string, v *string, allowed []string) error {
		if v != nil && !slices.Contains(allowed, *v) {
			return bad(field, "must be one of "+strings.Join(allowed, ", "))
		}
		return nil
	}
	values := func(field string, vs []string) error {
		if len(vs) > MaxViewValues {
			return bad(field, fmt.Sprintf("at most %d values", MaxViewValues))
		}
		for _, v := range vs {
			if n := utf8.RuneCountInString(v); n < 1 || n > MaxViewValueLength {
				return bad(field, fmt.Sprintf("each value must be 1 to %d characters", MaxViewValueLength))
			}
		}
		return nil
	}
	ratings := func(field string, vs []string) error {
		if len(vs) > len(viewContentRatings) {
			return bad(field, fmt.Sprintf("at most %d values", len(viewContentRatings)))
		}
		seen := make(map[string]bool, len(vs))
		for _, v := range vs {
			if !slices.Contains(viewContentRatings, v) {
				return bad(field, "each value must be one of "+strings.Join(viewContentRatings, ", "))
			}
			if seen[v] {
				return bad(field, "values must be unique")
			}
			seen[v] = true
		}
		return nil
	}
	var errs []error
	if f := d.Filters; f != nil {
		errs = append(errs,
			oneOf(".filters.unread", f.Unread, viewFilterValues),
			oneOf(".filters.started", f.Started, viewFilterValues),
			oneOf(".filters.downloaded", f.Downloaded, viewFilterValues),
			oneOf(".filters.bookmarked", f.Bookmarked, viewFilterValues),
			oneOf(".filters.new", f.New, viewFilterValues))
	}
	errs = append(errs, values(".types", d.Types), values(".statuses", d.Statuses),
		ratings(".content_ratings", d.ContentRatings), oneOf(".sort", d.Sort, viewSorts))
	if n := d.UpdatedWithinDays; n != nil && (*n < 1 || *n > MaxUpdatedWithin) {
		errs = append(errs, bad(".updated_within_days", fmt.Sprintf("must be 1 to %d", MaxUpdatedWithin)))
	}
	if d.Display != nil {
		errs = append(errs, oneOf(".display.layout", d.Display.Layout, viewLayouts))
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxViewDefinition {
		return nil, bad("", fmt.Sprintf("at most %d bytes", MaxViewDefinition))
	}
	return out, nil
}

func toView(r db.LibraryView) View {
	return View{
		ID: r.ID, Name: r.Name, Position: int(r.Position), Default: r.IsDefault,
		Definition: json.RawMessage(r.Definition), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func viewWriteError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "library_views_name_key" {
		return ErrViewNameTaken
	}
	return err
}

// viewsChanged tells the owner's devices (only theirs) about a change.
func (s *Service) viewsChanged(userID uuid.UUID, viewID *uuid.UUID) {
	data := map[string]any{}
	if viewID != nil {
		data["view_id"] = *viewID
	}
	s.hub.Publish(ViewsChangedEvent, data, events.Audience{Users: []uuid.UUID{userID}})
}

// inViews runs f in a transaction holding the user's views lock.
func (s *Service) inViews(ctx context.Context, userID uuid.UUID, f func(q *db.Queries) error) error {
	return s.st.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.LockLibraryViews(ctx, userID); err != nil {
			return err
		}
		return f(q)
	})
}

// Views lists a user's views in their order.
func (s *Service) Views(ctx context.Context, userID uuid.UUID) ([]View, error) {
	rows, err := s.st.ListLibraryViews(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(rows))
	for i, r := range rows {
		out[i] = toView(r)
	}
	return out, nil
}

// View returns one of a user's views.
func (s *Service) View(ctx context.Context, userID, id uuid.UUID) (View, error) {
	return getView(ctx, s.st.Queries, userID, id)
}

func getView(ctx context.Context, q *db.Queries, userID, id uuid.UUID) (View, error) {
	r, err := q.GetLibraryView(ctx, db.GetLibraryViewParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	return toView(r), nil
}

// CreateView adds a view at the end of the user's order. Their first view
// is the default whatever makeDefault says.
func (s *Service) CreateView(ctx context.Context, userID uuid.UUID, name string, definition json.RawMessage, makeDefault bool) (View, error) {
	name, err := cleanViewName(name)
	if err != nil {
		return View{}, err
	}
	def, err := cleanViewDefinition(definition)
	if err != nil {
		return View{}, err
	}
	var out View
	err = s.inViews(ctx, userID, func(q *db.Queries) error {
		n, err := q.CountLibraryViews(ctx, userID)
		if err != nil {
			return err
		}
		if n >= MaxViews {
			return ErrViewLimit
		}
		isDefault := makeDefault || n == 0
		if isDefault {
			if err := q.ClearLibraryViewDefault(ctx, db.ClearLibraryViewDefaultParams{UserID: userID}); err != nil {
				return err
			}
		}
		r, err := q.CreateLibraryView(ctx, db.CreateLibraryViewParams{
			UserID: userID, Name: name, Position: int32(n), IsDefault: isDefault, Definition: def, //nolint:gosec // n < MaxViews
		})
		if err != nil {
			return viewWriteError(err)
		}
		out = toView(r)
		return nil
	})
	if err != nil {
		return View{}, err
	}
	s.viewsChanged(userID, &out.ID)
	return out, nil
}

// UpdateView renames a view, replaces its definition or makes it the
// default (the old default stops being one in the same transaction).
func (s *Service) UpdateView(ctx context.Context, userID, id uuid.UUID, p ViewPatch) (View, error) {
	var name pgtype.Text
	if p.Name != nil {
		n, err := cleanViewName(*p.Name)
		if err != nil {
			return View{}, err
		}
		name = pgtype.Text{String: n, Valid: true}
	}
	var def []byte
	if p.Definition != nil {
		d, err := cleanViewDefinition(p.Definition)
		if err != nil {
			return View{}, err
		}
		def = d
	}
	var out View
	err := s.inViews(ctx, userID, func(q *db.Queries) error {
		cur, err := getView(ctx, q, userID, id)
		if err != nil {
			return err
		}
		if p.Default != nil && !*p.Default && cur.Default {
			return ErrDefaultRequired
		}
		if name.Valid || def != nil {
			if _, err := q.UpdateLibraryView(ctx, db.UpdateLibraryViewParams{Name: name, Definition: def, ID: id, UserID: userID}); err != nil {
				return viewWriteError(err)
			}
		}
		if p.Default != nil && *p.Default && !cur.Default {
			if err := q.ClearLibraryViewDefault(ctx, db.ClearLibraryViewDefaultParams{UserID: userID, Keep: id}); err != nil {
				return err
			}
			if err := q.SetLibraryViewDefault(ctx, db.SetLibraryViewDefaultParams{ID: id, UserID: userID}); err != nil {
				return err
			}
		}
		out, err = getView(ctx, q, userID, id)
		return err
	})
	if err != nil {
		return View{}, err
	}
	s.viewsChanged(userID, &id)
	return out, nil
}

// DeleteView deletes a view. The default passes to the first remaining
// view; the only view can't be deleted.
func (s *Service) DeleteView(ctx context.Context, userID, id uuid.UUID) error {
	err := s.inViews(ctx, userID, func(q *db.Queries) error {
		cur, err := getView(ctx, q, userID, id)
		if err != nil {
			return err
		}
		n, err := q.CountLibraryViews(ctx, userID)
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastView
		}
		if _, err := q.DeleteLibraryView(ctx, db.DeleteLibraryViewParams{ID: id, UserID: userID}); err != nil {
			return err
		}
		if err := q.ShiftLibraryViewsUp(ctx, db.ShiftLibraryViewsUpParams{UserID: userID, Position: int32(cur.Position)}); err != nil { //nolint:gosec // < MaxViews
			return err
		}
		if !cur.Default {
			return nil
		}
		rest, err := q.ListLibraryViews(ctx, userID)
		if err != nil {
			return err
		}
		return q.SetLibraryViewDefault(ctx, db.SetLibraryViewDefaultParams{ID: rest[0].ID, UserID: userID})
	})
	if err != nil {
		return err
	}
	s.viewsChanged(userID, &id)
	return nil
}

// OrderViews puts a user's views in the given order, which must list each
// of them exactly once.
func (s *Service) OrderViews(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) ([]View, error) {
	err := s.inViews(ctx, userID, func(q *db.Queries) error {
		rows, err := q.ListLibraryViews(ctx, userID)
		if err != nil {
			return err
		}
		if len(ids) != len(rows) {
			return ErrOrderMismatch
		}
		seen := make(map[uuid.UUID]bool, len(ids))
		for _, id := range ids {
			if seen[id] || !slices.ContainsFunc(rows, func(r db.LibraryView) bool { return r.ID == id }) {
				return ErrOrderMismatch
			}
			seen[id] = true
		}
		return q.OrderLibraryViews(ctx, db.OrderLibraryViewsParams{UserID: userID, Ids: ids})
	})
	if err != nil {
		return nil, err
	}
	s.viewsChanged(userID, nil)
	return s.Views(ctx, userID)
}
