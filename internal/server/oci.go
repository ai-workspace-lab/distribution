package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// handleOCI implements OCI Distribution Spec v2 for both Docker images and Helm OCI charts.
// Dual-mode:
//   1) Standalone R2/OSS/Local OCI hosting (`oci/v2/<name>/manifests/<ref>` & `oci/v2/<name>/blobs/<digest>`)
//   2) Upstream Harbor / GHCR / Docker Hub pull-through proxy with CDN 302 Blob redirect
func (h *Handler) handleOCI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")

	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		h.writeJSON(w, http.StatusOK, map[string]any{}, "no-cache")
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "read-only edge OCI endpoint (use distd sync for pushing)", http.StatusMethodNotAllowed)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if strings.HasSuffix(rest, "/tags/list") {
		repo := strings.TrimSuffix(rest, "/tags/list")
		h.handleOCITagsList(w, r, repo)
		return
	}

	if idx := strings.LastIndex(rest, "/manifests/"); idx > 0 {
		repo := rest[:idx]
		ref := rest[idx+len("/manifests/"):]
		h.handleOCIManifest(w, r, repo, ref)
		return
	}

	if idx := strings.LastIndex(rest, "/blobs/"); idx > 0 {
		repo := rest[:idx]
		digest := rest[idx+len("/blobs/"):]
		h.handleOCIBlob(w, r, repo, digest)
		return
	}

	http.NotFound(w, r)
}

