// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"

	metav1 "github.com/panelarr/meta/gen/panelarr/meta/v1"
	metadatav1 "github.com/panelarr/meta/gen/panelarr/metadata/v1"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/meta"
)

// The metadata service's own admin inside Panelarr's: providers, sync
// runs, health and refreshing an entry.

func metaHealthName(h metadatav1.HealthStatus) string {
	switch h {
	case metadatav1.HealthStatus_HEALTH_STATUS_OK:
		return "ok"
	case metadatav1.HealthStatus_HEALTH_STATUS_DEGRADED:
		return "degraded"
	case metadatav1.HealthStatus_HEALTH_STATUS_DOWN:
		return "down"
	}
	return "unknown"
}

func toMetaSyncRun(r *metav1.SyncRun) apigen.MetaSyncRun {
	status := enumName(r.GetStatus().String(), "SYNC_RUN_STATUS_")
	if status == "unspecified" {
		status = "unknown"
	}
	out := apigen.MetaSyncRun{
		Id: r.GetId(), Provider: r.GetProvider(), Kind: r.GetKind(), Status: apigen.MetaSyncRunStatus(status), Trigger: r.GetTrigger(),
		SnapshotId: nonEmpty(r.GetSnapshotId()), CursorStart: nonEmpty(r.GetCursorStart()), CursorEnd: nonEmpty(r.GetCursorEnd()),
		Upserts: r.GetUpserts(), Changed: r.GetChanged(), Removals: r.GetRemovals(), Merges: r.GetMerges(), Skipped: r.GetSkipped(),
		MarkedRemoved: r.GetMarkedRemoved(), Error: nonEmpty(r.GetError()), Notices: []apigen.MetaNotice{},
	}
	if t := r.GetStartedAt(); t != nil {
		at := t.AsTime()
		out.StartedAt = &at
	}
	if t := r.GetFinishedAt(); t != nil {
		at := t.AsTime()
		out.FinishedAt = &at
	}
	for _, n := range r.GetNotices() {
		mn := apigen.MetaNotice{Level: enumName(n.GetLevel().String(), "NOTICE_LEVEL_"), Message: n.GetMessage(), Ref: nonEmpty(n.GetRef())}
		if n.GetAt() != nil {
			at := n.GetAt().AsTime()
			mn.At = &at
		}
		out.Notices = append(out.Notices, mn)
	}
	if p := r.GetProgress(); p != nil {
		m := p.AsMap()
		out.Progress = &m
	}
	return out
}

// ListMetaProviders lists meta's providers.
func (s *Server) ListMetaProviders(ctx context.Context, _ apigen.ListMetaProvidersRequestObject) (apigen.ListMetaProvidersResponseObject, error) {
	list, err := s.deps.Meta.Providers(ctx)
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.ListMetaProviders200JSONResponse{Providers: make([]apigen.MetaProviderDetail, 0, len(list))}
	for _, p := range list {
		d := apigen.MetaProviderDetail{
			Id: p.GetId(), Mode: p.GetMode(), PluginId: nonEmpty(p.GetInfo().GetId()), PluginName: nonEmpty(p.GetInfo().GetName()),
			PluginVersion: nonEmpty(p.GetInfo().GetVersion()), Health: apigen.MetaProviderDetailHealth(metaHealthName(p.GetHealth())),
			HealthMessage: nonEmpty(p.GetHealthMessage()), Schedules: p.GetSchedules(), RecordCount: p.GetRecordCount(),
			LastRuns: []apigen.MetaSyncRun{},
		}
		if d.Schedules == nil {
			d.Schedules = map[string]string{}
		}
		for _, r := range p.GetLastRuns() {
			d.LastRuns = append(d.LastRuns, toMetaSyncRun(r))
		}
		out.Providers = append(out.Providers, d)
	}
	return out, nil
}

// ListMetaSyncRuns lists provider sync runs.
func (s *Server) ListMetaSyncRuns(ctx context.Context, req apigen.ListMetaSyncRunsRequestObject) (apigen.ListMetaSyncRunsResponseObject, error) {
	size := min(pageSize(req.Params.PageSize), 100)
	runs, next, err := s.deps.Meta.SyncRuns(ctx, deref(req.Params.Provider), int32(size), deref(req.Params.PageToken)) //nolint:gosec // at most 100
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.ListMetaSyncRuns200JSONResponse{Runs: make([]apigen.MetaSyncRun, len(runs)), NextPageToken: next}
	for i, r := range runs {
		out.Runs[i] = toMetaSyncRun(r)
	}
	return out, nil
}

// GetMetaSyncRun returns one sync run.
func (s *Server) GetMetaSyncRun(ctx context.Context, req apigen.GetMetaSyncRunRequestObject) (apigen.GetMetaSyncRunResponseObject, error) {
	r, err := s.deps.Meta.SyncRun(ctx, req.SyncRunId)
	if errors.Is(err, meta.ErrRunNotFound) {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "no such sync run")
	}
	if err != nil {
		return nil, metaError(err)
	}
	return apigen.GetMetaSyncRun200JSONResponse(toMetaSyncRun(r)), nil
}

// GetMetaHealth returns meta's own health.
func (s *Server) GetMetaHealth(ctx context.Context, _ apigen.GetMetaHealthRequestObject) (apigen.GetMetaHealthResponseObject, error) {
	h, err := s.deps.Meta.Health(ctx)
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.GetMetaHealth200JSONResponse{
		Status: apigen.MetaServiceHealthStatus(metaHealthName(h.GetStatus())), Version: nonEmpty(h.GetVersion()),
		Components: make([]apigen.MetaComponentHealth, 0, len(h.GetComponents())),
	}
	for _, c := range h.GetComponents() {
		mc := apigen.MetaComponentHealth{
			Name: c.GetName(), Kind: c.GetKind(), Status: apigen.MetaComponentHealthStatus(metaHealthName(c.GetStatus())),
			Message: nonEmpty(c.GetMessage()), Details: c.GetDetails(),
		}
		if mc.Details == nil {
			mc.Details = map[string]string{}
		}
		if c.GetLatency() != nil {
			mc.LatencyMs = ptr(int(c.GetLatency().AsDuration().Milliseconds()))
		}
		if c.GetCheckedAt() != nil {
			at := c.GetCheckedAt().AsTime()
			mc.CheckedAt = &at
		}
		out.Components = append(out.Components, mc)
	}
	return out, nil
}

// RefreshMetaEntry asks an entry's providers for fresh data.
func (s *Server) RefreshMetaEntry(ctx context.Context, req apigen.RefreshMetaEntryRequestObject) (apigen.RefreshMetaEntryResponseObject, error) {
	res, err := s.deps.Meta.RefreshEntry(ctx, req.EntryId)
	if err != nil {
		return nil, metaError(err)
	}
	out := apigen.RefreshMetaEntry200JSONResponse{Refreshed: res.GetRefreshed(), Failed: res.GetFailed()}
	if out.Refreshed == nil {
		out.Refreshed = []string{}
	}
	if out.Failed == nil {
		out.Failed = map[string]string{}
	}
	return out, nil
}
