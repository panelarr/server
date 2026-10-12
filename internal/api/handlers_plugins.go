// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/api/apigen"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/imagecache"
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
	case errors.Is(err, plugins.ErrNoPackages):
		return NewProblem(http.StatusUnprocessableEntity, "no_packages", "the plugin doesn't install packages")
	case errors.As(err, &pe) && pe.Reason == plugin.ReasonStoreFetchFailed:
		return NewProblem(http.StatusBadGateway, "store_fetch_failed", pe.Message)
	case errors.As(err, &pe) && pe.Reason == plugin.ReasonStoreInvalid:
		return NewProblem(http.StatusBadGateway, "store_invalid", pe.Message)
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

func (s *Server) toPackage(pluginID string, p *sourcev1.Package, task *uuid.UUID) apigen.Package {
	out := apigen.Package{
		Id: p.GetId(), Name: p.GetName(), Langs: nonNil(p.GetLangs()), Version: p.GetVersion(),
		Installed: p.GetInstalled(), InstalledVersion: nonEmpty(p.GetInstalledVersion()),
		UpdateAvailable: p.GetUpdateAvailable(), ContentRating: apigen.PackageContentRating(plugins.RatingName(p.GetContentRating())),
		Supported: p.GetSupported(), Offers: []apigen.PackageOffer{}, Sources: []apigen.PackageSource{},
		EnabledLangs: nonNil(p.GetSettings().GetEnabledLangs()), Error: nonEmpty(p.GetError()), TaskId: task,
		IconUrl: s.packageIconURL(pluginID, p.GetId(), p.GetIconUrl()),
	}
	for _, o := range p.GetOffers() {
		out.Offers = append(out.Offers, apigen.PackageOffer{
			OriginId: o.GetOriginId(), OriginName: o.GetOriginName(), Version: o.GetVersion(), Supported: o.GetSupported(),
		})
	}
	for _, src := range p.GetSources() {
		out.Sources = append(out.Sources, apigen.PackageSource{
			Id: pluginID + ":" + src.GetId(), Name: src.GetName(), Lang: src.GetLang(), Available: src.GetAvailable(),
		})
	}
	return out
}

// pendingTask is the queued or running task of a package, if any.
func pendingTask(pending map[string]uuid.UUID, pkg string) *uuid.UUID {
	if id, ok := pending[pkg]; ok {
		return &id
	}
	return nil
}

// ListPackages lists what a plugin offers and has installed.
func (s *Server) ListPackages(ctx context.Context, req apigen.ListPackagesRequestObject) (apigen.ListPackagesResponseObject, error) {
	pr := req.Params
	list, err := s.deps.Plugins.Packages(ctx, req.PluginId, plugins.PackageQuery{
		Query: deref(pr.Q), Lang: deref(pr.Lang), OriginID: deref(pr.OriginId),
		InstalledOnly: deref(pr.Installed), UpdatesOnly: deref(pr.Updates),
	})
	if err != nil {
		return nil, packageError(ctx, err)
	}
	pending, err := s.deps.Plugins.PendingPackageTasks(ctx, req.PluginId)
	if err != nil {
		return nil, err
	}
	out := apigen.ListPackages200JSONResponse{Packages: make([]apigen.Package, len(list))}
	for i, p := range list {
		out.Packages[i] = s.toPackage(req.PluginId, p, pendingTask(pending, p.GetId()))
	}
	return out, nil
}

// GetPackage returns one of a plugin's packages.
func (s *Server) GetPackage(ctx context.Context, req apigen.GetPackageRequestObject) (apigen.GetPackageResponseObject, error) {
	p, err := s.deps.Plugins.Package(ctx, req.PluginId, req.PackageId)
	if err != nil {
		return nil, packageError(ctx, err)
	}
	pending, err := s.deps.Plugins.PendingPackageTasks(ctx, req.PluginId)
	if err != nil {
		return nil, err
	}
	return apigen.GetPackage200JSONResponse(s.toPackage(req.PluginId, p, pendingTask(pending, p.GetId()))), nil
}

func (s *Server) queuePackage(ctx context.Context, pluginID, pkg, action, origin string) (apigen.TaskRef, error) {
	t, err := s.deps.Plugins.QueuePackage(ctx, auth.FromContext(ctx).User.ID, plugins.PackagePayload{
		PluginID: pluginID, PackageID: pkg, Action: action, OriginID: origin,
	})
	if err != nil {
		return apigen.TaskRef{}, packageError(ctx, err)
	}
	return apigen.TaskRef{TaskId: t.ID}, nil
}

