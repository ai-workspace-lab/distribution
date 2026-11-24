package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryBackend is a thread-safe in-memory storage backend (WASM-safe, ideal for unit tests and dev).
type MemoryBackend struct {
	mu            sync.RWMutex
	objects       map[string]*memEntry
	publicBaseURL string
}

type memEntry struct {
	meta ObjectMeta
	data []byte
}

func NewMemoryBackend(publicBaseURL string) *MemoryBackend {
	return &MemoryBackend{
		objects:       make(map[string]*memEntry),
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}
}

func (m *MemoryBackend) Name() string { return "memory" }

func (m *MemoryBackend) Get(_ context.Context, key string) (*Object, error) {
	k := CleanKey(key)
	m.mu.RLock()
	e, ok := m.objects[k]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(e.data))
	copy(cp, e.data)
	return &Object{
		Meta: e.meta,
		Body: io.NopCloser(bytes.NewReader(cp)),
	}, nil
}

func (m *MemoryBackend) Head(_ context.Context, key string) (ObjectMeta, error) {
	k := CleanKey(key)
	m.mu.RLock()
	e, ok := m.objects[k]
	m.mu.RUnlock()
	if !ok {
		return ObjectMeta{}, ErrNotFound
	}
	return e.meta, nil
}

func (m *MemoryBackend) Put(_ context.Context, key string, body io.Reader, _ int64, opts PutOptions) error {
	k := CleanKey(key)
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	m.mu.Lock()
	m.objects[k] = &memEntry{
		meta: ObjectMeta{
			Key:          k,
			Size:         int64(len(data)),
			ContentType:  ct,
			CacheControl: opts.CacheControl,
			ETag:         `"` + hex.EncodeToString(sum[:16]) + `"`,
			LastModified: time.Now().UTC(),
		},
		data: data,
	}
	m.mu.Unlock()
	return nil
}

func (m *MemoryBackend) Delete(_ context.Context, key string) error {
	k := CleanKey(key)
	m.mu.Lock()
	delete(m.objects, k)
	m.mu.Unlock()
	return nil
}

func (m *MemoryBackend) List(_ context.Context, prefix string) ([]ObjectMeta, error) {
	p := CleanKey(prefix)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ObjectMeta
	for k, e := range m.objects {
		if p == "" || strings.HasPrefix(k, p) {
			out = append(out, e.meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemoryBackend) PublicURL(key string) string {
	if m.publicBaseURL == "" {
		return ""
	}
	return m.publicBaseURL + "/" + CleanKey(key)
}
