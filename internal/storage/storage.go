package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("storage: object not found")
)

// ObjectMeta describes metadata for a stored object.
type ObjectMeta struct {
	Key          string
	Size         int64
	ContentType  string
	CacheControl string
	ETag         string
	LastModified time.Time
}

// Object is a readable stream plus metadata.
type Object struct {
	Meta ObjectMeta
	Body io.ReadCloser
}

// PutOptions controls content headers when writing objects.
type PutOptions struct {
	ContentType  string
	CacheControl string
}

// Backend abstracts Cloudflare R2, Aliyun OSS, generic S3, Local FS, and in-memory stores.
type Backend interface {
	Name() string
	Get(ctx context.Context, key string) (*Object, error)
	Head(ctx context.Context, key string) (ObjectMeta, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	// PublicURL returns a CDN/public URL for 302 redirecting large immutable blobs, or "" if not configured.
	PublicURL(key string) string
}

// CleanKey normalizes object keys to slash-delimited relative paths without leading slashes or path traversal.
func CleanKey(raw string) string {
	raw = strings.ReplaceAll(raw, "\\", "/")
	parts := strings.Split(raw, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "/")
}
