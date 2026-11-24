//go:build js && wasm

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/syumai/workers-go/cloudflare/r2"
)

// R2WorkerBackend binds directly to a Cloudflare Workers R2 bucket binding (zero network egress).
type R2WorkerBackend struct {
	bucket        *r2.Bucket
	bindingName   string
	publicBaseURL string
}

func NewR2WorkerBackend(bindingName, publicBaseURL string) (*R2WorkerBackend, error) {
	b, err := r2.NewBucket(bindingName)
	if err != nil {
		return nil, fmt.Errorf("bind R2 bucket %s: %w", bindingName, err)
	}
	return &R2WorkerBackend{
		bucket:        b,
		bindingName:   bindingName,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}, nil
}

func (r *R2WorkerBackend) Name() string { return "cf-r2:" + r.bindingName }

func (r *R2WorkerBackend) Get(_ context.Context, key string) (*Object, error) {
	k := CleanKey(key)
	obj, err := r.bucket.Get(k)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, ErrNotFound
	}
	ct := obj.HTTPMetadata.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	rc, ok := obj.Body.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(obj.Body)
	}
	return &Object{
		Meta: ObjectMeta{
			Key:          k,
			Size:         int64(obj.Size),
			ContentType:  ct,
			CacheControl: obj.HTTPMetadata.CacheControl,
			ETag:         obj.HTTPETag,
			LastModified: obj.Uploaded,
		},
		Body: rc,
	}, nil
}

func (r *R2WorkerBackend) Head(_ context.Context, key string) (ObjectMeta, error) {
	k := CleanKey(key)
	obj, err := r.bucket.Head(k)
	if err != nil {
		return ObjectMeta{}, err
	}
	if obj == nil {
		return ObjectMeta{}, ErrNotFound
	}
	ct := obj.HTTPMetadata.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ObjectMeta{
		Key:          k,
		Size:         int64(obj.Size),
		ContentType:  ct,
		CacheControl: obj.HTTPMetadata.CacheControl,
		ETag:         obj.HTTPETag,
		LastModified: obj.Uploaded,
	}, nil
}

func (r *R2WorkerBackend) Put(_ context.Context, key string, body io.Reader, _ int64, opts PutOptions) error {
	k := CleanKey(key)
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	_, err = r.bucket.Put(k, io.NopCloser(bytes.NewReader(data)), &r2.PutOptions{
		HTTPMetadata: r2.HTTPMetadata{
			ContentType:  opts.ContentType,
			CacheControl: opts.CacheControl,
		},
	})
	return err
}

func (r *R2WorkerBackend) Delete(_ context.Context, key string) error {
	return r.bucket.Delete(CleanKey(key))
}

func (r *R2WorkerBackend) List(_ context.Context, prefix string) ([]ObjectMeta, error) {
	res, err := r.bucket.List()
	if err != nil {
		return nil, err
	}
	p := CleanKey(prefix)
	out := make([]ObjectMeta, 0, len(res.Objects))
	for _, o := range res.Objects {
		ck := CleanKey(o.Key)
		if p != "" && !strings.HasPrefix(ck, p) {
			continue
		}
		out = append(out, ObjectMeta{
			Key:          ck,
			Size:         int64(o.Size),
			ContentType:  o.HTTPMetadata.ContentType,
			CacheControl: o.HTTPMetadata.CacheControl,
			ETag:         o.HTTPETag,
			LastModified: o.Uploaded,
		})
	}
	return out, nil
}

func (r *R2WorkerBackend) PublicURL(key string) string {
	if r.publicBaseURL == "" {
		return ""
	}
	return r.publicBaseURL + "/" + CleanKey(key)
}
