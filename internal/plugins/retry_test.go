// SPDX-License-Identifier: GPL-3.0-or-later

package plugins_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
	"github.com/panelarr/source-api/sdk/plugin"
	"github.com/panelarr/source-api/sdk/plugintest"

	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/plugins/pluginstest"
)

// flakyImages fails image fetches with the queued errors, then serves
// them; a nil entry breaks off a stream after its first bytes.
type flakyImages struct {
	*plugintest.Fake
	mu    sync.Mutex
	fails []error
	calls int
}

var errBreakOff = errors.New("break off")

func (f *flakyImages) FetchImage(_ context.Context, _ *sourcev1.FetchImageRequest, send plugin.ImageSendFunc) error {
	f.mu.Lock()
	f.calls++
	var err error
	if len(f.fails) > 0 {
		err, f.fails = f.fails[0], f.fails[1:]
	}
	f.mu.Unlock()
	if errors.Is(err, errBreakOff) {
		_ = send(&sourcev1.FetchImageResponse{Part: &sourcev1.FetchImageResponse_Metadata{Metadata: &sourcev1.ImageMetadata{ContentType: "image/png"}}})
		_ = send(&sourcev1.FetchImageResponse{Part: &sourcev1.FetchImageResponse_Chunk{Chunk: []byte("half")}})
		return plugin.ErrSourceDown("the connection dropped")
	}
	if err != nil {
		return err
	}
	return send(&sourcev1.FetchImageResponse{Part: &sourcev1.FetchImageResponse_Metadata{Metadata: &sourcev1.ImageMetadata{ContentType: "image/png"}}})
}

func (f *flakyImages) set(fails ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails, f.calls = fails, 0
}

func (f *flakyImages) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestPageImageRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := newHost(t)
	fake := &flakyImages{Fake: plugintest.New()}
	if _, err := h.Register(ctx, plugins.NewPlugin{URL: pluginstest.Serve(t, fake, key), APIKey: key}); err != nil {
		t.Fatal(err)
	}
	h.SetDefaultImageRetries(10*time.Millisecond, 30*time.Millisecond)
	waits := 0
	wait := func(context.Context) error { waits++; return nil }
	fetch := func() (time.Duration, error) {
		waits = 0
		start := time.Now()
		_, _, err := h.PageImage(ctx, admin(), "fake:1001", "page:x", wait)
		return time.Since(start), err
	}

	for _, tc := range []struct {
		name  string
		fails []error
		calls int
		ok    bool
	}{
		{"fails once, then works", []error{plugin.Errorf(plugin.CodeUnavailable, "down")}, 2, true},
		{"site down twice", []error{plugin.ErrSourceDown("down"), plugin.Errorf(plugin.CodeInternal, "502")}, 3, true},
		{"a stream that broke off", []error{errBreakOff}, 2, true},
		{"out of attempts", []error{plugin.ErrSourceDown("1"), plugin.ErrSourceDown("2"), plugin.ErrSourceDown("3")}, 3, false},
		{"not found", []error{plugin.ErrNotFound("gone")}, 1, false},
		{"rejected key", []error{plugin.Errorf(plugin.CodeUnauthenticated, "bad key")}, 1, false},
		{"challenge", []error{plugin.ErrChallenge("solve me")}, 1, false},
		{"login", []error{plugin.ErrLoginRequired("log in")}, 1, false},
	} {
		fake.set(tc.fails...)
		_, err := fetch()
		if (err == nil) != tc.ok || fake.count() != tc.calls || waits != tc.calls {
			t.Errorf("%s: err %v, %d calls, %d waits; want ok %v, %d calls", tc.name, err, fake.count(), waits, tc.ok, tc.calls)
		}
	}

	// The source's retry-after is waited out, up to a limit.
	fake.set(plugin.ErrRateLimited(150*time.Millisecond, "slow down"))
	if d, err := fetch(); err != nil || d < 150*time.Millisecond || fake.count() != 2 {
		t.Fatalf("retry-after: %v after %s, %d calls", err, d, fake.count())
	}
	fake.set(plugin.ErrRateLimited(plugins.MaxImageRetryAfter+time.Second, "come back tomorrow"))
	var pe *plugin.Error
	if d, err := fetch(); !errors.As(err, &pe) || pe.Reason != plugin.ReasonRateLimited || d > time.Second || fake.count() != 1 {
		t.Fatalf("long retry-after: %v after %s, %d calls", err, d, fake.count())
	}

	// The pace of the source's images applies to every attempt.
	fake.set(plugin.ErrSourceDown("down"))
	paced := errors.New("over budget")
	n := 0
	if _, _, err := h.PageImage(ctx, admin(), "fake:1001", "page:x", func(context.Context) error {
		if n++; n > 1 {
			return paced
		}
		return nil
	}); !errors.Is(err, paced) || fake.count() != 1 {
		t.Fatalf("pacing: %v, %d calls", err, fake.count())
	}

	// A package's setting applies to its sources; a source's own wins.
	if _, err := h.SetPackageImageRetries(ctx, "fake", "example.comics", nil); err != nil {
		t.Fatal(err)
	}
	fake.set(plugin.ErrSourceDown("down"), plugin.ErrSourceDown("down"))
	if _, err := fetch(); err == nil || fake.count() != 1 {
		t.Fatalf("package off: %v, %d calls", err, fake.count())
	}
	if _, err := h.SetSourceImageRetries(ctx, "fake:1001", []time.Duration{time.Millisecond, time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	fake.set(plugin.ErrSourceDown("down"), plugin.ErrSourceDown("down"))
	if _, err := fetch(); err != nil || fake.count() != 3 {
		t.Fatalf("source override: %v, %d calls", err, fake.count())
	}
	if err := h.ClearSourceImageRetries(ctx, "fake:1001"); err != nil {
		t.Fatal(err)
	}
	fake.set(plugin.ErrSourceDown("down"))
	if _, err := fetch(); err == nil || fake.count() != 1 {
		t.Fatalf("back to the package: %v, %d calls", err, fake.count())
	}
	if err := h.ClearPackageImageRetries(ctx, "fake", "example.comics"); err != nil {
		t.Fatal(err)
	}

	// No retries when they're off; a request that goes away stops waiting.
	h.SetDefaultImageRetries()
	fake.set(plugin.ErrSourceDown("down"))
	if _, err := fetch(); err == nil || fake.count() != 1 {
		t.Fatalf("off: %v, %d calls", err, fake.count())
	}
	h.SetDefaultImageRetries(time.Minute)
	fake.set(plugin.ErrSourceDown("down"))
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := h.PageImage(cctx, admin(), "fake:1001", "page:x", nil); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled: %v after %s", err, time.Since(start))
	}
}
