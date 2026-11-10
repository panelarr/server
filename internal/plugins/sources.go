// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"

	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

// ErrUnsupported is a call the source doesn't support (its capabilities).
var ErrUnsupported = errors.New("plugins: the source doesn't support this")

// MaxImageBytes caps one image from a plugin.
const MaxImageBytes = 15 << 20

// Viewer is who is browsing: it decides which sources show.
type Viewer struct {
	Grants users.Grants
	Admin  bool
	// HideUnknown hides sources without a content rating from users.
	HideUnknown bool
	// Testing lets an admin browse a source turned off, to try it before
	// turning it on. It never reaches sources no longer listed or of a
	// plugin turned off.
	Testing bool
	// AnyKnown lets an admin see every known source, absent ones too (for
	// their icons on the admin's source list; nothing is called on them).
	AnyKnown bool
}

// Source is a source with whether its plugin is enabled.
type Source struct {
	db.Source
	PluginEnabled bool
}

// Usable reports whether the source can be called: listed by an enabled
// plugin and enabled itself.
func (s Source) Usable() bool { return s.Present && s.Enabled && s.PluginEnabled }

// ratingRank places a source's rating on the users' ceiling scale: mixed
// sources count as suggestive (their entries are filtered by their own
// rating), nsfw sources as the strongest. -1 is unknown.
func ratingRank(r string) int {
	switch r {
	case "safe":
		return 0
	case "mixed":
		return 1
	case "nsfw":
		return len(users.ContentRatings) - 1
	}
	return -1
}

// Visible reports whether v may see and use s. Admins see every usable
// source (testing, also ones turned off); users only allowed ones within
// their ceiling.
func (v Viewer) Visible(s Source) bool {
	if v.AnyKnown && v.Admin {
		return true
	}
	testing := v.Testing && v.Admin && s.Present && s.PluginEnabled
	if !s.Usable() && !testing {
		return false
	}
	if v.Admin {
		return true
	}
	if !v.Grants.AllSources && !slices.Contains(v.Grants.AllowedSources, s.ID) {
		return false
	}
	rank := ratingRank(s.ContentRating)
	if rank < 0 {
		return !v.HideUnknown
	}
	return rank <= slices.Index(users.ContentRatings, v.Grants.ContentRatingCeiling)
}

// Sources lists the sources v may see; with all (admins), every source,
// including disabled and absent ones.
func (h *Host) Sources(ctx context.Context, v Viewer, all bool) ([]Source, error) {
	rows, err := h.st.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(rows))
	for _, r := range rows {
		s := Source{Source: r.Source, PluginEnabled: r.PluginEnabled}
		if (all && v.Admin) || v.Visible(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Source returns one source v may see, or ErrNotFound.
func (h *Host) Source(ctx context.Context, id string, v Viewer) (Source, error) {
	r, err := h.st.GetSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, err
	}
	s := Source{Source: r.Source, PluginEnabled: r.PluginEnabled}
	if !v.Visible(s) {
		return Source{}, ErrNotFound
	}
	return s, nil
}

// SetSourcesChanged sets what runs when sources become usable or stop
// being usable (turned on or off, listed again or no longer, their plugin
// turned on or off), with their IDs; chapters choose their copies again.
func (h *Host) SetSourcesChanged(fn func(ctx context.Context, sourceIDs []string)) {
	h.sourcesChanged = fn
}

// usable maps a plugin's sources to whether they are usable.
func (h *Host) usable(ctx context.Context, pluginID string) (map[string]bool, error) {
	rows, err := h.st.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Source.PluginID == pluginID {
			out[r.Source.ID] = Source{Source: r.Source, PluginEnabled: r.PluginEnabled}.Usable()
		}
	}
	return out, nil
}

