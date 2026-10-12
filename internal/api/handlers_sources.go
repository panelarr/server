// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/users"
)

// sourceViewer is what the caller may see of the sources.
func (s *Server) sourceViewer(ctx context.Context) plugins.Viewer {
	p := auth.FromContext(ctx)
	return plugins.Viewer{Grants: users.Effective(p.User), Admin: p.IsAdmin(), HideUnknown: s.deps.Meta.HideUnknown(ctx)}
}

// browseViewer is sourceViewer for browsing a source directly (admins):
// an admin may also try a source turned off.
func (s *Server) browseViewer(ctx context.Context) plugins.Viewer {
	v := s.sourceViewer(ctx)
	v.Testing = v.Admin
	return v
}

// imageURL is the server's signed URL for an image of a source (a cover).
func (s *Server) imageURL(sourceID, ref string) *string {
	if ref == "" {
		return nil
	}
	q := url.Values{"ref": {ref}, "sig": {s.deps.Signer.Sign(sourceID, ref)}}
	u := BasePath + "/sources/" + url.PathEscape(sourceID) + "/image?" + q.Encode()
	return &u
}

// iconURL is the server's signed URL for a source's icon. Icons are fetched
// from the plugin's address, not through the source (see Host.Icon); the
// kind is part of the signature. A source without an icon of its own shows
// its package's.
func (s *Server) iconURL(sourceID, ref, packageID string) *string {
	if ref == "" && packageID != "" {
		ref = plugins.PackageIconRef
	}
	if ref == "" {
		return nil
	}
	q := url.Values{"ref": {ref}, "kind": {"icon"}, "sig": {s.deps.Signer.Sign("icon", sourceID, ref)}}
	u := BasePath + "/sources/" + url.PathEscape(sourceID) + "/image?" + q.Encode()
	return &u
}

func (s *Server) toSourceInfo(src plugins.Source) apigen.SourceInfo {
	out := apigen.SourceInfo{
		Id: src.ID, PluginId: src.PluginID, Name: src.Name, Lang: src.Lang,
		ContentRating: apigen.SourceInfoContentRating(src.ContentRating),
		IconUrl:       s.iconURL(src.ID, src.IconUrl, src.PackageID), HomeUrl: nonEmpty(src.HomeUrl),
		Capabilities: nonNil(src.Capabilities), Enabled: src.Enabled, Present: src.Present,
		PackageId: nonEmpty(src.PackageID), PackageName: nonEmpty(src.PackageName),
		Health: apigen.SourceHealth{
			Status: apigen.SourceHealthStatus(src.Health), Reason: nonEmpty(src.HealthReason),
			Message: nonEmpty(src.HealthMessage), CheckedAt: src.HealthCheckedAt,
		},
	}
	if !src.Present && src.UnavailableReason != "" {
		r := apigen.SourceInfoUnavailableReason(src.UnavailableReason)
		out.UnavailableReason = &r
	}
	return out
}

// ListSources lists the sources (admins), filtered.
func (s *Server) ListSources(ctx context.Context, req apigen.ListSourcesRequestObject) (apigen.ListSourcesResponseObject, error) {
	rows, err := s.deps.Plugins.Sources(ctx, s.sourceViewer(ctx), deref(req.Params.All))
	if err != nil {
		return nil, err
	}
	out := apigen.ListSources200JSONResponse{Sources: make([]apigen.SourceInfo, 0, len(rows))}
	for _, src := range rows {
		pr := req.Params
		if (pr.Lang != nil && *pr.Lang != "" && src.Lang != *pr.Lang) ||
			(pr.PluginId != nil && src.PluginID != *pr.PluginId) ||
			(pr.PackageId != nil && src.PackageID != *pr.PackageId) ||
			(pr.Enabled != nil && src.Enabled != *pr.Enabled) ||
			(pr.Present != nil && src.Present != *pr.Present) {
			continue
		}
		out.Sources = append(out.Sources, s.toSourceInfo(src))
	}
	return out, nil
}

// UpdateSource turns a source on or off.
func (s *Server) UpdateSource(ctx context.Context, req apigen.UpdateSourceRequestObject) (apigen.UpdateSourceResponseObject, error) {
	src, err := s.deps.Plugins.SetSourceEnabled(ctx, req.SourceId, req.Body.Enabled)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.UpdateSource200JSONResponse(s.toSourceInfo(src)), nil
}

