// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/client"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/store/db"
)

// Plugins that host many sites (like an extension runtime) install
// packages that provide sources: the package capability. The host lists
// what they offer, installs, updates and uninstalls packages as tasks, and
// syncs the plugin right after each change so sources appear or go absent
// within seconds.

// ErrNoPackages is a plugin without the package capability.
var ErrNoPackages = errors.New("plugins: the plugin doesn't install packages")

// KindPackage is the task kind of installing, updating and uninstalling a
// package.
const KindPackage = "plugin.package"

// Package actions.
const (
	ActionInstall   = "install"
	ActionUpdate    = "update"
	ActionUninstall = "uninstall"
)

// packageTimeout bounds one install or update: plugins answer when done,
// which can take a couple of minutes.
const packageTimeout = 10 * time.Minute

// PackagePayload is a package task's payload.
type PackagePayload struct {
	PluginID  string `json:"plugin_id"`
	PackageID string `json:"package_id"`
	Action    string `json:"action"`
	OriginID  string `json:"origin_id,omitempty"`
}

// PackageQuery narrows a plugin's package list.
type PackageQuery struct {
	Query, Lang, OriginID      string
	InstalledOnly, UpdatesOnly bool
}

// HasPackages reports whether a plugin declares the package capability.
func HasPackages(info *sourcev1.PluginInfo) bool {
	return slices.Contains(info.GetCapabilities(), sourcev1.PluginCapability_PLUGIN_CAPABILITY_PACKAGES)
}

// packagesClient is the client of an enabled plugin with the package
// capability.
func (h *Host) packagesClient(ctx context.Context, id string) (client.Client, error) {
	p, err := h.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, ErrDisabled
	}
	if !HasPackages(p.Info) {
		return nil, ErrNoPackages
	}
	return h.Client(ctx, id)
}

