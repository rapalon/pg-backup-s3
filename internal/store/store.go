// Package store isolates the object store behind a small interface with a
// real S3 implementation (aws-sdk-go-v2) and an in-memory fake for tests.
//
// Keys are full object keys inside one bucket; prefix handling belongs to the
// callers (internal/manifest builds keys, this package stores them).
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotFound is returned by Get and Stat for a missing key.
	ErrNotFound = errors.New("object not found")
	// ErrBucketNotFound is returned by Ping when the bucket does not exist.
	// The bucket is never created by this program: it is configuration.
	ErrBucketNotFound = errors.New("bucket not found")
)

// ObjectStore is everything the backup, restore and retention code needs.
type ObjectStore interface {
	// Put streams r into key. The object becomes visible only when Put returns
	// nil; an error from r or from the store leaves no object behind.
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error)
	// Get opens key for streaming reads. The caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	// Stat returns size, modification time and user metadata of key.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List returns every object under prefix (recursive), sorted by key.
	// Metadata is not populated by List.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Delete removes keys; missing keys are not an error.
	Delete(ctx context.Context, keys []string) error
}

// PutOptions tune one upload.
type PutOptions struct {
	ContentType string
	// Metadata is stored as user metadata (x-amz-meta-*). Keys are lowercased
	// by S3; values must be printable ASCII.
	Metadata map[string]string
	// PartSize overrides the store's multipart part size for this upload.
	PartSize int64
}

// PutResult describes a completed upload.
type PutResult struct {
	Size int64  // bytes read from the reader and stored
	ETag string // as reported by the store, quotes stripped; may be empty
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	Metadata     map[string]string
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
