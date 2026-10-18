// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"time"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins"
)

// How failed page images are retried, per package and per source.

func msOf(delays []time.Duration) []int {
	out := make([]int, len(delays))
	for i, d := range delays {
		out[i] = int(d.Milliseconds())
	}
	return out
}

func toImageRetries(r plugins.ImageRetries) apigen.ImageRetries {
	out := apigen.ImageRetries{
		TargetKind: apigen.ImageRetriesTargetKind(r.TargetKind), TargetId: r.TargetID,
		Effective: apigen.ImageRetryDelays{DelaysMs: msOf(r.Effective), Attempts: len(r.Effective)},
		Level:     apigen.ImageRetriesLevel(r.Level), FromTargetId: nonEmpty(r.FromID),
	}
	if r.Own != nil {
		out.Own = &struct {
			DelaysMs  []int     `json:"delays_ms"`
			UpdatedAt time.Time `json:"updated_at"`
		}{DelaysMs: msOf(r.Own.Delays), UpdatedAt: r.Own.UpdatedAt}
	}
	return out
}

func delaysIn(ms []int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, d := range ms {
		out[i] = time.Duration(d) * time.Millisecond
	}
	return out
}

func imageRetriesError(ctx context.Context, err error) error {
	if errors.Is(err, plugins.ErrBadImageRetries) {
		return fieldProblem("delays_ms", err.Error())
	}
	return pluginError(ctx, err)
}

// GetPackageImageRetries returns how a package's sources retry images.
func (s *Server) GetPackageImageRetries(ctx context.Context, req apigen.GetPackageImageRetriesRequestObject) (apigen.GetPackageImageRetriesResponseObject, error) {
	r, err := s.deps.Plugins.PackageImageRetries(ctx, req.PluginId, req.PackageId)
	if err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.GetPackageImageRetries200JSONResponse(toImageRetries(r)), nil
}

// SetPackageImageRetries sets how a package's sources retry images.
func (s *Server) SetPackageImageRetries(ctx context.Context, req apigen.SetPackageImageRetriesRequestObject) (apigen.SetPackageImageRetriesResponseObject, error) {
	r, err := s.deps.Plugins.SetPackageImageRetries(ctx, req.PluginId, req.PackageId, delaysIn(req.Body.DelaysMs))
	if err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.SetPackageImageRetries200JSONResponse(toImageRetries(r)), nil
}

// ClearPackageImageRetries removes a package's setting.
func (s *Server) ClearPackageImageRetries(ctx context.Context, req apigen.ClearPackageImageRetriesRequestObject) (apigen.ClearPackageImageRetriesResponseObject, error) {
	if err := s.deps.Plugins.ClearPackageImageRetries(ctx, req.PluginId, req.PackageId); err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.ClearPackageImageRetries204Response{}, nil
}

// GetSourceImageRetries returns how a source retries images.
func (s *Server) GetSourceImageRetries(ctx context.Context, req apigen.GetSourceImageRetriesRequestObject) (apigen.GetSourceImageRetriesResponseObject, error) {
	r, err := s.deps.Plugins.SourceImageRetries(ctx, req.SourceId)
	if err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.GetSourceImageRetries200JSONResponse(toImageRetries(r)), nil
}

// SetSourceImageRetries sets a source's own setting.
func (s *Server) SetSourceImageRetries(ctx context.Context, req apigen.SetSourceImageRetriesRequestObject) (apigen.SetSourceImageRetriesResponseObject, error) {
	r, err := s.deps.Plugins.SetSourceImageRetries(ctx, req.SourceId, delaysIn(req.Body.DelaysMs))
	if err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.SetSourceImageRetries200JSONResponse(toImageRetries(r)), nil
}

// ClearSourceImageRetries removes a source's own setting.
func (s *Server) ClearSourceImageRetries(ctx context.Context, req apigen.ClearSourceImageRetriesRequestObject) (apigen.ClearSourceImageRetriesResponseObject, error) {
	if err := s.deps.Plugins.ClearSourceImageRetries(ctx, req.SourceId); err != nil {
		return nil, imageRetriesError(ctx, err)
	}
	return apigen.ClearSourceImageRetries204Response{}, nil
}
