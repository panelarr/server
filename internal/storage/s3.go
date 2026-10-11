// SPDX-License-Identifier: GPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is how to reach an S3-compatible bucket.
type S3Config struct {
	// Endpoint is the service's URL, e.g. "https://s3.example.com".
	Endpoint string `json:"endpoint"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// Prefix is prepended to every key, e.g. "panelarr/".
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key"`
	// PathStyle addresses the bucket in the path (most self-hosted
	// services need it).
	PathStyle bool `json:"path_style"`
}

// S3 stores files in an S3-compatible bucket. Object writes are atomic.
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// NewS3 returns an S3 backend; secret is the secret access key.
func NewS3(cfg S3Config, secret string) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("storage: endpoint must be http(s)://host[:port]")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("storage: bucket is required")
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, secret, ""),
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	return &S3{cfg: cfg, client: c}, nil
}

func (s *S3) object(key string) string {
	return path.Join(strings.Trim(s.cfg.Prefix, "/"), key)
}

// Put uploads a file.
func (s *S3) Put(ctx context.Context, key, src string) error {
	f, err := os.Open(src) //nolint:gosec // a file the server wrote
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.cfg.Bucket, s.object(key), f, st.Size(), minio.PutObjectOptions{ContentType: contentType(key)})
	return err
}

func contentType(key string) string {
	if strings.HasSuffix(strings.ToLower(key), ".cbz") {
		return "application/vnd.comicbook+zip"
	}
	return "application/octet-stream"
}

type s3File struct {
	*minio.Object
	size int64
}

func (f s3File) Size() int64 { return f.size }

// Open opens an object for ranged reads.
func (s *S3) Open(ctx context.Context, key string) (File, error) {
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, s.object(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s3File{Object: obj, size: st.Size}, nil
}

// Delete removes an object.
func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.cfg.Bucket, s.object(key), minio.RemoveObjectOptions{})
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return nil
	}
	return err
}

// Test checks the bucket exists and a probe can be written, read and
// deleted.
func (s *S3) Test(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("storage: reach the bucket: %w", err)
	}
	if !ok {
		return fmt.Errorf("storage: bucket %q doesn't exist", s.cfg.Bucket)
	}
	return probe(ctx, s, "")
}
