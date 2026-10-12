// SPDX-License-Identifier: GPL-3.0-or-later

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"
)

// ErrNotFound is an entry that doesn't exist or that the viewer may not see.
var ErrNotFound = errors.New("meta: entry not found")

// BadFilterError is a filter value the catalogue doesn't know.
type BadFilterError struct{ Field, Value string }

func (e *BadFilterError) Error() string {
	return fmt.Sprintf("meta: unknown %s %q", e.Field, e.Value)
}

// settingHideUnknown hides entries without a known content rating from
// users (admins see everything). Off by default: many entries have no
// rating.
const settingHideUnknown = "catalog.hide_unknown_ratings"

// Viewer is who is looking: it decides which content ratings show.
type Viewer struct {
	// Ceiling is the strongest rating shown: safe, suggestive, erotic or
	// pornographic.
	Ceiling string
	// Admin viewers always see unrated entries.
	Admin bool
}

// ratingOrder lists the ratings from mildest to strongest.
var ratingOrder = []metadatav1.ContentRating{
	metadatav1.ContentRating_CONTENT_RATING_SAFE,
	metadatav1.ContentRating_CONTENT_RATING_SUGGESTIVE,
	metadatav1.ContentRating_CONTENT_RATING_EROTIC,
	metadatav1.ContentRating_CONTENT_RATING_PORNOGRAPHIC,
}

var unrated = []metadatav1.ContentRating{
	metadatav1.ContentRating_CONTENT_RATING_UNSPECIFIED,
	metadatav1.ContentRating_CONTENT_RATING_UNKNOWN,
}

// allowedRatings lists the ratings a viewer may see.
func (s *Service) allowedRatings(ctx context.Context, v Viewer) []metadatav1.ContentRating {
	var out []metadatav1.ContentRating
	for _, r := range ratingOrder {
		out = append(out, r)
		if RatingName(r) == v.Ceiling {
			break
		}
	}
	if v.Admin || !s.HideUnknown(ctx) {
		out = append(out, unrated...)
	}
	return out
}

// HideUnknown reports whether users are kept from entries and sources
// without a known content rating.
func (s *Service) HideUnknown(ctx context.Context) bool {
	raw, err := s.st.GetSetting(ctx, settingHideUnknown)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		s.log.WarnContext(ctx, "read setting", "key", settingHideUnknown, "err", err)
		return true // fail closed
	}
	var hide bool
	_ = json.Unmarshal(raw, &hide)
	return hide
}

// RatingName turns an entry's rating into the API's name: safe,
// suggestive, erotic, pornographic or unknown.
func RatingName(r metadatav1.ContentRating) string {
	if slices.Contains(unrated, r) {
		return "unknown"
	}
	return enumName(r.String(), "CONTENT_RATING_")
}

// TypeName and StatusName turn entry types and statuses into the API's
// names ("manga", "releasing"), or "" when unknown.
func TypeName(t metadatav1.EntryType) string     { return enumName(t.String(), "ENTRY_TYPE_") }
func StatusName(s metadatav1.EntryStatus) string { return enumName(s.String(), "ENTRY_STATUS_") }

func enumName(s, prefix string) string {
	s = strings.ToLower(strings.TrimPrefix(s, prefix))
	if s == "unspecified" {
		return ""
	}
	return s
}

func parseEnum(field, prefix string, values map[string]int32, names []string) ([]int32, error) {
	out := make([]int32, 0, len(names))
	for _, n := range names {
		v, ok := values[prefix+strings.ToUpper(n)]
		if !ok || v == 0 {
			return nil, &BadFilterError{Field: field, Value: n}
		}
		out = append(out, v)
	}
	return out, nil
}

// Query is a catalogue search or browse.
type Query struct {
	Text      string
	Types     []string
	Statuses  []string
	Genres    []string
	YearFrom  int32
	YearTo    int32
	Sort      string // popularity, rating, updated, year, title, relevance
	Reverse   bool
	Language  string
	PageSize  int32
	PageToken string
}

func (s *Service) filter(ctx context.Context, q Query, v Viewer) (*metav1.EntryFilter, metav1.SortField, error) {
	types, err := parseEnum("type", "ENTRY_TYPE_", metadatav1.EntryType_value, q.Types)
	if err != nil {
		return nil, 0, err
	}
	statuses, err := parseEnum("status", "ENTRY_STATUS_", metadatav1.EntryStatus_value, q.Statuses)
	if err != nil {
		return nil, 0, err
	}
	var sort metav1.SortField
	if q.Sort != "" {
		sv, ok := metav1.SortField_value["SORT_FIELD_"+strings.ToUpper(q.Sort)]
		if !ok || sv == 0 {
			return nil, 0, &BadFilterError{Field: "sort", Value: q.Sort}
		}
		sort = metav1.SortField(sv)
	}
	f := &metav1.EntryFilter{
		Genres:         q.Genres,
		ContentRatings: s.allowedRatings(ctx, v),
		YearFrom:       q.YearFrom,
		YearTo:         q.YearTo,
	}
	for _, t := range types {
		f.Types = append(f.Types, metadatav1.EntryType(t))
	}
	for _, st := range statuses {
		f.Statuses = append(f.Statuses, metadatav1.EntryStatus(st))
	}
	return f, sort, nil
}