// changedSince reports the plugin's sources whose usability differs from
// before (nil: report them all).
func (h *Host) changedSince(ctx context.Context, pluginID string, before map[string]bool) error {
	if h.sourcesChanged == nil {
		return nil
	}
	now, err := h.usable(ctx, pluginID)
	if err != nil {
		return err
	}
	var changed []string
	for id, ok := range now {
		if before == nil || before[id] != ok {
			changed = append(changed, id)
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		h.sourcesChanged(ctx, changed)
	}
	return nil
}

// SetSourceEnabled turns a source on or off for everyone.
func (h *Host) SetSourceEnabled(ctx context.Context, id string, enabled bool) (Source, error) {
	before, err := h.st.GetSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, err
	}
	n, err := h.st.SetSourceEnabled(ctx, db.SetSourceEnabledParams{ID: id, Enabled: enabled})
	if err != nil {
		return Source{}, err
	}
	if n == 0 {
		return Source{}, ErrNotFound
	}
	h.log.InfoContext(ctx, "source changed", "component", "audit", "source", id, "enabled", enabled)
	r, err := h.st.GetSource(ctx, id)
	if err != nil {
		return Source{}, err
	}
	out := Source{Source: r.Source, PluginEnabled: r.PluginEnabled}
	if was := (Source{Source: before.Source, PluginEnabled: before.PluginEnabled}).Usable(); was != out.Usable() && h.sourcesChanged != nil {
		h.sourcesChanged(ctx, []string{id})
	}
	return out, nil
}

// call resolves a source v may use and the plugin client serving it.
func (h *Host) call(ctx context.Context, v Viewer, id string, need string) (Source, client.Client, error) {
	s, err := h.Source(ctx, id, v)
	if err != nil {
		return Source{}, nil, err
	}
	if need != "" && !slices.Contains(s.Capabilities, need) {
		return Source{}, nil, ErrUnsupported
	}
	c, err := h.Client(ctx, s.PluginID)
	if err != nil {
		return Source{}, nil, err
	}
	return s, c, nil
}

// Page is one page of series from a source.
type Page struct {
	Series      []*sourcev1.Series
	HasNextPage bool
}

// Popular lists a source's popular series.
func (h *Host) Popular(ctx context.Context, v Viewer, id string, page uint32) (Page, error) {
	s, c, err := h.call(ctx, v, id, "")
	if err != nil {
		return Page{}, err
	}
	res, err := c.Popular(ctx, &sourcev1.PopularRequest{SourceId: s.SourceID, Page: page})
	if err != nil {
		return Page{}, err
	}
	return Page{Series: res.GetSeries(), HasNextPage: res.GetHasNextPage()}, nil
}

// Latest lists a source's latest updates.
func (h *Host) Latest(ctx context.Context, v Viewer, id string, page uint32) (Page, error) {
	s, c, err := h.call(ctx, v, id, "latest")
	if err != nil {
		return Page{}, err
	}
	res, err := c.Latest(ctx, &sourcev1.LatestRequest{SourceId: s.SourceID, Page: page})
	if err != nil {
		return Page{}, err
	}
	return Page{Series: res.GetSeries(), HasNextPage: res.GetHasNextPage()}, nil
}

// Search searches a source, with optional filter values.
func (h *Host) Search(ctx context.Context, v Viewer, id, query string, page uint32, filters []*sourcev1.FilterValue) (Page, error) {
	need := "search"
	if query == "" && len(filters) > 0 {
		need = "filters"
	}
	s, c, err := h.call(ctx, v, id, need)
	if err != nil {
		return Page{}, err
	}
	if len(filters) > 0 && !slices.Contains(s.Capabilities, "filters") {
		return Page{}, ErrUnsupported
	}
	res, err := c.Search(ctx, &sourcev1.SearchRequest{SourceId: s.SourceID, Page: page, Query: query, Filters: filters})
	if err != nil {
		return Page{}, err
	}
	return Page{Series: res.GetSeries(), HasNextPage: res.GetHasNextPage()}, nil
}

// Filters returns a source's search filters.
func (h *Host) Filters(ctx context.Context, v Viewer, id string) (*sourcev1.GetFiltersResponse, error) {
	s, c, err := h.call(ctx, v, id, "filters")
	if err != nil {
		return nil, err
	}
	return c.GetFilters(ctx, &sourcev1.GetFiltersRequest{SourceId: s.SourceID})
}

// Series returns a series' details.
func (h *Host) Series(ctx context.Context, v Viewer, id, key string) (*sourcev1.Series, error) {
	s, c, err := h.call(ctx, v, id, "")
	if err != nil {
		return nil, err
	}
	res, err := c.GetSeries(ctx, &sourcev1.GetSeriesRequest{SourceId: s.SourceID, Key: key})
	if err != nil {
		return nil, err
	}
	return res.GetSeries(), nil
}

