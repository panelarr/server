// SPDX-License-Identifier: GPL-3.0-or-later

package plugins

import (
	"context"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"

	"github.com/panelarr/source-api/sdk/client"
	"github.com/panelarr/source-api/sdk/plugin"

	"github.com/panelarr/server/internal/metrics"
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

// SetImageRetries changes the default waits before each extra attempt at a
// page image (none: no retries).
func (h *Host) SetImageRetries(delays ...time.Duration) {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.imageRetries = slices.Clone(delays)
}

// SetMetrics makes the host count page image retries.
func (h *Host) SetMetrics(m *metrics.Metrics) { h.metrics = m }

// imageRetriesFor returns the waits before each extra attempt at a source's
// page images.
func (h *Host) imageRetriesFor(_ context.Context, _ string) []time.Duration {
	h.imu.RLock()
	defer h.imu.RUnlock()
	return h.imageRetries
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
