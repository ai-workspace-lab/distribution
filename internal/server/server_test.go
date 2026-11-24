package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

func TestUnifiedServerAllProtocols(t *testing.T) {
	ctx := context.Background()

	// Mock upstream npm registry
	npmUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello-pkg":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"hello-pkg","versions":{"1.0.0":{"dist":{"tarball":"` + "http://" + r.Host + `/hello-pkg/-/hello-pkg-1.0.0.tgz"}}}}`))
		case "/hello-pkg/-/hello-pkg-1.0.0.tgz":
			_, _ = w.Write([]byte("fake-tgz-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer npmUpstream.Close()

	store := storage.NewMemoryBackend("https://cdn.example.com")
	cfg := config.DefaultConfig()
	cfg.MirrorName = "global"
	cfg.PublicURL = "https://repo.svc.plus"
	cfg.CDNRedirect = true
	cfg.Upstreams.NPM.UpstreamRegistry = npmUpstream.URL
	cfg.Upstreams.NPM.PullThrough = true

	// Seed files, helm, OCI standalone, WinGet REST index
	_ = store.Put(ctx, "files/tool-v1.0.0.tar.gz", bytes.NewBufferString("tarball"), 7, storage.PutOptions{})
	_ = store.Put(ctx, "helm/index.yaml", bytes.NewBufferString("apiVersion: v1\nentries: {}\n"), 27, storage.PutOptions{ContentType: "application/yaml"})
	_ = store.Put(ctx, "helm/charts/demo-0.1.0.tgz", bytes.NewBufferString("chart-tgz"), 9, storage.PutOptions{ContentType: "application/gzip"})

	blobBytes := []byte("oci-layer-content")
	blobSum := sha256.Sum256(blobBytes)
	blobDigest := "sha256:" + hex.EncodeToString(blobSum[:])
	_ = store.Put(ctx, "oci/blobs/"+blobDigest, bytes.NewReader(blobBytes), int64(len(blobBytes)), storage.PutOptions{})
	manifestBytes := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"digest":"` + blobDigest + `"}]}`)
	_ = store.Put(ctx, "oci/v2/svc-plus/app/manifests/v1.0.0", bytes.NewReader(manifestBytes), int64(len(manifestBytes)), storage.PutOptions{ContentType: "application/vnd.oci.image.manifest.v1+json"})

	wingetIdx := WinGetIndex{
		SourceIdentifier: "SvcPlus.Distribution",
		Packages: []WinGetPackageRecord{
			{
				PackageIdentifier: "SvcPlus.XConnect",
				PackageName:       "XConnect",
				Publisher:         "svc.plus",
				Moniker:           "xconnect",
				Versions: []WinGetVersionManifest{
					{
						PackageVersion: "1.1.9",
						DefaultLocale: WinGetLocaleMetadata{
							PackageLocale:    "en-US",
							Publisher:        "svc.plus",
							PackageName:      "XConnect",
							License:          "Apache-2.0",
							ShortDescription: "Cross-platform client",
						},
						Installers: []WinGetInstaller{
							{
								Architecture:    "x64",
								InstallerType:   "msi",
								InstallerUrl:    "https://dl.svc.plus/releases/xconnect/v1.1.9/xconnect-windows.msi",
								CNInstallerUrl:  "https://cn-dl.svc.plus/releases/xconnect/v1.1.9/xconnect-windows.msi",
								InstallerSha256: strings.Repeat("A", 64),
							},
						},
					},
				},
			},
		},
	}
	wgBytes, _ := json.Marshal(wingetIdx)
	_ = store.Put(ctx, "winget/api/index.json", bytes.NewReader(wgBytes), int64(len(wgBytes)), storage.PutOptions{ContentType: "application/json"})

	h := NewHandler(cfg, store, nil)

	// 1. Health & probe
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"distribution-v2-go"`) {
		t.Fatalf("/healthz unexpected: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Files immutable CDN 302 redirect
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/tool-v1.0.0.tar.gz", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://cdn.example.com/files/tool-v1.0.0.tar.gz" {
		t.Fatalf("/files redirect unexpected: %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	// 3. npm pull-through + tarball URL rewrite
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/npm/hello-pkg", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://repo.svc.plus/npm/hello-pkg/-/hello-pkg-1.0.0.tgz") {
		t.Fatalf("/npm packument rewrite failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/npm/hello-pkg/-/hello-pkg-1.0.0.tgz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "fake-tgz-bytes" {
		t.Fatalf("/npm tarball pull-through failed: %d %q", rec.Code, rec.Body.String())
	}

	// 4. OCI v2 standalone manifest & 302 CDN Blob redirect + tags/list
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/svc-plus/app/manifests/v1.0.0", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Docker-Content-Digest") == "" {
		t.Fatalf("OCI manifest failed: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/svc-plus/app/blobs/"+blobDigest, nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://cdn.example.com/oci/blobs/"+blobDigest {
		t.Fatalf("OCI blob redirect failed: %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/svc-plus/app/tags/list", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"v1.0.0"`) {
		t.Fatalf("OCI tags/list failed: %d %s", rec.Code, rec.Body.String())
	}

	// 5. Helm classic index.yaml & chart CDN redirect
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/helm/index.yaml", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatalf("Helm index.yaml failed: %d cc=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/helm/charts/demo-0.1.0.tgz", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("Helm chart redirect expected 302, got %d", rec.Code)
	}

	// 6. WinGet REST Source API (/information, /manifestSearch, /packageManifests/{id})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/winget/api/information", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SvcPlus.Distribution") {
		t.Fatalf("WinGet /information failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/winget/api/manifestSearch", strings.NewReader(`{"Query":{"KeyWord":"xconnect"}}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SvcPlus.XConnect") {
		t.Fatalf("WinGet /manifestSearch failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/winget/api/packageManifests/SvcPlus.XConnect", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://dl.svc.plus/releases/xconnect/v1.1.9/xconnect-windows.msi") {
		t.Fatalf("WinGet /packageManifests failed: %d %s", rec.Code, rec.Body.String())
	}
}
