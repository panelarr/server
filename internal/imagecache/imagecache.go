// SPDX-License-Identifier: GPL-3.0-or-later

// Package imagecache fetches remote images (covers) once and keeps them on
// disk, so browsers never fetch from metadata or source sites themselves:
// that would leak users' addresses and referrers, and many sites refuse
// hotlinking anyway.
package imagecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// MaxBytes caps one image.
const MaxBytes = 15 << 20

// ErrNotImage is a response that isn't an image.
var ErrNotImage = errors.New("imagecache: not an image")

// Cache is a directory of fetched images, keyed by a hash of their URL.
// It is rebuildable: deleting the directory only costs refetches.
type Cache struct {
	dir   string
	hc    *http.Client
	group singleflight.Group
}

// New returns a cache in dir.
func New(dir string) *Cache {
	return &Cache{dir: dir, hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}}
}

// SetHTTPClient replaces the HTTP client (tests).
func (c *Cache) SetHTTPClient(hc *http.Client) { c.hc = hc }

// Image is a cached image.
type Image struct {
	Path        string
	ContentType string
	ModTime     time.Time
}

func (c *Cache) paths(rawURL string) (data, meta string) {
	sum := sha256.Sum256([]byte(rawURL))
	key := hex.EncodeToString(sum[:])
	base := filepath.Join(c.dir, key[:2], key)
	return base, base + ".type"
}

// Get returns the image at rawURL, fetching it on first use. Concurrent
// requests for the same URL share one fetch.
func (c *Cache) Get(ctx context.Context, rawURL string) (Image, error) {
	data, meta := c.paths(rawURL)
	if img, ok := c.cached(data, meta); ok {
		return img, nil
	}
	v, err, _ := c.group.Do(data, func() (any, error) {
		if img, ok := c.cached(data, meta); ok {
			return img, nil
		}
		// The fetch outlives a cancelled caller: others may wait for it.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return c.fetch(fctx, rawURL, data, meta)
	})
	if err != nil {
		return Image{}, err
	}
	return v.(Image), nil
}

func (c *Cache) cached(data, meta string) (Image, bool) {
	st, err := os.Stat(data)
	if err != nil {
		return Image{}, false
	}
	ct, err := os.ReadFile(meta) //nolint:gosec // path built from a hash
	if err != nil {
		return Image{}, false
	}
	return Image{Path: data, ContentType: string(ct), ModTime: st.ModTime()}, true
}

func (c *Cache) fetch(ctx context.Context, rawURL, data, meta string) (Image, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Image{}, fmt.Errorf("imagecache: unsupported URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Image{}, err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.8")
	res, err := c.hc.Do(req)
	if err != nil {
		return Image{}, fmt.Errorf("imagecache: fetch: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Image{}, fmt.Errorf("imagecache: fetch: status %d", res.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
		// SVG can carry scripts; it is never served from our origin.
		return Image{}, ErrNotImage
	}
	if err := os.MkdirAll(filepath.Dir(data), 0o750); err != nil {
		return Image{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(data), ".tmp-*")
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(res.Body, MaxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Image{}, fmt.Errorf("imagecache: fetch: %w", err)
	}
	if n > MaxBytes {
		return Image{}, fmt.Errorf("imagecache: image larger than %d bytes", MaxBytes)
	}
	if err := os.WriteFile(meta, []byte(ct), 0o600); err != nil {
		return Image{}, err
	}
	if err := os.Rename(tmp.Name(), data); err != nil {
		return Image{}, err
	}
	st, err := os.Stat(data)
	if err != nil {
		return Image{}, err
	}
	return Image{Path: data, ContentType: ct, ModTime: st.ModTime()}, nil
}
