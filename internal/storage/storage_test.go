// SPDX-License-Identifier: GPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// FakeS3 runs an in-memory S3 service with one bucket and returns a
// backend for it.
func fakeS3(t *testing.T) *S3 {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("library"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "library", Prefix: "panelarr/", AccessKey: "key", PathStyle: true, Region: "us-east-1"}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exercise(t *testing.T, s Storage) {
	t.Helper()
	ctx := context.Background()
	if err := s.Test(ctx); err != nil {
		t.Fatalf("test: %v", err)
	}
	src := filepath.Join(t.TempDir(), "x.cbz")
	if err := os.WriteFile(src, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := "Some Work [abc]/Ch.0001.cbz"
	if err := s.Put(ctx, key, src); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := f.ReadAt(buf, 4); err != nil || string(buf) != "456" || f.Size() != 10 {
		t.Fatalf("ranged read %q %v size %d", buf, err, f.Size())
	}
	_ = f.Close()
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted file: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestLocal(t *testing.T) {
	root := t.TempDir()
	exercise(t, &Local{Root: root})
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
	l := &Local{Root: root}
	for _, bad := range []string{"", "../x", "/etc/passwd", ".."} {
		if _, err := l.path(bad); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

func TestS3(t *testing.T) {
	exercise(t, fakeS3(t))
	if _, err := NewS3(S3Config{Endpoint: "s3.example.com", Bucket: "b"}, ""); err == nil {
		t.Fatal("endpoint without scheme accepted")
	}
	missing := fakeS3(t)
	missing.cfg.Bucket = "nope"
	if err := missing.Test(context.Background()); err == nil {
		t.Fatal("missing bucket passed")
	}
}
