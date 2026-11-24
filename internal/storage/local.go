//go:build !js

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalFSBackend stores objects on the local filesystem for single-binary / air-gapped deployments.
type LocalFSBackend struct {
	root          string
	publicBaseURL string
}

func NewLocalFSBackend(rootDir, publicBaseURL string) (*LocalFSBackend, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("local storage root directory is required")
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LocalFSBackend{
		root:          abs,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}, nil
}

func (l *LocalFSBackend) Name() string { return "local:" + l.root }

func (l *LocalFSBackend) resolve(key string) (string, error) {
	clean := CleanKey(key)
	if clean == "" {
		return "", ErrNotFound
	}
	full := filepath.Join(l.root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(l.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrNotFound
	}
	return full, nil
}

func (l *LocalFSBackend) Get(_ context.Context, key string) (*Object, error) {
	full, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, ErrNotFound
	}
	ct := detectContentType(key)
	return &Object{
		Meta: ObjectMeta{
			Key:          CleanKey(key),
			Size:         st.Size(),
			ContentType:  ct,
			ETag:         fileETag(st),
			LastModified: st.ModTime().UTC(),
		},
		Body: f,
	}, nil
}

func (l *LocalFSBackend) Head(_ context.Context, key string) (ObjectMeta, error) {
	full, err := l.resolve(key)
	if err != nil {
		return ObjectMeta{}, err
	}
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectMeta{}, ErrNotFound
		}
		return ObjectMeta{}, err
	}
	if st.IsDir() {
		return ObjectMeta{}, ErrNotFound
	}
	return ObjectMeta{
		Key:          CleanKey(key),
		Size:         st.Size(),
		ContentType:  detectContentType(key),
		ETag:         fileETag(st),
		LastModified: st.ModTime().UTC(),
	}, nil
}

func (l *LocalFSBackend) Put(_ context.Context, key string, body io.Reader, _ int64, _ PutOptions) error {
	full, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, full)
}

func (l *LocalFSBackend) Delete(_ context.Context, key string) error {
	full, err := l.resolve(key)
	if err != nil {
		return nil
	}
	err = os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (l *LocalFSBackend) List(_ context.Context, prefix string) ([]ObjectMeta, error) {
	cleanPrefix := CleanKey(prefix)
	var out []ObjectMeta
	err := filepath.Walk(l.root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return nil
		}
		k := filepath.ToSlash(rel)
		if cleanPrefix != "" && !strings.HasPrefix(k, cleanPrefix) {
			return nil
		}
		out = append(out, ObjectMeta{
			Key:          k,
			Size:         info.Size(),
			ContentType:  detectContentType(k),
			ETag:         fileETag(info),
			LastModified: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (l *LocalFSBackend) PublicURL(key string) string {
	if l.publicBaseURL == "" {
		return ""
	}
	return l.publicBaseURL + "/" + CleanKey(key)
}

func fileETag(info os.FileInfo) string {
	raw := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(raw))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func detectContentType(key string) string {
	low := strings.ToLower(key)
	switch {
	case strings.HasSuffix(low, ".json"):
		return "application/json"
	case strings.HasSuffix(low, ".yaml"), strings.HasSuffix(low, ".yml"):
		return "application/yaml"
	case strings.HasSuffix(low, ".xml"):
		return "application/xml"
	case strings.HasSuffix(low, ".gz"), strings.HasSuffix(low, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(low, ".deb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(low, ".rpm"):
		return "application/x-rpm"
	case strings.HasSuffix(low, ".dmg"):
		return "application/x-apple-diskimage"
	case strings.HasSuffix(low, ".msi"):
		return "application/x-msi"
	case strings.HasSuffix(low, ".sh"):
		return "text/x-shellscript; charset=utf-8"
	case strings.HasSuffix(low, ".ps1"):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(low, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(low, ".gpg"), strings.HasSuffix(low, ".asc"):
		return "application/pgp-keys"
	}
	if ext := filepath.Ext(key); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}