func (h *Handler) handleOCITagsList(w http.ResponseWriter, r *http.Request, repo string) {
	prefix := "oci/v2/" + storage.CleanKey(repo) + "/manifests/"
	items, err := h.store.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var tags []string
	for _, it := range items {
		tag := strings.TrimPrefix(it.Key, prefix)
		if tag == "" || strings.Contains(tag, "/") || strings.HasPrefix(tag, "sha256:") || strings.HasSuffix(tag, ".meta.json") {
			continue
		}
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	h.writeJSON(w, http.StatusOK, map[string]any{
		"name": repo,
		"tags": tags,
	}, "public, max-age=60")
}

func (h *Handler) handleOCIManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	key := "oci/v2/" + storage.CleanKey(repo) + "/manifests/" + storage.CleanKey(ref)
	obj, err := h.store.Get(r.Context(), key)
	if err == nil {
		defer obj.Body.Close()
		data, readErr := io.ReadAll(obj.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadGateway)
			return
		}
		ct := obj.Meta.ContentType
		if ct == "" || ct == "application/octet-stream" {
			ct = detectOCIManifestMediaType(data)
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Cache-Control", CacheControlForPath(r.URL.Path))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
		return
	}
	if !errors.Is(err, storage.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if !h.cfg.Upstreams.OCI.PullThroughEnabled {
		h.writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found in distribution store")
		return
	}

	registryHost, upstreamRepo, ok := h.resolveOCIUpstream(repo)
	if !ok {
		h.writeOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown repository and no upstream configured")
		return
	}
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registryHost, upstreamRepo, ref)
	accept := r.Header.Get("Accept")
	if accept == "" {
		accept = strings.Join([]string{
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
			"application/vnd.docker.distribution.manifest.v2+json",
		}, ", ")
	}
	resp, err := h.doOCIUpstreamRequest(r.Context(), r.Method, upstreamURL, accept)
	if err != nil {
		h.writeOCIError(w, http.StatusBadGateway, "UNAVAILABLE", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.writeOCIError(w, resp.StatusCode, "MANIFEST_UNKNOWN", fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		return
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeOCIError(w, http.StatusBadGateway, "UNAVAILABLE", err.Error())
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = detectOCIManifestMediaType(data)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(data)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if r.Method == http.MethodGet && len(data) > 0 {
		_ = h.store.Put(r.Context(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
			ContentType:  ct,
			CacheControl: CacheControlForPath(r.URL.Path),
		})
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Cache-Control", CacheControlForPath(r.URL.Path))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (h *Handler) handleOCIBlob(w http.ResponseWriter, r *http.Request, repo, digest string) {
	// Blobs are content-addressed globally by digest under oci/blobs/<digest> (deduplicated across repos),
	// with fallback to repo-scoped oci/v2/<repo>/blobs/<digest>.
	candidates := []string{
		"oci/blobs/" + storage.CleanKey(digest),
		"oci/v2/" + storage.CleanKey(repo) + "/blobs/" + storage.CleanKey(digest),
	}
	for _, key := range candidates {
		if _, err := h.store.Head(r.Context(), key); err == nil {
			if h.cfg.CDNRedirect {
				if pub := h.ociBlobRedirectURL(key); pub != "" && !strings.HasPrefix(pub, h.publicBaseURL(r)+"/") {
					w.Header().Set("Docker-Content-Digest", digest)
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					http.Redirect(w, r, pub, http.StatusFound)
					return
				}
			}
			w.Header().Set("Docker-Content-Digest", digest)
			h.serveObject(w, r, key, false)
			return
		}
	}

	if !h.cfg.Upstreams.OCI.PullThroughEnabled {
		h.writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found in distribution store")
		return
	}

	registryHost, upstreamRepo, ok := h.resolveOCIUpstream(repo)
	if !ok {
		h.writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "unknown repository and no upstream configured")
		return
	}
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registryHost, upstreamRepo, digest)
	resp, err := h.doOCIUpstreamRequest(r.Context(), r.Method, upstreamURL, "*/*")
	if err != nil {
		h.writeOCIError(w, http.StatusBadGateway, "UNAVAILABLE", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
	}
	if resp.StatusCode != http.StatusOK {
		h.writeOCIError(w, resp.StatusCode, "BLOB_UNKNOWN", fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (h *Handler) ociBlobRedirectURL(key string) string {
	if base := strings.TrimRight(h.cfg.Upstreams.OCI.BlobRedirectBase, "/"); base != "" {
		return base + "/" + storage.CleanKey(key)
	}
	return h.store.PublicURL(key)
}

func (h *Handler) resolveOCIUpstream(repo string) (host, upstreamRepo string, ok bool) {
	parts := strings.SplitN(strings.Trim(repo, "/"), "/", 2)
	if len(parts) == 2 {
		if aliasHost, exists := h.cfg.Upstreams.OCI.RegistryAliases[parts[0]]; exists && aliasHost != "" {
			r := parts[1]
			if aliasHost == "registry-1.docker.io" && !strings.Contains(r, "/") {
				r = "library/" + r
			}
			return strings.TrimPrefix(strings.TrimPrefix(aliasHost, "https://"), "http://"), r, true
		}
	}
	if h.cfg.Upstreams.OCI.HarborURL != "" {
		host := strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(h.cfg.Upstreams.OCI.HarborURL, "https://"), "http://"), "/")
		return host, strings.Trim(repo, "/"), true
	}
	return "", "", false
}

var wwwAuthParamRE = regexp.MustCompile(`([a-zA-Z0-9_]+)="([^"]*)"`)

// doOCIUpstreamRequest performs an OCI v2 request and handles Bearer WWW-Authenticate challenges.
func (h *Handler) doOCIUpstreamRequest(ctx context.Context, method, targetURL, accept string) (*http.Response, error) {
	noRedirectClient := *h.client
	noRedirectClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	authHeader := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()

	token, err := h.fetchOCIBearerToken(ctx, authHeader)
	if err != nil || token == "" {
		return nil, fmt.Errorf("oci auth failed: %w", err)
	}
	req2, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req2.Header.Set("Accept", accept)
	}
	req2.Header.Set("Authorization", "Bearer "+token)
	return noRedirectClient.Do(req2)
}

func (h *Handler) fetchOCIBearerToken(ctx context.Context, wwwAuth string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(wwwAuth), "bearer ") {
		return "", fmt.Errorf("unsupported auth challenge: %s", wwwAuth)
	}
	params := map[string]string{}
	for _, m := range wwwAuthParamRE.FindAllStringSubmatch(wwwAuth, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("missing bearer realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if params["scope"] != "" {
		q.Set("scope", params["scope"])
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	return payload.AccessToken, nil
}

func (h *Handler) writeOCIError(w http.ResponseWriter, status int, code, msg string) {
	h.writeJSON(w, status, map[string]any{
		"errors": []map[string]string{
			{"code": code, "message": msg},
		},
	}, "no-cache")
}

func detectOCIManifestMediaType(data []byte) string {
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []any  `json:"manifests"`
	}
	if err := json.Unmarshal(data, &m); err == nil {
		if m.MediaType != "" {
			return m.MediaType
		}
		if len(m.Manifests) > 0 {
			return "application/vnd.oci.image.index.v1+json"
		}
	}
	return "application/vnd.oci.image.manifest.v1+json"
}