func (s *Server) toSeries(sourceID string, se *sourcev1.Series) apigen.SourceSeries {
	return apigen.SourceSeries{
		Key: se.GetKey(), Title: se.GetTitle(), AltTitles: nonNil(se.GetAltTitles()),
		Authors: nonNil(se.GetAuthors()), Artists: nonNil(se.GetArtists()),
		Description: nonEmpty(se.GetDescription()), Genres: nonNil(se.GetGenres()),
		Status:   nonEmpty(enumLower(se.GetStatus().String(), "SERIES_STATUS_")),
		CoverUrl: s.imageURL(sourceID, se.GetCoverUrl()), Url: nonEmpty(se.GetUrl()),
	}
}

func (s *Server) toPage(sourceID string, p plugins.Page) apigen.SourceSeriesPage {
	out := apigen.SourceSeriesPage{Series: make([]apigen.SourceSeries, len(p.Series)), HasNextPage: p.HasNextPage}
	for i, se := range p.Series {
		out.Series[i] = s.toSeries(sourceID, se)
	}
	return out
}

func page(p *int) uint32 {
	if p == nil {
		return 1
	}
	return uint32(*p) //nolint:gosec // bounded by the spec
}

// SourcePopular lists a source's popular series.
func (s *Server) SourcePopular(ctx context.Context, req apigen.SourcePopularRequestObject) (apigen.SourcePopularResponseObject, error) {
	p, err := s.deps.Plugins.Popular(ctx, s.browseViewer(ctx), req.SourceId, page(req.Params.Page))
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SourcePopular200JSONResponse(s.toPage(req.SourceId, p)), nil
}

// SourceLatest lists a source's latest updates.
func (s *Server) SourceLatest(ctx context.Context, req apigen.SourceLatestRequestObject) (apigen.SourceLatestResponseObject, error) {
	p, err := s.deps.Plugins.Latest(ctx, s.browseViewer(ctx), req.SourceId, page(req.Params.Page))
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SourceLatest200JSONResponse(s.toPage(req.SourceId, p)), nil
}

// SourceSearch searches a source.
func (s *Server) SourceSearch(ctx context.Context, req apigen.SourceSearchRequestObject) (apigen.SourceSearchResponseObject, error) {
	query, filters, err := searchRequest(req.Body)
	if err != nil {
		return nil, err
	}
	p, err := s.deps.Plugins.Search(ctx, s.browseViewer(ctx), req.SourceId, query, page(req.Body.Page), filters)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SourceSearch200JSONResponse(s.toPage(req.SourceId, p)), nil
}

// searchRequest reads a source search's query and filters.
func searchRequest(b *apigen.SourceSearchRequest) (string, []*sourcev1.FilterValue, error) {
	var filters []*sourcev1.FilterValue
	if b.Filters != nil {
		for i, raw := range *b.Filters {
			j, err := json.Marshal(raw)
			if err != nil {
				return "", nil, err
			}
			fv := &sourcev1.FilterValue{}
			if err := protojson.Unmarshal(j, fv); err != nil {
				return "", nil, fieldProblem("filters["+strconv.Itoa(i)+"]", "not a filter value: "+err.Error())
			}
			filters = append(filters, fv)
		}
	}
	if deref(b.Query) == "" && len(filters) == 0 {
		return "", nil, fieldProblem("query", "give a query or filters")
	}
	return deref(b.Query), filters, nil
}

// SourceFilters returns a source's filters in the contract's JSON form.
func (s *Server) SourceFilters(ctx context.Context, req apigen.SourceFiltersRequestObject) (apigen.SourceFiltersResponseObject, error) {
	res, err := s.deps.Plugins.Filters(ctx, s.browseViewer(ctx), req.SourceId)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	raw, err := protojson.Marshal(res)
	if err != nil {
		return nil, err
	}
	var out apigen.SourceFilters
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Filters == nil {
		out.Filters = []map[string]any{}
	}
	return apigen.SourceFilters200JSONResponse(out), nil
}

// SourceSeries returns a series' details.
func (s *Server) SourceSeries(ctx context.Context, req apigen.SourceSeriesRequestObject) (apigen.SourceSeriesResponseObject, error) {
	se, err := s.deps.Plugins.Series(ctx, s.browseViewer(ctx), req.SourceId, req.Params.Key)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SourceSeries200JSONResponse(s.toSeries(req.SourceId, se)), nil
}

