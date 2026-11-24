package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMemoryAndMultiBackend(t *testing.T) {
	ctx := context.Background()
	m1 := NewMemoryBackend("https://repo.svc.plus")
	m2 := NewMemoryBackend("https://cn-repo.svc.plus")
	multi, err := NewMultiBackend(m1, m2)
	if err != nil {
		t.Fatalf("NewMultiBackend: %v", err)
	}

	if err := multi.Put(ctx, "/apt/dists/stable/Release", bytes.NewBufferString("Suite: stable\n"), 14, PutOptions{
		ContentType:  "text/plain",
		CacheControl: "public, max-age=60",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, b := range []Backend{m1, m2, multi} {
		obj, err := b.Get(ctx, "apt/dists/stable/Release")
		if err != nil {
			t.Fatalf("Get from %s: %v", b.Name(), err)
		}
		data, _ := io.ReadAll(obj.Body)
		_ = obj.Body.Close()
		if string(data) != "Suite: stable\n" {
			t.Fatalf("unexpected data from %s: %q", b.Name(), string(data))
		}
	}

	if got := m1.PublicURL("files/app.dmg"); got != "https://repo.svc.plus/files/app.dmg" {
		t.Fatalf("unexpected PublicURL: %s", got)
	}
}

func TestLocalFSBackend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewLocalFSBackend(dir, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewLocalFSBackend: %v", err)
	}
	if err := fs.Put(ctx, "releases/xconnect/v1.0.0/app.dmg", bytes.NewBufferString("dmg-bytes"), 9, PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, err := fs.Head(ctx, "releases/xconnect/v1.0.0/app.dmg")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != 9 || meta.ContentType != "application/x-apple-diskimage" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	items, err := fs.List(ctx, "releases/xconnect")
	if err != nil || len(items) != 1 {
		t.Fatalf("List: len=%d err=%v", len(items), err)
	}
}

func TestS3BackendSigV4(t *testing.T) {
	var gotAuth string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	s3b, err := NewS3Backend(S3Config{
		Endpoint:       srv.URL,
		Bucket:         "svc-plus-repo-global",
		Region:         "auto",
		AccessKeyID:    "AKIA_TEST",
		SecretKey:      "SECRET_TEST",
		PublicBaseURL:  "https://repo.svc.plus",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	ctx := context.Background()
	if err := s3b.Put(ctx, "repo-index.json", bytes.NewBufferString(`{"ok":true}`), 11, PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("S3 Put: %v", err)
	}
	if gotMethod != http.MethodPut || gotAuth == "" || !bytes.Contains([]byte(gotAuth), []byte("AWS4-HMAC-SHA256")) {
		t.Fatalf("expected SigV4 header on PUT, got method=%s auth=%q", gotMethod, gotAuth)
	}
	obj, err := s3b.Get(ctx, "repo-index.json")
	if err != nil {
		t.Fatalf("S3 Get: %v", err)
	}
	defer obj.Body.Close()
	if obj.Meta.ContentType != "application/json" {
		t.Fatalf("unexpected content-type: %s", obj.Meta.ContentType)
	}
}
