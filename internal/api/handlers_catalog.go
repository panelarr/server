// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/store/db"
	"github.com/panelarr/server/internal/users"
)

const defaultCatalogPageSize = 30

// viewer is what the caller may see of the catalogue.
func viewer(ctx context.Context) meta.Viewer {
	p := auth.FromContext(ctx)
	return meta.Viewer{Ceiling: users.Effective(p.User).ContentRatingCeiling, Admin: p.IsAdmin()}
}

var langTag = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

// language is the lang parameter, else the first Accept-Language tag.
func language(ctx context.Context, param *string) string {
	if param != nil && *param != "" {
		return *param
	}
	al, _ := ctx.Value(acceptLanguageKey).(string)
	first, _, _ := strings.Cut(al, ",")
	first, _, _ = strings.Cut(first, ";")
	first = strings.TrimSpace(first)
	if langTag.MatchString(first) {
		return first
	}
	return ""
}

func coverPath(id string) string { return BasePath + "/catalog/entries/" + id + "/cover" }

func toCover(id string, im *metadatav1.Image) *apigen.CatalogCover {
	if im == nil || im.GetUrl() == "" {
		return nil
	}
	c := &apigen.CatalogCover{Url: coverPath(id)}
	if im.GetWidth() > 0 {
		c.Width = ptr(int(im.GetWidth()))
	}
	if im.GetHeight() > 0 {
		c.Height = ptr(int(im.GetHeight()))
	}
	if im.GetBlurhash() != "" {
		c.Blurhash = ptr(im.GetBlurhash())
	}
	if im.GetThumbhash() != "" {
		c.Thumbhash = ptr(im.GetThumbhash())
	}
	return c
}

func toAttribution(a []*metadatav1.Attribution) []apigen.Attribution {
	out := make([]apigen.Attribution, 0, len(a))
	for _, x := range a {
		at := apigen.Attribution{Provider: x.GetProviderName()}
		at.Url = nonEmpty(x.GetUrl())
		at.License = nonEmpty(x.GetLicense())
		at.LicenseUrl = nonEmpty(x.GetLicenseUrl())
		at.Notice = nonEmpty(x.GetNotice())
		out = append(out, at)
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func positive(n int32) *int {
	if n <= 0 {
		return nil
	}
	v := int(n)
	return &v
}

func toSummary(e *metav1.EntrySummary) apigen.CatalogEntrySummary {
	out := apigen.CatalogEntrySummary{
		Id: e.GetId(), Title: e.GetTitle(),
		Type: nonEmpty(meta.TypeName(e.GetType())), Status: nonEmpty(meta.StatusName(e.GetStatus())),
		ContentRating:    apigen.EntryContentRating(meta.RatingName(e.GetContentRating())),
		Year:             positive(e.GetYear()),
		OriginalLanguage: nonEmpty(e.GetOriginalLanguage()),
		Cover:            toCover(e.GetId(), e.GetCover()),
		Genres:           nonNil(e.GetGenres()),
		Attribution:      toAttribution(e.GetAttributions()),
	}
	if e.Rating != nil {
		out.Rating = ptr(float32(e.GetRating()))
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func strs[T ~string](p *[]T) []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(*p))
	for i, v := range *p {
		out[i] = string(v)
	}
	return out
}

func catalogQuery(ctx context.Context, types, statuses, genres *[]string, yearFrom, yearTo *int, sort *string, reverse *bool, lang *string, pageSize *int, pageToken *string) meta.Query {
	q := meta.Query{
		Types: strs(types), Statuses: strs(statuses), Genres: strs(genres),
		Sort: deref(sort), Reverse: deref(reverse), Language: language(ctx, lang),
		PageSize: defaultCatalogPageSize, PageToken: deref(pageToken),
	}
	if yearFrom != nil {
		q.YearFrom = int32(*yearFrom) //nolint:gosec // bounded by the spec
	}
	if yearTo != nil {
		q.YearTo = int32(*yearTo) //nolint:gosec // bounded by the spec
	}
	if pageSize != nil {
		q.PageSize = int32(*pageSize) //nolint:gosec // bounded by the spec
	}
	return q
}

// libraryWorks maps catalogue entries in the caller's library to their
// works.
func (s *Server) libraryWorks(ctx context.Context, entryIDs []string) (map[string]uuid.UUID, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}
	rows, err := s.deps.Store.LibraryWorksByEntries(ctx, db.LibraryWorksByEntriesParams{UserID: auth.FromContext(ctx).User.ID, EntryIds: entryIDs})
	if err != nil {
		return nil, err
	}
	out := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		out[r.MetaEntryID] = r.ID
	}
	return out, nil
}

