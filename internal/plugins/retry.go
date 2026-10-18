// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/panelarr/source-api/sdk/client"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/store/db"
)

// Some sites fail a page image now and then. Page images (the reader's and
// downloads') are tried again a few times before the error goes out, for
// errors that may pass: the server holds the whole image before it
// answers, so even a stream that broke off part way is safe to fetch again.

// DefaultImageRetries are the waits before each extra attempt at a page
// image: two more attempts, after 1 s and then 3 s.
var DefaultImageRetries = []time.Duration{time.Second, 3 * time.Second}

// MaxImageRetryAfter is the longest retry-after a page image waits out; a
// source asking for longer fails at once with its rate-limit error (the
// reader's user is waiting; downloads try again later as a task).
const MaxImageRetryAfter = 30 * time.Second

// SetDefaultImageRetries changes the default waits before each extra
// attempt at a page image (none: no retries), for sources without a
// setting of their own or their package's.
func (h *Host) SetDefaultImageRetries(delays ...time.Duration) {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.imageRetries = slices.Clone(delays)
	h.retryCache = nil
}

// SetMetrics makes the host count page image retries.
func (h *Host) SetMetrics(m *metrics.Metrics) { h.metrics = m }

// Levels a source's image retries come from.
const (
	RetryLevelSource  = "source"
	RetryLevelPackage = "package"
	RetryLevelDefault = "default"
)

// Image retry setting limits.
const (
	MaxImageRetries     = 5
	MaxImageRetryDelay  = 30 * time.Second
	imageRetryCacheLife = time.Minute
)

// ErrBadImageRetries is a setting out of bounds.
var ErrBadImageRetries = errors.New("at most 5 waits, each from 0 to 30000 ms")

// ImageRetrySetting is the setting at one level.
type ImageRetrySetting struct {
	Delays    []time.Duration
	UpdatedAt time.Time
}

// ImageRetries is what a package or source uses for its page images.
type ImageRetries struct {
	// TargetKind is "package" or "source"; TargetID "plugin:package" or
	// the source's ID.
	TargetKind, TargetID string
	// Own is the setting at this level; nil when there is none (it
	// inherits).
	Own *ImageRetrySetting
	// Effective is what applies; Level where it comes from, and FromID
	// the target there (empty for the default).
	Effective []time.Duration
	Level     string
	FromID    string
}

type cachedRetries struct {
	delays []time.Duration
	at     time.Time
}

func delaysOf(ms []int32) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, d := range ms {
		out[i] = time.Duration(d) * time.Millisecond
	}
	return out
}

func settingOf(r db.ImageRetrySetting) *ImageRetrySetting {
	return &ImageRetrySetting{Delays: delaysOf(r.DelaysMs), UpdatedAt: r.UpdatedAt}
}

func (h *Host) defaultRetries() []time.Duration {
	h.imu.RLock()
	defer h.imu.RUnlock()
	return slices.Clone(h.imageRetries)
}

// PackageKey is a package's target ID, "plugin:package".
func PackageKey(pluginID, pkg string) string { return pluginID + ":" + pkg }

// sourceRetries resolves a source's image retries: its own setting, then
// its package's, then the default.
func (h *Host) sourceRetries(ctx context.Context, sourceID string) (ImageRetries, error) {
	row, err := h.st.GetSource(ctx, sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImageRetries{}, ErrNotFound
	}
	if err != nil {
		return ImageRetries{}, err
	}
	out := ImageRetries{TargetKind: RetryLevelSource, TargetID: sourceID, Effective: h.defaultRetries(), Level: RetryLevelDefault}
	pkgKey := ""
	if row.Source.PackageID != "" {
		pkgKey = PackageKey(row.Source.PluginID, row.Source.PackageID)
	}
	rows, err := h.st.ImageRetrySettingsFor(ctx, db.ImageRetrySettingsForParams{SourceID: sourceID, PackageKey: pkgKey})
	if err != nil {
		return ImageRetries{}, err
	}
	for _, r := range rows {
		if r.TargetKind == RetryLevelPackage && out.Level == RetryLevelDefault {
			out.Effective, out.Level, out.FromID = delaysOf(r.DelaysMs), RetryLevelPackage, r.TargetID
		}
	}
	for _, r := range rows {
		if r.TargetKind == RetryLevelSource {
			out.Own = settingOf(r)
			out.Effective, out.Level, out.FromID = out.Own.Delays, RetryLevelSource, r.TargetID
		}
	}
	return out, nil
}