// Chapters lists a series' chapters as the source reports them.
func (h *Host) Chapters(ctx context.Context, v Viewer, id, key string) ([]*sourcev1.Chapter, error) {
	return h.ListChapters(ctx, v, id, key, false)
}

// ListChapters lists a series' chapters; refresh asks the plugin to skip
// its cache (update checks).
func (h *Host) ListChapters(ctx context.Context, v Viewer, id, key string, refresh bool) ([]*sourcev1.Chapter, error) {
	s, c, err := h.call(ctx, v, id, "")
	if err != nil {
		return nil, err
	}
	res, err := c.ListChapters(ctx, &sourcev1.ListChaptersRequest{SourceId: s.SourceID, SeriesKey: key, Refresh: refresh})
	if err != nil {
		return nil, err
	}
	return res.GetChapters(), nil
}

// Image fetches an image through the source's plugin (its headers and
// network settings apply).
func (h *Host) Image(ctx context.Context, v Viewer, id, ref string) (string, []byte, error) {
	s, c, err := h.call(ctx, v, id, "")
	if err != nil {
		return "", nil, err
	}
	return client.ReadImage(ctx, c, &sourcev1.FetchImageRequest{SourceId: s.SourceID, Ref: ref}, MaxImageBytes)
}

// PackageIconRef is a source's icon ref meaning "its package's icon", for
// sources without an icon of their own. It isn't an http(s) URL, so it
// never names a real icon.
const PackageIconRef = "package:"

// SourceIcon fetches the icon of a source v may see (see [Host.Icon]).
// When the plugin has no icon at ref (it answers 404, e.g. for a source in
// a language turned off) or ref is [PackageIconRef], the icon of the
// package that provides the source stands in. ErrNotFound when neither
// exists.
func (h *Host) SourceIcon(ctx context.Context, v Viewer, id, ref string) (string, io.Reader, error) {
	s, err := h.Source(ctx, id, v)
	if err != nil {
		return "", nil, err
	}
	if ref != PackageIconRef {
		ct, body, err := h.Icon(ctx, s.PluginID, ref)
		if !errors.Is(err, ErrNotFound) {
			return ct, body, err
		}
	}
	if s.PackageID == "" {
		return "", nil, ErrNotFound
	}
	return h.PackageIcon(ctx, s.PluginID, s.PackageID)
}

// Resolved is a series found from a pasted URL.
type Resolved struct {
	Source     Source
	Series     *sourcev1.Series
	BestEffort bool
}

// ResolveURL finds the series a URL points to. It asks every enabled
// plugin with a source v may use, plugins with a native URL resolver (a
// source with the resolve_url capability) first: a plugin without one may
// still find the series another way (e.g. by searching the site), and says
// so with best_effort. A plugin that doesn't implement ResolveUrl, fails
// or knows nothing is skipped. A best-effort answer is used only when no
// plugin knows the URL for certain. ErrNotFound when none knows the URL.
func (h *Host) ResolveURL(ctx context.Context, v Viewer, rawURL string) (Resolved, error) {
	srcs, err := h.Sources(ctx, v, false)
	if err != nil {
		return Resolved{}, err
	}
	var order []string
	native := map[string]bool{}
	for _, s := range srcs {
		if !slices.Contains(order, s.PluginID) {
			order = append(order, s.PluginID)
		}
		if slices.Contains(s.Capabilities, "resolve_url") {
			native[s.PluginID] = true
		}
	}
	slices.SortStableFunc(order, func(a, b string) int {
		switch {
		case native[a] == native[b]:
			return 0
		case native[a]:
			return -1
		default:
			return 1
		}
	})
	var guess *Resolved
	for _, id := range order {
		c, err := h.Client(ctx, id)
		if err != nil {
			continue
		}
		res, err := h.resolveURL(ctx, c, rawURL)
		if err != nil {
			if connect.CodeOf(err) != connect.CodeUnimplemented && connect.CodeOf(err) != connect.CodeNotFound {
				h.log.WarnContext(ctx, "resolve url failed", "plugin", id, "err", errorMessage(err))
			}
			continue
		}
		if res.GetSeries() == nil {
			continue
		}
		found, err := h.Source(ctx, id+":"+res.GetSourceId(), v)
		if err != nil {
			continue // a source the viewer may not use
		}
		r := Resolved{Source: found, Series: res.GetSeries(), BestEffort: res.GetBestEffort()}
		if !r.BestEffort {
			return r, nil
		}
		if guess == nil {
			guess = &r // keep asking: another plugin may know the URL for certain
		}
	}
	if guess != nil {
		return *guess, nil
	}
	return Resolved{}, ErrNotFound
}

