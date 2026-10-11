// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"

	"connectrpc.com/connect"

	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/plugins"
)

// pluginError maps plugin host and plugin errors to problems.
func pluginError(ctx context.Context, err error) error {
	var unreachable *plugins.UnreachableError
	var pe *plugin.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unreachable):
		return NewProblem(http.StatusUnprocessableEntity, "plugin_unreachable", unreachable.Reason)
	case errors.Is(err, plugins.ErrNotFound):
		return NewProblem(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, plugins.ErrIDTaken):
		return NewProblem(http.StatusConflict, "plugin_id_taken", "a plugin with this ID exists; choose another ID")
	case errors.Is(err, plugins.ErrInvalidID):
		return fieldProblem("id", err.Error())
	case errors.Is(err, plugins.ErrInvalidURL):
		return fieldProblem("url", "must be http(s)://host[:port]")
	case errors.Is(err, plugins.ErrDisabled):
		return NewProblem(http.StatusConflict, "plugin_disabled", "the plugin is turned off")
	case errors.Is(err, plugins.ErrNoConfig):
		return NewProblem(http.StatusNotFound, "no_config", "the plugin has no settings")
	case errors.Is(err, plugins.ErrBadConfig):
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, plugins.ErrUnsupported):
		return NewProblem(http.StatusUnprocessableEntity, "unsupported", "the source doesn't support this")
	case errors.As(err, &pe):
		switch pe.Code {
		case plugin.CodeInvalidArgument:
			return NewProblem(http.StatusBadRequest, CodeInvalidRequest, pe.Message)
		case plugin.CodeNotFound:
			return NewProblem(http.StatusNotFound, CodeNotFound, pe.Message)
		case plugin.CodeUnimplemented:
			return NewProblem(http.StatusUnprocessableEntity, "unsupported", "the plugin doesn't support this")
		case plugin.CodeResourceExhausted:
			if pe.RetryAfter > 0 {
				if w := responseWriter(ctx); w != nil {
					w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(pe.RetryAfter.Seconds()))))
				}
			}
			return NewProblem(http.StatusTooManyRequests, "rate_limited", "the source is rate limiting; try again later")
		}
		detail := pe.Message
		if pe.Reason != "" {
			detail = string(pe.Reason) + ": " + pe.Message
		}
		return NewProblem(http.StatusBadGateway, "source_failed", detail)
	case connect.CodeOf(err) == connect.CodeUnavailable || connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		return NewProblem(http.StatusBadGateway, "source_failed", "the plugin didn't answer")
	}
	return err
}

func toPlugin(p plugins.Plugin) apigen.Plugin {
	info := p.Info
	out := apigen.Plugin{
		Id: p.ID, Url: p.Url, Enabled: p.Enabled, Name: info.GetName(), Version: info.GetVersion(),
		PluginId: nonEmpty(info.GetId()), Contracts: nonNil(info.GetContracts()), Capabilities: []string{},
		HasConfig: len(info.GetConfigSchema().GetFields()) > 0, Health: apigen.PluginHealth(p.Health),
		HealthMessage: nonEmpty(p.HealthMessage), CheckedAt: p.CheckedAt, SourceCount: p.SourceCount, CreatedAt: p.CreatedAt,
	}
	for _, c := range info.GetCapabilities() {
		if n := enumLower(c.String(), "PLUGIN_CAPABILITY_"); n != "" {
			out.Capabilities = append(out.Capabilities, n)
		}
	}
	if l := info.GetLimits(); l != nil {
		if l.GetMaxConcurrencyPerSource() > 0 {
			out.MaxConcurrencyPerSource = ptr(int(l.GetMaxConcurrencyPerSource()))
		}
		if rl := l.GetDefaultRateLimit(); rl != nil && rl.GetRequests() > 0 {
			out.DefaultRateLimit = &apigen.RateLimit{
				Requests: int(rl.GetRequests()), PeriodSeconds: float32(rl.GetPeriod().AsDuration().Seconds()),
			}
			if rl.GetBurst() > 0 {
				out.DefaultRateLimit.Burst = ptr(int(rl.GetBurst()))
			}
		}
	}
	return out
}

// ListPlugins lists the plugins.
func (s *Server) ListPlugins(ctx context.Context, _ apigen.ListPluginsRequestObject) (apigen.ListPluginsResponseObject, error) {
	rows, err := s.deps.Plugins.List(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.ListPlugins200JSONResponse{Plugins: make([]apigen.Plugin, len(rows))}
	for i, p := range rows {
		out.Plugins[i] = toPlugin(p)
	}
	return out, nil
}

// RegisterPlugin adds a plugin.
func (s *Server) RegisterPlugin(ctx context.Context, req apigen.RegisterPluginRequestObject) (apigen.RegisterPluginResponseObject, error) {
	p, err := s.deps.Plugins.Register(ctx, plugins.NewPlugin{ID: deref(req.Body.Id), URL: req.Body.Url, APIKey: deref(req.Body.ApiKey)})
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.RegisterPlugin201JSONResponse(toPlugin(p)), nil
}

// GetPlugin returns a plugin.
func (s *Server) GetPlugin(ctx context.Context, req apigen.GetPluginRequestObject) (apigen.GetPluginResponseObject, error) {
	p, err := s.deps.Plugins.Get(ctx, req.PluginId)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.GetPlugin200JSONResponse(toPlugin(p)), nil
}

// UpdatePlugin changes a plugin.
func (s *Server) UpdatePlugin(ctx context.Context, req apigen.UpdatePluginRequestObject) (apigen.UpdatePluginResponseObject, error) {
	p, err := s.deps.Plugins.Update(ctx, req.PluginId, plugins.Patch{URL: req.Body.Url, APIKey: req.Body.ApiKey, Enabled: req.Body.Enabled})
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.UpdatePlugin200JSONResponse(toPlugin(p)), nil
}

// DeletePlugin removes a plugin.
func (s *Server) DeletePlugin(ctx context.Context, req apigen.DeletePluginRequestObject) (apigen.DeletePluginResponseObject, error) {
	if err := s.deps.Plugins.Delete(ctx, req.PluginId); err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.DeletePlugin204Response{}, nil
}

// SyncPlugin refreshes a plugin now.
func (s *Server) SyncPlugin(ctx context.Context, req apigen.SyncPluginRequestObject) (apigen.SyncPluginResponseObject, error) {
	p, err := s.deps.Plugins.Sync(ctx, req.PluginId)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SyncPlugin200JSONResponse(toPlugin(p)), nil
}

func toPluginConfig(c plugins.Config) apigen.PluginConfig {
	values := c.Values
	if values == nil {
		values = map[string]any{}
	}
	return apigen.PluginConfig{Schema: c.Schema.AsMap(), Values: values}
}

// GetPluginConfig returns a plugin's configuration.
func (s *Server) GetPluginConfig(ctx context.Context, req apigen.GetPluginConfigRequestObject) (apigen.GetPluginConfigResponseObject, error) {
	c, err := s.deps.Plugins.Config(ctx, req.PluginId)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.GetPluginConfig200JSONResponse(toPluginConfig(c)), nil
}

// SetPluginConfig replaces a plugin's configuration.
func (s *Server) SetPluginConfig(ctx context.Context, req apigen.SetPluginConfigRequestObject) (apigen.SetPluginConfigResponseObject, error) {
	c, err := s.deps.Plugins.SetConfig(ctx, req.PluginId, req.Body.Values)
	if err != nil {
		return nil, pluginError(ctx, err)
	}
	return apigen.SetPluginConfig200JSONResponse(toPluginConfig(c)), nil
}
