// SPDX-License-Identifier: GPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local stores files under a directory.
type Local struct {
	Root string
}

func (l *Local) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if key == "" || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("storage: bad key %q", key)
	}
	return filepath.Join(l.Root, clean), nil
}

// Put moves (or copies, across filesystems) the file into place through a
// temporary name in the target directory, then renames it.
func (l *Local) Put(_ context.Context, key, src string) error {
	dst, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	in, err := os.Open(src) //nolint:gosec // a file the server wrote
	if err != nil {
		_ = tmp.Close()
		return err
	}
	_, err = io.Copy(tmp, in)
	_ = in.Close()
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

type localFile struct {
	*os.File
	size int64
}

func (f localFile) Size() int64 { return f.size }

// Open opens a file.
func (l *Local) Open(_ context.Context, key string) (File, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // inside the storage root
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return localFile{File: f, size: st.Size()}, nil
}

// Delete removes a file and then empty parent directories up to the root.
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(p); dir != filepath.Clean(l.Root) && strings.HasPrefix(dir, filepath.Clean(l.Root)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break // not empty
		}
	}
	return nil
}

// Test writes, reads and deletes a probe file.
func (l *Local) Test(ctx context.Context) error {
	if err := os.MkdirAll(l.Root, 0o750); err != nil {
		return fmt.Errorf("storage: create %s: %w", l.Root, err)
	}
	return probe(ctx, l, l.Root)
}

// probe stores a small file, reads it back and deletes it.
func probe(ctx context.Context, s Storage, tmpDir string) error {
	f, err := os.CreateTemp(tmpDir, ".probe-src-*")
	if err != nil {
		f, err = os.CreateTemp("", ".panelarr-probe-*")
		if err != nil {
			return err
		}
	}
	defer func() { _ = os.Remove(f.Name()) }()
	want := "panelarr storage probe"
	if _, err := f.WriteString(want); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	key := ".panelarr-probe"
	if err := s.Put(ctx, key, f.Name()); err != nil {
		return fmt.Errorf("storage: write: %w", err)
	}
	r, err := s.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("storage: read: %w", err)
	}
	buf := make([]byte, len(want))
	_, err = r.ReadAt(buf, 0)
	_ = r.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("storage: read: %w", err)
	}
	if string(buf) != want {
		return errors.New("storage: read back different bytes")
	}
	if err := s.Delete(ctx, key); err != nil {
		return fmt.Errorf("storage: delete: %w", err)
	}
	return nil
}