// SearchCatalog searches the catalogue.
func (s *Server) SearchCatalog(ctx context.Context, req apigen.SearchCatalogRequestObject) (apigen.SearchCatalogResponseObject, error) {
	p := req.Params
	var sort *string
	if p.Sort != nil {
		sort = ptr(string(*p.Sort))
	}
	q := catalogQuery(ctx, p.Type, p.Status, p.Genre, p.YearFrom, p.YearTo, sort, p.Reverse, p.Lang, p.PageSize, p.PageToken)
	q.Text = p.Q
	res, err := s.deps.Meta.Search(ctx, q, viewer(ctx))
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.SearchCatalog200JSONResponse{
		Hits: make([]apigen.CatalogSearchHit, 0, len(res.GetHits())), NextPageToken: res.GetNextPageToken(), NoProviders: res.GetNoProviders(),
	}
	if res.GetTotalSize() > 0 {
		out.TotalSize = ptr(res.GetTotalSize())
	}
	ids := make([]string, 0, len(res.GetHits()))
	for _, h := range res.GetHits() {
		ids = append(ids, h.GetEntry().GetId())
	}
	inLib, err := s.libraryWorks(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, h := range res.GetHits() {
		hit := apigen.CatalogSearchHit{Entry: toSummary(h.GetEntry())}
		if w, ok := inLib[h.GetEntry().GetId()]; ok {
			hit.Entry.LibraryWorkId = &w
		}
		if m := h.GetMatched(); m != "" && m != h.GetEntry().GetTitle() {
			hit.Matched = &m
		}
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

// BrowseCatalog lists the catalogue.
func (s *Server) BrowseCatalog(ctx context.Context, req apigen.BrowseCatalogRequestObject) (apigen.BrowseCatalogResponseObject, error) {
	p := req.Params
	var sort *string
	if p.Sort != nil {
		sort = ptr(string(*p.Sort))
	}
	res, err := s.deps.Meta.Browse(ctx, catalogQuery(ctx, p.Type, p.Status, p.Genre, p.YearFrom, p.YearTo, sort, p.Reverse, p.Lang, p.PageSize, p.PageToken), viewer(ctx))
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.BrowseCatalog200JSONResponse{
		Entries: make([]apigen.CatalogEntrySummary, 0, len(res.GetEntries())), NextPageToken: res.GetNextPageToken(), NoProviders: res.GetNoProviders(),
	}
	if res.GetTotalSize() > 0 {
		out.TotalSize = ptr(res.GetTotalSize())
	}
	ids := make([]string, 0, len(res.GetEntries()))
	for _, e := range res.GetEntries() {
		ids = append(ids, e.GetId())
	}
	inLib, err := s.libraryWorks(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, e := range res.GetEntries() {
		sum := toSummary(e)
		if w, ok := inLib[e.GetId()]; ok {
			sum.LibraryWorkId = &w
		}
		out.Entries = append(out.Entries, sum)
	}
	return out, nil
}

// GetCatalogEntry returns one entry in full.
func (s *Server) GetCatalogEntry(ctx context.Context, req apigen.GetCatalogEntryRequestObject) (apigen.GetCatalogEntryResponseObject, error) {
	e, err := s.deps.Meta.GetEntry(ctx, req.EntryId, language(ctx, req.Params.Lang), viewer(ctx))
	if err != nil {
		return nil, metaError(err)
	}
	out := toEntry(e)
	inLib, err := s.libraryWorks(ctx, []string{e.GetId()})
	if err != nil {
		return nil, err
	}
	if w, ok := inLib[e.GetId()]; ok {
		out.LibraryWorkId = &w
	}
	return apigen.GetCatalogEntry200JSONResponse(out), nil
}

func toEntry(e *metav1.Entry) apigen.CatalogEntry {
	out := apigen.CatalogEntry{
		Id: e.GetId(), Title: e.GetTitle(),
		Type: nonEmpty(meta.TypeName(e.GetType())), Status: nonEmpty(meta.StatusName(e.GetStatus())),
		ContentRating:    apigen.EntryContentRating(meta.RatingName(e.GetContentRating())),
		OriginalLanguage: nonEmpty(e.GetOriginalLanguage()),
		Year:             positive(e.GetDates().GetStart().GetYear()),
		EndYear:          positive(e.GetDates().GetEnd().GetYear()),
		Cover:            toCover(e.GetId(), meta.CoverOf(e.GetImages())),
		Titles:           []apigen.CatalogTitle{},
		Descriptions:     []apigen.CatalogDescription{},
		Credits:          []apigen.CatalogCredit{},
		Genres:           []string{},
		Tags:             []apigen.CatalogTag{},
		ExternalIds:      []apigen.CatalogExternalId{},
		Links:            []apigen.CatalogLink{},
		Attribution:      []apigen.Attribution{},
	}
	for _, t := range e.GetTitles() {
		out.Titles = append(out.Titles, apigen.CatalogTitle{
			Text: t.GetText(), Language: nonEmpty(t.GetLanguage()),
			Kind: orUnknown(enumLower(t.GetKind().String(), "TITLE_KIND_")), Primary: t.GetIsPrimary(),
		})
	}
	for _, d := range e.GetDescriptions() {
		f := enumLower(d.GetFormat().String(), "DESCRIPTION_FORMAT_")
		if f == "" {
			f = "plain"
		}
		out.Descriptions = append(out.Descriptions, apigen.CatalogDescription{
			Text: d.GetText(), Language: nonEmpty(d.GetLanguage()), Format: apigen.CatalogDescriptionFormat(f),
		})
	}
	for _, c := range e.GetCredits() {
		role := enumLower(c.GetRole().String(), "PERSON_ROLE_")
		if role == "" {
			role = strings.ToLower(c.GetRawRole())
		}
		out.Credits = append(out.Credits, apigen.CatalogCredit{
			Name: c.GetPerson().GetName(), Role: orUnknown(role), PersonId: nonEmpty(c.GetPerson().GetId()),
		})
	}
	for _, t := range e.GetTags() {
		tag := t.GetTag()
		if tag.GetIsGenre() {
			out.Genres = append(out.Genres, tag.GetName())
		}
		out.Tags = append(out.Tags, apigen.CatalogTag{Name: tag.GetName(), Genre: tag.GetIsGenre(), Spoiler: t.GetIsSpoiler() || tag.GetIsSpoiler()})
	}
	for _, x := range e.GetExternalIds() {
		out.ExternalIds = append(out.ExternalIds, apigen.CatalogExternalId{Provider: x.GetProvider(), Id: x.GetId(), Url: nonEmpty(x.GetUrl())})
	}
	for _, l := range e.GetLinks() {
		out.Links = append(out.Links, apigen.CatalogLink{
			Type: orUnknown(enumLower(l.GetType().String(), "LINK_TYPE_")), Url: l.GetUrl(), Title: nonEmpty(l.GetTitle()),
		})
	}
	attrs := make([]*metadatav1.Attribution, 0, len(e.GetSources()))
	for _, src := range e.GetSources() {
		if src.GetAttribution() != nil {
			attrs = append(attrs, src.GetAttribution())
		}
	}
	out.Attribution = toAttribution(attrs)
	return out
}

func enumLower(s, prefix string) string {
	s = strings.ToLower(strings.TrimPrefix(s, prefix))
	if s == "unspecified" {
		return ""
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// GetCatalogEntryCover serves an entry's cover from the image cache.
func (s *Server) GetCatalogEntryCover(ctx context.Context, req apigen.GetCatalogEntryCoverRequestObject) (apigen.GetCatalogEntryCoverResponseObject, error) {
	u, err := s.deps.Meta.CoverURL(ctx, req.EntryId, viewer(ctx))
	if err != nil {
		return nil, metaError(err)
	}
	if u == "" {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "the entry has no cover")
	}
	img, err := s.deps.Images.Get(ctx, u)
	if err != nil {
		s.deps.Log.WarnContext(ctx, "cover fetch failed", "entry_id", req.EntryId, "err", err)
		if errors.Is(err, imagecache.ErrNotImage) {
			return nil, NewProblem(http.StatusNotFound, CodeNotFound, "the entry has no usable cover")
		}
		return nil, NewProblem(http.StatusBadGateway, "image_unavailable", "the cover couldn't be fetched")
	}
	f, err := os.Open(img.Path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return apigen.GetCatalogEntryCover200ImageResponse{
		Body: f, ContentType: img.ContentType, ContentLength: st.Size(),
		Headers: apigen.GetCatalogEntryCover200ResponseHeaders{CacheControl: ptr("private, max-age=86400")},
	}, nil
}
