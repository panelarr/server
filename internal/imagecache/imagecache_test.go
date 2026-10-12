// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestGetFresh(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir())
	ctx := context.Background()
	calls := 0
	body := []byte("png-1")
	fail := false
	fetch := func(context.Context) (string, io.Reader, error) {
		calls++
		if fail {
			return "", nil, errors.New("down")
		}
		return "image/png", bytes.NewReader(body), nil
	}
	read := func(img Image) string {
		b, err := os.ReadFile(img.Path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	img, err := c.GetFresh(ctx, "k", time.Hour, fetch)
	if err != nil || read(img) != "png-1" || calls != 1 {
		t.Fatalf("first %v %q %d", err, read(img), calls)
	}
	if _, err := c.GetFresh(ctx, "k", time.Hour, fetch); err != nil || calls != 1 {
		t.Fatalf("fresh refetched: %v %d", err, calls)
	}

	// Older than maxAge: fetched again.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(img.Path, old, old); err != nil {
		t.Fatal(err)
	}
	body = []byte("png-2")
	img, err = c.GetFresh(ctx, "k", time.Hour, fetch)
	if err != nil || read(img) != "png-2" || calls != 2 {
		t.Fatalf("stale %v %q %d", err, read(img), calls)
	}

	// A failed refresh keeps the old image.
	if err := os.Chtimes(img.Path, old, old); err != nil {
		t.Fatal(err)
	}
	fail = true
	img, err = c.GetFresh(ctx, "k", time.Hour, fetch)
	if err != nil || read(img) != "png-2" || calls != 3 {
		t.Fatalf("failed refresh %v %d", err, calls)
	}
	// Nothing cached and the fetch fails: the error.
	if _, err := c.GetFresh(ctx, "other", time.Hour, fetch); err == nil {
		t.Fatal("no error without an image")
	}

	// SVG is never stored.
	fail = false
	svg := func(context.Context) (string, io.Reader, error) {
		return "image/svg+xml", bytes.NewReader([]byte("<svg/>")), nil
	}
	if _, err := c.GetFresh(ctx, "svg", time.Hour, svg); !errors.Is(err, ErrNotImage) {
		t.Fatalf("svg %v", err)
	}
}
