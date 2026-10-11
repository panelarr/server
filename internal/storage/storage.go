// SPDX-License-Identifier: GPL-3.0-or-later

// Package storage stores downloaded chapter files on configurable
// backends: the local filesystem (which includes mounted network shares)
// and S3-compatible object storage. Files are addressed by key (a
// slash-separated path); every backend writes atomically and reads with
// random access, so the reader can open archives without downloading
// them whole.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is a key with no file.
var ErrNotFound = errors.New("storage: not found")

// File is an open stored file with random access.
type File interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

// Storage is one storage backend.
type Storage interface {
	// Put stores the file at path under key, replacing nothing until it is
	// complete: readers see the old file or the new one, never a partial
	// one.
	Put(ctx context.Context, key, path string) error
	// Open opens a stored file for random access.
	Open(ctx context.Context, key string) (File, error)
	// Delete removes a file; a missing file is not an error.
	Delete(ctx context.Context, key string) error
	// Test checks the backend can write, read and delete.
	Test(ctx context.Context) error
}