// Packages lists what a plugin offers and has installed, all pages.
func (h *Host) Packages(ctx context.Context, id string, q PackageQuery) ([]*sourcev1.Package, error) {
	c, err := h.packagesClient(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req := &sourcev1.ListPackagesRequest{
		Query: q.Query, Lang: q.Lang, OriginId: q.OriginID, InstalledOnly: q.InstalledOnly, UpdatesOnly: q.UpdatesOnly,
	}
	var out []*sourcev1.Package
	for range 1000 {
		res, err := c.ListPackages(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, res.GetPackages()...)
		if res.GetNextPageToken() == "" || res.GetNextPageToken() == req.PageToken {
			return out, nil
		}
		req.PageToken = res.GetNextPageToken()
	}
	return out, nil
}

// Package returns one of a plugin's packages.
func (h *Host) Package(ctx context.Context, id, pkg string) (*sourcev1.Package, error) {
	c, err := h.packagesClient(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.GetPackage(ctx, &sourcev1.GetPackageRequest{PackageId: pkg})
	if err != nil {
		return nil, err
	}
	return res.GetPackage(), nil
}

// SetPackageLanguages sets which of a package's languages its sources are
// offered in (empty: every language the plugin allows), then syncs the
// plugin so its sources follow.
func (h *Host) SetPackageLanguages(ctx context.Context, id, pkg string, langs []string) (*sourcev1.Package, error) {
	c, err := h.packagesClient(ctx, id)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.SetPackageSettings(cctx, &sourcev1.SetPackageSettingsRequest{
		PackageId: pkg, Settings: &sourcev1.PackageSettings{EnabledLangs: langs},
	})
	if err != nil {
		return nil, err
	}
	h.log.InfoContext(ctx, "package languages changed", "component", "audit", "plugin", id, "package", pkg, "langs", langs)
	h.packageChanged(ctx, id)
	return res.GetPackage(), nil
}

// packageChanged syncs a plugin now and a little later, after one of its
// packages changed.
func (h *Host) packageChanged(ctx context.Context, id string) {
	if _, err := h.Sync(ctx, id); err != nil {
		h.log.WarnContext(ctx, "sync after a package change", "plugin", id, "err", err)
	}
	h.scheduleFollowUp(id)
}

// RegisterPackageTasks lets q run package tasks, one at a time.
func (h *Host) RegisterPackageTasks(q *queue.Queue) {
	h.queue = q
	q.Register(KindPackage, queue.Kind{Handler: h.handlePackage, Concurrency: 1, MaxAttempts: 3})
}

// QueuePackage queues installing, updating or uninstalling a package. The
// same action on the same package is queued once; a plugin without the
// capability or turned off, or a package it doesn't know, is refused up
// front.
func (h *Host) QueuePackage(ctx context.Context, by uuid.UUID, p PackagePayload) (db.Task, error) {
	if h.queue == nil {
		return db.Task{}, errors.New("plugins: package tasks aren't registered")
	}
	// An unknown package is refused now rather than as a failed task.
	if _, err := h.Package(ctx, p.PluginID, p.PackageID); err != nil {
		return db.Task{}, err
	}
	t, _, err := h.queue.Enqueue(ctx, queue.NewTask{
		Kind: KindPackage, Payload: p, CreatedBy: by, Priority: 10,
		DedupeKey: "package:" + p.PluginID + ":" + p.PackageID + ":" + p.Action,
	})
	return t, err
}

// PendingPackageTasks maps a plugin's packages to their queued or running
// task.
func (h *Host) PendingPackageTasks(ctx context.Context, id string) (map[string]uuid.UUID, error) {
	rows, err := h.st.ActiveTasksOf(ctx, db.ActiveTasksOfParams{Kind: KindPackage, Field: "plugin_id", Value: id})
	if err != nil {
		return nil, err
	}
	out := map[string]uuid.UUID{}
	for _, r := range rows {
		t := queue.Task{Task: r}
		var p PackagePayload
		if t.Decode(&p) == nil && p.PackageID != "" {
			if _, ok := out[p.PackageID]; !ok {
				out[p.PackageID] = r.ID
			}
		}
	}
	return out, nil
}

func (h *Host) handlePackage(ctx context.Context, t *queue.Task) (any, error) {
	var p PackagePayload
	if err := t.Decode(&p); err != nil {
		return nil, queue.Permanent(err)
	}
	c, err := h.packagesClient(ctx, p.PluginID)
	if err != nil {
		return nil, queue.Permanent(err)
	}
	t.SetProgress(queue.Progress{Phase: p.Action})
	cctx, cancel := context.WithTimeout(ctx, packageTimeout)
	defer cancel()
	var pkg *sourcev1.Package
	switch p.Action {
	case ActionInstall:
		var res *sourcev1.InstallPackageResponse
		if res, err = c.InstallPackage(cctx, &sourcev1.InstallPackageRequest{PackageId: p.PackageID, OriginId: p.OriginID}); err == nil {
			pkg = res.GetPackage()
		}
	case ActionUpdate:
		var res *sourcev1.UpdatePackageResponse
		if res, err = c.UpdatePackage(cctx, &sourcev1.UpdatePackageRequest{PackageId: p.PackageID}); err == nil {
			pkg = res.GetPackage()
		}
	case ActionUninstall:
		_, err = c.UninstallPackage(cctx, &sourcev1.UninstallPackageRequest{PackageId: p.PackageID})
	default:
		return nil, queue.Permanent(fmt.Errorf("unknown package action %q", p.Action))
	}
	if err != nil {
		// The task's error keeps the plugin's reason (e.g.
		// STORE_FETCH_FAILED: …) for clients to tell failures apart.
		msg := errorMessage(err)
		var pe *plugin.Error
		if errors.As(err, &pe) {
			if pe.Reason != "" {
				msg = string(pe.Reason) + ": " + msg
			}
			switch pe.Code {
			case plugin.CodeNotFound, plugin.CodeInvalidArgument, plugin.CodeFailedPrecondition, plugin.CodeUnimplemented:
				// Unknown package, bad signature or checksum, a broken
				// store: trying again won't help. A store that couldn't
				// be reached (unavailable) is tried again.
				return nil, queue.Permanent(errors.New(msg))
			}
		}
		return nil, errors.New(msg)
	}
	h.log.InfoContext(ctx, "package changed", "component", "audit", "action", p.Action, "plugin", p.PluginID, "package", p.PackageID,
		"version", pkg.GetInstalledVersion())
	h.packageChanged(ctx, p.PluginID)
	out := map[string]string{"plugin_id": p.PluginID, "package_id": p.PackageID, "action": p.Action}
	if v := pkg.GetInstalledVersion(); v != "" {
		out["version"] = v
	}
	return out, nil
}

// installedPackages is what a plugin's installed packages say about its
// sources: their package names, and which sources they provide in a
// language turned off. ok is false when the plugin has no packages or
// didn't answer.
type installedPackages struct {
	ok          bool
	names       map[string]string
	languageOff []string
}

func (h *Host) installedPackages(ctx context.Context, c client.Client, info *sourcev1.PluginInfo) installedPackages {
	out := installedPackages{names: map[string]string{}, languageOff: []string{}}
	if !HasPackages(info) {
		return out
	}
	res, err := c.ListPackages(ctx, &sourcev1.ListPackagesRequest{InstalledOnly: true})
	if err != nil {
		h.log.DebugContext(ctx, "list installed packages", "err", err)
		return out
	}
	out.ok = true
	for _, p := range res.GetPackages() {
		out.names[p.GetId()] = p.GetName()
		for _, src := range p.GetSources() {
			if !src.GetAvailable() {
				out.languageOff = append(out.languageOff, src.GetId())
			}
		}
	}
	return out
}

// PackageIcon fetches a package's own icon. ErrNotFound when it has none.
func (h *Host) PackageIcon(ctx context.Context, id, pkg string) (string, io.Reader, error) {
	p, err := h.Package(ctx, id, pkg)
	if err != nil {
		var pe *plugin.Error
		if errors.As(err, &pe) && pe.Code == plugin.CodeNotFound {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	if p.GetIconUrl() == "" {
		return "", nil, ErrNotFound
	}
	return h.Icon(ctx, id, p.GetIconUrl())
}

// MaxIconBytes caps an icon (of a package or a source).
const MaxIconBytes = 2 << 20

// iconClient fetches icons.
var iconClient = &http.Client{Timeout: callTimeout}

// Icon fetches a package's or a source's icon from ref, its icon URL as
// the plugin gave it: a path relative to the plugin's address (served by
// the plugin over HTTP with the API's credentials), or an absolute URL.
// The plugin's key goes only to the plugin's own address. Icons never go
// through the image RPC, which is for covers and pages.
func (h *Host) Icon(ctx context.Context, id, ref string) (string, io.Reader, error) {
	p, err := h.st.GetPlugin(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	if !p.Enabled {
		return "", nil, ErrDisabled
	}
	base, err := url.Parse(p.Url)
	if err != nil {
		return "", nil, err
	}
	rel, err := url.Parse(ref)
	if err != nil {
		return "", nil, ErrNotFound
	}
	u := base.ResolveReference(rel)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil, ErrNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.8")
	if u.Scheme == base.Scheme && u.Host == base.Host {
		key, err := h.open(p.ApiKey)
		if err != nil {
			return "", nil, err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	res, err := iconClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("plugins: icon: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
		// E.g. a plugin hiding sources in a language turned off.
		return "", nil, fmt.Errorf("%w: no icon there", ErrNotFound)
	}
	if res.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("plugins: icon: status %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxIconBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("plugins: icon: %w", err)
	}
	if len(data) > MaxIconBytes {
		return "", nil, fmt.Errorf("plugins: icon larger than %d bytes", MaxIconBytes)
	}
	return res.Header.Get("Content-Type"), bytes.NewReader(data), nil
}
