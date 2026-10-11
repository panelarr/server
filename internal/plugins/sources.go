// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

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
// source; users only allowed ones within their ceiling.
func (v Viewer) Visible(s Source) bool {
	if !s.Usable() {
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

// SetSourceEnabled turns a source on or off for everyone.
func (h *Host) SetSourceEnabled(ctx context.Context, id string, enabled bool) (Source, error) {
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
	return Source{Source: r.Source, PluginEnabled: r.PluginEnabled}, nil
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

// Resolved is a series found from a pasted URL.
type Resolved struct {
	Source     Source
	Series     *sourcev1.Series
	BestEffort bool
}

// ResolveURL finds the series a URL points to, asking each enabled plugin
// with a source v may use that resolves URLs. ErrNotFound when none knows
// it.
func (h *Host) ResolveURL(ctx context.Context, v Viewer, rawURL string) (Resolved, error) {
	srcs, err := h.Sources(ctx, v, false)
	if err != nil {
		return Resolved{}, err
	}
	asked := map[string]bool{}
	for _, s := range srcs {
		if asked[s.PluginID] || !slices.Contains(s.Capabilities, "resolve_url") {
			continue
		}
		asked[s.PluginID] = true
		c, err := h.Client(ctx, s.PluginID)
		if err != nil {
			continue
		}
		res, err := c.ResolveUrl(ctx, &sourcev1.ResolveUrlRequest{Url: rawURL})
		if err != nil || res.GetSeries() == nil {
			continue
		}
		found, err := h.Source(ctx, s.PluginID+":"+res.GetSourceId(), v)
		if err != nil {
			continue // a source the viewer may not use
		}
		return Resolved{Source: found, Series: res.GetSeries(), BestEffort: res.GetBestEffort()}, nil
	}
	return Resolved{}, ErrNotFound
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