// SourceChapters lists a series' chapters.
func (s *Server) SourceChapters(ctx context.Context, req apigen.SourceChaptersRequestObject) (apigen.SourceChaptersResponseObject, error) {
	chs, err := s.deps.Plugins.Chapters(ctx, s.browseViewer(ctx), req.SourceId, req.Params.Key)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	out := apigen.SourceChapters200JSONResponse{Chapters: make([]apigen.SourceChapter, len(chs))}
	for i, c := range chs {
		ch := apigen.SourceChapter{
			Key: c.GetKey(), Name: c.GetName(), Volume: nonEmpty(c.GetVolume()), Scanlator: nonEmpty(c.GetScanlator()),
			Lang: nonEmpty(c.GetLang()), Url: nonEmpty(c.GetUrl()),
		}
		if c.Number != nil {
			ch.Number = ptr(float32(c.GetNumber()))
		}
		if c.GetUploadedAt() != nil {
			t := c.GetUploadedAt().AsTime()
			ch.UploadedAt = &t
		}
		out.Chapters[i] = ch
	}
	return out, nil
}

// SourceImage serves a signed image of a source, fetched through its plugin
// and cached.
func (s *Server) SourceImage(ctx context.Context, req apigen.SourceImageRequestObject) (apigen.SourceImageResponseObject, error) {
	icon := req.Params.Kind != nil && *req.Params.Kind == apigen.SourceImageParamsKindIcon
	signed := s.deps.Signer.Verify(req.Params.Sig, req.SourceId, req.Params.Ref)
	if icon {
		signed = s.deps.Signer.Verify(req.Params.Sig, "icon", req.SourceId, req.Params.Ref)
	}
	if !signed {
		return nil, NewProblem(http.StatusForbidden, "bad_signature", "not an image link from this server")
	}
	v := s.browseViewer(ctx)
	// Admins list absent sources too; their icons still show.
	v.AnyKnown = icon && v.Admin
	if _, err := s.deps.Plugins.Source(ctx, req.SourceId, v); err != nil {
		return nil, pluginError(ctx, err)
	}
	var (
		img imagecache.Image
		err error
	)
	if icon {
		// Icons may change; failures aren't kept, so the next request
		// tries again.
		img, err = s.deps.Images.GetFresh(ctx, "source-icon\x00"+req.SourceId+"\x00"+req.Params.Ref, iconMaxAge, func(ctx context.Context) (string, io.Reader, error) {
			return s.deps.Plugins.SourceIcon(ctx, v, req.SourceId, req.Params.Ref)
		})
	} else {
		img, err = s.deps.Images.GetWith(ctx, "source\x00"+req.SourceId+"\x00"+req.Params.Ref, func(ctx context.Context) (string, io.Reader, error) {
			ct, data, err := s.deps.Plugins.Image(ctx, v, req.SourceId, req.Params.Ref)
			if err != nil {
				return "", nil, err
			}
			return ct, bytes.NewReader(data), nil
		})
	}
	if errors.Is(err, imagecache.ErrNotImage) {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "not a usable image")
	}
	if err != nil {
		return nil, s.imageError(ctx, err, icon, "source", req.SourceId)
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
	return apigen.SourceImage200ImageResponse{
		Body: f, ContentType: img.ContentType, ContentLength: st.Size(),
		Headers: apigen.SourceImage200ResponseHeaders{CacheControl: ptr("private, max-age=604800")},
	}, nil
}

// imageError answers a failed image or icon fetch. Nothing there (the
// plugin answered 404) is a 404 logged at debug; a real failure is a
// warning, once in a while per subject.
func (s *Server) imageError(ctx context.Context, err error, icon bool, kind, subject string) error {
	if errors.Is(err, plugins.ErrNotFound) {
		s.deps.Log.DebugContext(ctx, kind+" image not found", kind, subject, "icon", icon, "err", err)
		return NewProblem(http.StatusNotFound, CodeNotFound, "no such image")
	}
	if s.iconWarnings.allow(kind + "\x00" + subject) {
		s.deps.Log.WarnContext(ctx, kind+" image failed", kind, subject, "icon", icon, "err", err)
	} else {
		s.deps.Log.DebugContext(ctx, kind+" image failed", kind, subject, "icon", icon, "err", err)
	}
	if p := pluginError(ctx, err); p != err || !icon { //nolint:errorlint // identity: was it mapped
		return p
	}
	return NewProblem(http.StatusBadGateway, "icon_failed", "the icon couldn't be fetched")
}