// imageRetriesFor returns the waits before each extra attempt at a source's
// page images, kept for a minute (and dropped on every change).
func (h *Host) imageRetriesFor(ctx context.Context, sourceID string) []time.Duration {
	h.imu.RLock()
	c, ok := h.retryCache[sourceID]
	h.imu.RUnlock()
	if ok && time.Since(c.at) < imageRetryCacheLife {
		return c.delays
	}
	r, err := h.sourceRetries(ctx, sourceID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			h.log.WarnContext(ctx, "image retry setting", "source", sourceID, "err", err)
		}
		return h.defaultRetries()
	}
	h.imu.Lock()
	if h.retryCache == nil {
		h.retryCache = map[string]cachedRetries{}
	}
	h.retryCache[sourceID] = cachedRetries{delays: r.Effective, at: time.Now()}
	h.imu.Unlock()
	return r.Effective
}

func (h *Host) retriesChanged() {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.retryCache = nil
}

// SourceImageRetries returns what a source uses for its page images.
func (h *Host) SourceImageRetries(ctx context.Context, sourceID string) (ImageRetries, error) {
	return h.sourceRetries(ctx, sourceID)
}

// checkPackage returns the target ID of a plugin's package.
func (h *Host) checkPackage(ctx context.Context, pluginID, pkg string) (string, error) {
	if !plugin.PackageIDPattern.MatchString(pkg) {
		return "", ErrNotFound
	}
	if _, err := h.st.GetPlugin(ctx, pluginID); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	return PackageKey(pluginID, pkg), nil
}

// PackageImageRetries returns what a package's sources use for their page
// images (unless one has its own setting).
func (h *Host) PackageImageRetries(ctx context.Context, pluginID, pkg string) (ImageRetries, error) {
	key, err := h.checkPackage(ctx, pluginID, pkg)
	if err != nil {
		return ImageRetries{}, err
	}
	out := ImageRetries{TargetKind: RetryLevelPackage, TargetID: key, Effective: h.defaultRetries(), Level: RetryLevelDefault}
	r, err := h.st.GetImageRetrySetting(ctx, db.GetImageRetrySettingParams{TargetKind: RetryLevelPackage, TargetID: key})
	switch {
	case err == nil:
		out.Own = settingOf(r)
		out.Effective, out.Level, out.FromID = out.Own.Delays, RetryLevelPackage, key
	case !errors.Is(err, pgx.ErrNoRows):
		return ImageRetries{}, err
	}
	return out, nil
}

func checkDelays(delays []time.Duration) ([]int32, error) {
	if len(delays) > MaxImageRetries {
		return nil, ErrBadImageRetries
	}
	ms := make([]int32, len(delays))
	for i, d := range delays {
		if d < 0 || d > MaxImageRetryDelay {
			return nil, ErrBadImageRetries
		}
		ms[i] = int32(d.Milliseconds()) //nolint:gosec // bounded above
	}
	return ms, nil
}

func (h *Host) putRetries(ctx context.Context, kind, id, pluginID string, delays []time.Duration) error {
	ms, err := checkDelays(delays)
	if err != nil {
		return err
	}
	if _, err := h.st.PutImageRetrySetting(ctx, db.PutImageRetrySettingParams{TargetKind: kind, TargetID: id, DelaysMs: ms, PluginID: pluginID}); err != nil {
		return err
	}
	h.retriesChanged()
	h.log.InfoContext(ctx, "image retries set", "component", "audit", "target_kind", kind, "target", id, "attempts", len(delays))
	return nil
}

func (h *Host) clearRetries(ctx context.Context, kind, id string) error {
	n, err := h.st.DeleteImageRetrySetting(ctx, db.DeleteImageRetrySettingParams{TargetKind: kind, TargetID: id})
	if err != nil {
		return err
	}
	h.retriesChanged()
	if n > 0 {
		h.log.InfoContext(ctx, "image retries cleared", "component", "audit", "target_kind", kind, "target", id)
	}
	return nil
}

// SetSourceImageRetries sets a source's own waits before each extra
// attempt at its page images (empty: no retries for it).
func (h *Host) SetSourceImageRetries(ctx context.Context, sourceID string, delays []time.Duration) (ImageRetries, error) {
	row, err := h.st.GetSource(ctx, sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImageRetries{}, ErrNotFound
	}
	if err != nil {
		return ImageRetries{}, err
	}
	if err := h.putRetries(ctx, RetryLevelSource, sourceID, row.Source.PluginID, delays); err != nil {
		return ImageRetries{}, err
	}
	return h.sourceRetries(ctx, sourceID)
}

