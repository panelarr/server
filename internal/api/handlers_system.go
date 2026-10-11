// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/buildinfo"
)

// GetSystemInfo identifies the server and says whether it needs setting up.
func (s *Server) GetSystemInfo(ctx context.Context, _ apigen.GetSystemInfoRequestObject) (apigen.GetSystemInfoResponseObject, error) {
	has, err := s.deps.Users.HasUsers(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.GetSystemInfo200JSONResponse{Name: apigen.SystemInfoNamePanelarr, Version: buildinfo.Version, SetupRequired: !has}, nil
}

// GetSystemHealth shows every component's health, with errors (admins).
func (s *Server) GetSystemHealth(ctx context.Context, _ apigen.GetSystemHealthRequestObject) (apigen.GetSystemHealthResponseObject, error) {
	overall, results := runChecks(ctx, s.deps.Checks)
	out := apigen.GetSystemHealth200JSONResponse{
		Status: apigen.HealthStatus(overall), Version: buildinfo.Version,
		Components: make([]apigen.ComponentHealth, 0, len(results)),
	}
	for _, r := range results {
		c := apigen.ComponentHealth{Name: r.Name, Required: r.Required, Status: apigen.ComponentHealthStatusOk, LatencyMs: ptr(int(r.Latency.Milliseconds()))}
		if r.Err != nil {
			c.Status = apigen.ComponentHealthStatusFailing
			c.Error = ptr(r.Err.Error())
		}
		out.Components = append(out.Components, c)
	}
	return out, nil
}
