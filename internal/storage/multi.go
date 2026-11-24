package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// MultiBackend fans out writes/deletes across multiple storage targets (e.g. Cloudflare R2 + Aliyun OSS + Local FS)
// and reads from the primary target with automatic fallback.
type MultiBackend struct {
	targets []Backend
}

func NewMultiBackend(targets ...Backend) (*MultiBackend, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("multi backend requires at least one target")
	}
	return &MultiBackend{targets: targets}, nil
}

func (m *MultiBackend) Name() string {
	names := make([]string, len(m.targets))
	for i, t := range m.targets {
		names[i] = t.Name()
	}
	return "multi(" + strings.Join(names, ",") + ")"
}

func (m *MultiBackend) Get(ctx context.Context, key string) (*Object, error) {
	var lastErr error
	for _, t := range m.targets {
		obj, err := t.Get(ctx, key)
		if err == nil {
			return obj, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (m *MultiBackend) Head(ctx context.Context, key string) (ObjectMeta, error) {
	var lastErr error
	for _, t := range m.targets {
		meta, err := t.Head(ctx, key)
		if err == nil {
			return meta, nil
		}
		lastErr = err
	}
	return ObjectMeta{}, lastErr
}

func (m *MultiBackend) Put(ctx context.Context, key string, body io.Reader, _ int64, opts PutOptions) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	for _, t := range m.targets {
		if err := t.Put(ctx, key, bytes.NewReader(data), int64(len(data)), opts); err != nil {
			return fmt.Errorf("put %s to %s: %w", key, t.Name(), err)
		}
	}
	return nil
}

func (m *MultiBackend) Delete(ctx context.Context, key string) error {
	for _, t := range m.targets {
		if err := t.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete %s from %s: %w", key, t.Name(), err)
		}
	}
	return nil
}

func (m *MultiBackend) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	return m.targets[0].List(ctx, prefix)
}

func (m *MultiBackend) PublicURL(key string) string {
	return m.targets[0].PublicURL(key)
}