// InstallPackage queues installing a package.
func (s *Server) InstallPackage(ctx context.Context, req apigen.InstallPackageRequestObject) (apigen.InstallPackageResponseObject, error) {
	origin := ""
	if req.Body != nil {
		origin = deref(req.Body.OriginId)
	}
	ref, err := s.queuePackage(ctx, req.PluginId, req.PackageId, plugins.ActionInstall, origin)
	if err != nil {
		return nil, err
	}
	return apigen.InstallPackage202JSONResponse(ref), nil
}

// UpdatePackage queues updating a package.
func (s *Server) UpdatePackage(ctx context.Context, req apigen.UpdatePackageRequestObject) (apigen.UpdatePackageResponseObject, error) {
	ref, err := s.queuePackage(ctx, req.PluginId, req.PackageId, plugins.ActionUpdate, "")
	if err != nil {
		return nil, err
	}
	return apigen.UpdatePackage202JSONResponse(ref), nil
}

// UninstallPackage queues uninstalling a package.
func (s *Server) UninstallPackage(ctx context.Context, req apigen.UninstallPackageRequestObject) (apigen.UninstallPackageResponseObject, error) {
	ref, err := s.queuePackage(ctx, req.PluginId, req.PackageId, plugins.ActionUninstall, "")
	if err != nil {
		return nil, err
	}
	return apigen.UninstallPackage202JSONResponse(ref), nil
}

// SetPackageLanguages chooses a package's languages.
func (s *Server) SetPackageLanguages(ctx context.Context, req apigen.SetPackageLanguagesRequestObject) (apigen.SetPackageLanguagesResponseObject, error) {
	p, err := s.deps.Plugins.SetPackageLanguages(ctx, req.PluginId, req.PackageId, req.Body.EnabledLangs)
	if err != nil {
		return nil, packageError(ctx, err)
	}
	pending, err := s.deps.Plugins.PendingPackageTasks(ctx, req.PluginId)
	if err != nil {
		return nil, err
	}
	return apigen.SetPackageLanguages200JSONResponse(s.toPackage(req.PluginId, p, pendingTask(pending, p.GetId()))), nil
}

// packageIconURL is the server's signed URL for a package's icon.
func (s *Server) packageIconURL(pluginID, pkg, ref string) *string {
	if ref == "" {
		return nil
	}
	q := url.Values{"ref": {ref}, "sig": {s.deps.Signer.Sign("package-icon", pluginID, pkg, ref)}}
	u := BasePath + "/plugins/" + url.PathEscape(pluginID) + "/packages/" + url.PathEscape(pkg) + "/icon?" + q.Encode()
	return &u
}

// iconMaxAge is how long a package icon is kept before it is fetched again.
const iconMaxAge = 7 * 24 * time.Hour

// GetPackageIcon serves a package's icon through the server, cached.
func (s *Server) GetPackageIcon(ctx context.Context, req apigen.GetPackageIconRequestObject) (apigen.GetPackageIconResponseObject, error) {
	if !s.deps.Signer.Verify(req.Params.Sig, "package-icon", req.PluginId, req.PackageId, req.Params.Ref) {
		return nil, NewProblem(http.StatusForbidden, "bad_signature", "not an image link from this server")
	}
	key := "package-icon\x00" + req.PluginId + "\x00" + req.Params.Ref
	img, err := s.deps.Images.GetFresh(ctx, key, iconMaxAge, func(ctx context.Context) (string, io.Reader, error) {
		return s.deps.Plugins.Icon(ctx, req.PluginId, req.Params.Ref)
	})
	if errors.Is(err, imagecache.ErrNotImage) {
		return nil, NewProblem(http.StatusNotFound, CodeNotFound, "not a usable image")
	}
	if err != nil {
		return nil, s.imageError(ctx, err, true, "package", req.PluginId+"/"+req.PackageId)
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
	return apigen.GetPackageIcon200ImageResponse{
		Body: f, ContentType: img.ContentType, ContentLength: st.Size(),
		Headers: apigen.GetPackageIcon200ResponseHeaders{CacheControl: ptr("private, max-age=86400")},
	}, nil
}

// packageError is pluginError for package calls: a request the plugin
// refuses (e.g. a language the package doesn't have) is 422.
func packageError(ctx context.Context, err error) error {
	var pe *plugin.Error
	if errors.As(err, &pe) && pe.Code == plugin.CodeInvalidArgument && pe.Reason == "" {
		return NewProblem(http.StatusUnprocessableEntity, "package_rejected", pe.Message)
	}
	return pluginError(ctx, err)
}
