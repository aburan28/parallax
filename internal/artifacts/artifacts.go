/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package artifacts stores provider snapshots, run reports, readback details and
// report bundles in a content-addressed object store; the results DB keeps only
// URIs + digests (DESIGN.md §15). Content addressing makes writes idempotent, so
// re-runs never corrupt existing objects.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ObjectStore is the storage abstraction. Put streams r to storage, returning a
// retrieval uri, the content digest, and the number of bytes written. Get opens
// a previously stored object by its uri.
type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader) (uri, digest string, n int64, err error)
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
}

// FSStore is a filesystem-backed ObjectStore that content-addresses objects by
// SHA-256 under baseDir. It is the default for --local mode.
type FSStore struct {
	baseDir string
}

var _ ObjectStore = (*FSStore)(nil)

// NewFSStore returns a filesystem object store rooted at baseDir (created if
// absent).
func NewFSStore(baseDir string) (*FSStore, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, fmt.Errorf("artifacts: baseDir must not be empty")
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve baseDir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: create baseDir %q: %w", abs, err)
	}
	return &FSStore{baseDir: abs}, nil
}

// Put streams r to a temp file while hashing, then atomically renames it to its
// content-addressed path (baseDir/blobs/sha256/<aa>/<hash>). key is a logical
// name for the caller's records; the physical location is derived purely from
// the content, so identical content is deduplicated.
func (s *FSStore) Put(ctx context.Context, key string, r io.Reader) (uri, digest string, n int64, err error) {
	_ = key // content-addressed: the location comes from the digest, not the key
	if err := ctx.Err(); err != nil {
		return "", "", 0, fmt.Errorf("artifacts: put cancelled: %w", err)
	}

	tmp, err := os.CreateTemp(s.baseDir, ".upload-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("artifacts: create temp: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", "", 0, fmt.Errorf("artifacts: write object: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("artifacts: sync object: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("artifacts: close object: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	digest = "sha256:" + sum

	dir := filepath.Join(s.baseDir, "blobs", "sha256", sum[:2])
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", 0, fmt.Errorf("artifacts: create blob dir: %w", err)
	}
	dst := filepath.Join(dir, sum)

	if _, statErr := os.Stat(dst); statErr == nil {
		// Identical content already stored: dedupe, drop the temp file.
		committed = true // handled below; nothing to rename
		_ = os.Remove(tmpName)
		return "file://" + dst, digest, n, nil
	}

	if err = os.Rename(tmpName, dst); err != nil {
		return "", "", 0, fmt.Errorf("artifacts: commit object: %w", err)
	}
	committed = true
	return "file://" + dst, digest, n, nil
}

// Get opens an object previously stored by Put. Only file:// URIs are supported
// in M0.
func (s *FSStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: get cancelled: %w", err)
	}
	const scheme = "file://"
	if !strings.HasPrefix(uri, scheme) {
		if strings.HasPrefix(uri, "s3://") {
			return nil, fmt.Errorf("artifacts: s3 backend not implemented") // TODO(m1)
		}
		return nil, fmt.Errorf("artifacts: unsupported uri scheme: %q", uri)
	}
	path := strings.TrimPrefix(uri, scheme)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open %q: %w", path, err)
	}
	return f, nil
}

// S3Config configures an S3/MinIO/GCS-compatible object store.
type S3Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
}

// NewS3Store will return an S3/MinIO-backed ObjectStore.
//
// TODO(m1): implement against a pure-Go S3 client (content-addressed keys,
// multipart upload, digest verification on Get). Returns an error in M0 so
// callers fail loudly rather than silently dropping artifacts.
func NewS3Store(cfg S3Config) (ObjectStore, error) {
	_ = cfg
	return nil, fmt.Errorf("artifacts: S3/MinIO object store not implemented yet")
}