// resolveURL asks one plugin, within callTimeout: plugins are asked one
// after another, so one that hangs (a best-effort search can take as long
// as the site's search) mustn't hold up the others.
func (h *Host) resolveURL(ctx context.Context, c client.Client, rawURL string) (*sourcev1.ResolveUrlResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.ResolveUrl(ctx, &sourcev1.ResolveUrlRequest{Url: rawURL})
}

// Pages lists a chapter's pages in reading order.
func (h *Host) Pages(ctx context.Context, v Viewer, id, chapterKey string) ([]*sourcev1.Page, error) {
	s, c, err := h.call(ctx, v, id, "")
	if err != nil {
		return nil, err
	}
	res, err := c.ListPages(ctx, &sourcev1.ListPagesRequest{SourceId: s.SourceID, ChapterKey: chapterKey})
	if err != nil {
		return nil, err
	}
	return res.GetPages(), nil
}

// SourceSettings is a source's own settings: its JSON Schema and values,
// with secret (writeOnly) values blanked.
type SourceSettings struct {
	Schema map[string]any
	Values map[string]any
}

// settingsViewer is who edits a source's settings: admins, also on a
// source turned off.
var settingsViewer = Viewer{Admin: true, Testing: true}

// writeOnly lists the schema's secret top-level properties.
func writeOnly(schema map[string]any) map[string]bool {
	out := map[string]bool{}
	props, _ := schema["properties"].(map[string]any)
	for k, v := range props {
		if p, ok := v.(map[string]any); ok && p["writeOnly"] == true {
			out[k] = true
		}
	}
	return out
}

func shown(schema, values map[string]any) map[string]any {
	secret := writeOnly(schema)
	out := make(map[string]any, len(values))
	for k, v := range values {
		if secret[k] {
			v = ""
		}
		out[k] = v
	}
	return out
}

// Settings returns a source's settings, read from its plugin.
func (h *Host) Settings(ctx context.Context, id string) (SourceSettings, error) {
	s, c, err := h.call(ctx, settingsViewer, id, "settings")
	if err != nil {
		return SourceSettings{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sch, err := c.GetSettingsSchema(ctx, &sourcev1.GetSettingsSchemaRequest{SourceId: s.SourceID})
	if err != nil {
		return SourceSettings{}, err
	}
	vals, err := c.GetSettings(ctx, &sourcev1.GetSettingsRequest{SourceId: s.SourceID})
	if err != nil {
		return SourceSettings{}, err
	}
	schema := sch.GetSchema().AsMap()
	return SourceSettings{Schema: schema, Values: shown(schema, vals.GetValues().AsMap())}, nil
}

// SetSettings changes some of a source's settings (keys left out keep
// their value; a secret left empty too) and returns them all.
func (h *Host) SetSettings(ctx context.Context, id string, values map[string]any) (SourceSettings, error) {
	s, c, err := h.call(ctx, settingsViewer, id, "settings")
	if err != nil {
		return SourceSettings{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sch, err := c.GetSettingsSchema(ctx, &sourcev1.GetSettingsSchemaRequest{SourceId: s.SourceID})
	if err != nil {
		return SourceSettings{}, err
	}
	schema := sch.GetSchema().AsMap()
	secret := writeOnly(schema)
	changes := make(map[string]any, len(values))
	for k, v := range values {
		if secret[k] && v == "" {
			continue
		}
		changes[k] = v
	}
	st, err := structpb.NewStruct(changes)
	if err != nil {
		return SourceSettings{}, fmt.Errorf("%w: %w", ErrBadConfig, err)
	}
	res, err := c.SetSettings(ctx, &sourcev1.SetSettingsRequest{SourceId: s.SourceID, Values: st})
	if err != nil {
		return SourceSettings{}, err
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	h.log.InfoContext(ctx, "source settings changed", "component", "audit", "source", id, "keys", keys)
	return SourceSettings{Schema: schema, Values: shown(schema, res.GetValues().AsMap())}, nil
}