// Search searches the catalogue by text, showing only what v may see.
func (s *Service) Search(ctx context.Context, q Query, v Viewer) (*metav1.SearchResponse, error) {
	client, err := s.Client()
	if err != nil {
		return nil, err
	}
	f, sort, err := s.filter(ctx, q, v)
	if err != nil {
		return nil, err
	}
	res, err := client.Meta.Search(ctx, connect.NewRequest(&metav1.SearchRequest{
		Query: q.Text, Filter: f, Sort: sort, Reverse: q.Reverse,
		PageSize: q.PageSize, PageToken: q.PageToken, Language: q.Language,
	}))
	if err != nil {
		return nil, wrap("search", err)
	}
	rememberCovers(s, res.Msg.GetHits(), func(h *metav1.SearchHit) *metav1.EntrySummary { return h.GetEntry() })
	return res.Msg, nil
}

// Browse lists the catalogue with filters, showing only what v may see.
func (s *Service) Browse(ctx context.Context, q Query, v Viewer) (*metav1.BrowseResponse, error) {
	client, err := s.Client()
	if err != nil {
		return nil, err
	}
	f, sort, err := s.filter(ctx, q, v)
	if err != nil {
		return nil, err
	}
	res, err := client.Meta.Browse(ctx, connect.NewRequest(&metav1.BrowseRequest{
		Filter: f, Sort: sort, Reverse: q.Reverse,
		PageSize: q.PageSize, PageToken: q.PageToken, Language: q.Language,
	}))
	if err != nil {
		return nil, wrap("browse", err)
	}
	rememberCovers(s, res.Msg.GetEntries(), func(e *metav1.EntrySummary) *metav1.EntrySummary { return e })
	return res.Msg, nil
}

// GetEntry returns one entry, or ErrNotFound when it doesn't exist or its
// rating is above what v may see.
func (s *Service) GetEntry(ctx context.Context, id, language string, v Viewer) (*metav1.Entry, error) {
	client, err := s.Client()
	if err != nil {
		return nil, err
	}
	res, err := client.Meta.GetEntry(ctx, connect.NewRequest(&metav1.GetEntryRequest{Id: id, Language: language}))
	if connect.CodeOf(err) == connect.CodeNotFound || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, wrap("get entry", err)
	}
	e := res.Msg.GetEntry()
	if !slices.Contains(s.allowedRatings(ctx, v), e.GetContentRating()) {
		return nil, ErrNotFound
	}
	s.covers.put(id, CoverOf(e.GetImages()), e.GetContentRating())
	return e, nil
}

// CoverOf picks an entry's cover: the first cover image, else the first
// image.
func CoverOf(images []*metadatav1.Image) *metadatav1.Image {
	for _, im := range images {
		if im.GetKind() == metadatav1.ImageKind_IMAGE_KIND_COVER && im.GetUrl() != "" {
			return im
		}
	}
	for _, im := range images {
		if im.GetUrl() != "" {
			return im
		}
	}
	return nil
}

// CoverURL returns the source URL of an entry's cover, if v may see the
// entry. Covers seen in recent lists are remembered, so showing a page of
// results doesn't cost one lookup per cover.
func (s *Service) CoverURL(ctx context.Context, id string, v Viewer) (string, error) {
	if c, ok := s.covers.get(id); ok {
		if !slices.Contains(s.allowedRatings(ctx, v), c.rating) {
			return "", ErrNotFound
		}
		return c.url, nil
	}
	e, err := s.GetEntry(ctx, id, "", v)
	if err != nil {
		return "", err
	}
	if c := CoverOf(e.GetImages()); c != nil {
		return c.GetUrl(), nil
	}
	return "", nil
}

func rememberCovers[T any](s *Service, items []T, summary func(T) *metav1.EntrySummary) {
	for _, it := range items {
		e := summary(it)
		s.covers.put(e.GetId(), e.GetCover(), e.GetContentRating())
	}
}

type coverInfo struct {
	url    string
	rating metadatav1.ContentRating
}

// coverIndex remembers entry covers, bounded: it starts over when full.
type coverIndex struct {
	mu sync.Mutex
	m  map[string]coverInfo
}

const coverIndexMax = 20000

func (c *coverIndex) put(id string, im *metadatav1.Image, rating metadatav1.ContentRating) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= coverIndexMax {
		c.m = make(map[string]coverInfo)
	}
	c.m[id] = coverInfo{url: im.GetUrl(), rating: rating}
}

func (c *coverIndex) get(id string) (coverInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	return v, ok
}