// ClearSourceImageRetries removes a source's own setting: it inherits its
// package's or the default.
func (h *Host) ClearSourceImageRetries(ctx context.Context, sourceID string) error {
	if _, err := h.st.GetSource(ctx, sourceID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return h.clearRetries(ctx, RetryLevelSource, sourceID)
}

// SetPackageImageRetries sets the waits for a package's sources (empty: no
// retries for them).
func (h *Host) SetPackageImageRetries(ctx context.Context, pluginID, pkg string, delays []time.Duration) (ImageRetries, error) {
	key, err := h.checkPackage(ctx, pluginID, pkg)
	if err != nil {
		return ImageRetries{}, err
	}
	if err := h.putRetries(ctx, RetryLevelPackage, key, pluginID, delays); err != nil {
		return ImageRetries{}, err
	}
	return h.PackageImageRetries(ctx, pluginID, pkg)
}

// ClearPackageImageRetries removes a package's setting: its sources use the
// default (or their own).
func (h *Host) ClearPackageImageRetries(ctx context.Context, pluginID, pkg string) error {
	key, err := h.checkPackage(ctx, pluginID, pkg)
	if err != nil {
		return err
	}
	return h.clearRetries(ctx, RetryLevelPackage, key)
}

// retryable says whether a failed image fetch may pass if tried again, and
// how long the source asked to wait first.
func retryable(err error) (bool, time.Duration) {
	if errors.Is(err, client.ErrImageTooLarge) {
		return false, 0
	}
	var pe *plugin.Error
	if errors.As(err, &pe) {
		switch pe.Reason {
		case plugin.ReasonChallenge, plugin.ReasonNotFound, plugin.ReasonLoginRequired, plugin.ReasonUnsupported:
			// The plugin's solver handles challenges; the rest won't pass.
			return false, 0
		case plugin.ReasonRateLimited, plugin.ReasonSourceDown:
			return true, pe.RetryAfter
		}
		return retryableCode(pe.Code), pe.RetryAfter
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return retryableCode(ce.Code()), 0
	}
	// A call that ran out of time (the request itself is still on).
	return errors.Is(err, context.DeadlineExceeded), 0
}

// errorClass names an error's status and reason, e.g. "unavailable" or
// "resource_exhausted/RATE_LIMITED".
func errorClass(err error) string {
	var pe *plugin.Error
	if errors.As(err, &pe) {
		if pe.Reason != "" {
			return pe.Code.String() + "/" + string(pe.Reason)
		}
		return pe.Code.String()
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code().String()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	return "other"
}

// retryableCode is a status worth another try: the plugin or the site was
// unavailable, timed out, broke off or failed on its side.
func retryableCode(c connect.Code) bool {
	switch c {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeAborted, connect.CodeInternal,
		connect.CodeUnknown, connect.CodeResourceExhausted:
		return true
	}
	return false
}

// PageImage fetches a page image like [Host.Image], trying again after
// errors that may pass (see [DefaultImageRetries]). wait, when set, paces
// every attempt (the source's image limits), so retries spend the same
// budget as first tries.
func (h *Host) PageImage(ctx context.Context, v Viewer, id, ref string, wait func(context.Context) error) (string, []byte, error) {
	delays := h.imageRetriesFor(ctx, id)
	for attempt := 0; ; attempt++ {
		if wait != nil {
			if err := wait(ctx); err != nil {
				return "", nil, err
			}
		}
		ct, data, err := h.Image(ctx, v, id, ref)
		if err == nil {
			if attempt > 0 {
				h.metrics.ImageRetry("recovered")
			}
			return ct, data, nil
		}
		if ctx.Err() != nil {
			return "", nil, err
		}
		ok, after := retryable(err)
		if !ok || after > MaxImageRetryAfter || attempt >= len(delays) {
			if attempt == 0 {
				h.metrics.ImageRetry("not_retried")
			} else {
				h.metrics.ImageRetry("exhausted")
			}
			return "", nil, err
		}
		d := max(delays[attempt], after)
		// The error's text may carry the site's URLs: only its class.
		h.log.DebugContext(ctx, "page image failed, trying again", "source", id, "attempt", attempt+1, "in", d, "class", errorClass(err))
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", nil, err
		case <-t.C:
		}
	}
}
