//go:build !js

package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// OCISyncer is a pure-Go OCI Distribution Spec v2 syncer (zero Docker daemon required).
// Supports syncing Docker images & Helm OCI charts from GHCR / Docker Hub / Harbor into:
//   1) Standalone R2 / Aliyun OSS / Local FS OCI blob+manifest layout
//   2) Upstream Harbor / OCI Registry (pushing missing blobs & manifests)
type OCISyncer struct {
	Store     storage.Backend
	OCIConfig config.OCIUpstreamConfig
	Client    *http.Client
}

func NewOCISyncer(store storage.Backend, ociCfg config.OCIUpstreamConfig, client *http.Client) *OCISyncer {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &OCISyncer{
		Store:     store,
		OCIConfig: ociCfg,
		Client:    client,
	}
}

type ociManifestDoc struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        *struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"config,omitempty"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers,omitempty"`
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"manifests,omitempty"`
}

// ParseOCIImageRef splits "ghcr.io/org/repo" -> ("ghcr.io", "org/repo") and "nginx" -> ("registry-1.docker.io", "library/nginx").
func ParseOCIImageRef(ref string) (host, repo string) {
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "https://"), "http://")
	ref = strings.TrimPrefix(ref, "oci://")
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		return "registry-1.docker.io", "library/" + parts[0]
	}
	if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost" {
		return parts[0], parts[1]
	}
	return "registry-1.docker.io", ref
}

// SyncImage mirrors an image (or multi-arch manifest list / Helm OCI chart) for a given tag/digest.
func (o *OCISyncer) SyncImage(ctx context.Context, mirror config.OCIImageMirror, tag string) (int, error) {
	srcHost, srcRepo := ParseOCIImageRef(mirror.Source)
	targetRepo := mirror.Target
	if targetRepo == "" {
		targetRepo = srcRepo
	}
	storeInR2 := mirror.StoreInR2 || o.OCIConfig.Mode == "standalone" || o.OCIConfig.Mode == "hybrid" || o.OCIConfig.Mode == ""
	pushHarbor := (mirror.SyncToHarbor || o.OCIConfig.Mode == "harbor" || o.OCIConfig.Mode == "hybrid") && o.OCIConfig.HarborURL != ""

	return o.syncManifestRecursive(ctx, srcHost, srcRepo, targetRepo, tag, storeInR2, pushHarbor)
}

func (o *OCISyncer) syncManifestRecursive(ctx context.Context, srcHost, srcRepo, targetRepo, ref string, storeInR2, pushHarbor bool) (int, error) {
	accept := strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", ")
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", srcHost, srcRepo, ref)
	if strings.HasPrefix(srcHost, "http://") {
		manifestURL = fmt.Sprintf("%s/v2/%s/manifests/%s", srcHost, srcRepo, ref)
	}
	resp, err := o.doRegistryRequest(ctx, http.MethodGet, manifestURL, accept, nil, "", "", "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch OCI manifest %s: HTTP %d", manifestURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/vnd.oci.image.manifest.v1+json"
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var doc ociManifestDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("parse OCI manifest %s: %w", manifestURL, err)
	}

	blobsSynced := 0
	// Multi-arch OCI Index / Docker Manifest List
	for _, child := range doc.Manifests {
		n, err := o.syncManifestRecursive(ctx, srcHost, srcRepo, targetRepo, child.Digest, storeInR2, pushHarbor)
		if err != nil {
			return blobsSynced, err
		}
		blobsSynced += n
	}

	// Single-arch config & layers (or Helm OCI config + chart tarball layer)
	var digests []string
	if doc.Config != nil && doc.Config.Digest != "" {
		digests = append(digests, doc.Config.Digest)
	}
	for _, layer := range doc.Layers {
		if layer.Digest != "" {
			digests = append(digests, layer.Digest)
		}
	}

	for _, dg := range digests {
		synced, err := o.syncBlob(ctx, srcHost, srcRepo, targetRepo, dg, storeInR2, pushHarbor)
		if err != nil {
			return blobsSynced, err
		}
		if synced {
			blobsSynced++
		}
	}

	if storeInR2 && o.Store != nil {
		refKey := "oci/v2/" + storage.CleanKey(targetRepo) + "/manifests/" + storage.CleanKey(ref)
		dgKey := "oci/v2/" + storage.CleanKey(targetRepo) + "/manifests/" + storage.CleanKey(digest)
		cc := "public, max-age=60, stale-while-revalidate=300"
		if strings.HasPrefix(ref, "sha256:") {
			cc = "public, max-age=31536000, immutable"
		}
		if err := o.Store.Put(ctx, refKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
			ContentType:  ct,
			CacheControl: cc,
		}); err != nil {
			return blobsSynced, err
		}
		if refKey != dgKey {
			if err := o.Store.Put(ctx, dgKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
				ContentType:  ct,
				CacheControl: "public, max-age=31536000, immutable",
			}); err != nil {
				return blobsSynced, err
			}
		}
	}

	if pushHarbor {
		if err := o.pushHarborManifest(ctx, targetRepo, ref, ct, data); err != nil {
			return blobsSynced, err
		}
	}

	return blobsSynced, nil
}

func (o *OCISyncer) syncBlob(ctx context.Context, srcHost, srcRepo, targetRepo, digest string, storeInR2, pushHarbor bool) (bool, error) {
	blobKey := "oci/blobs/" + storage.CleanKey(digest)
	needR2 := storeInR2 && o.Store != nil
	if needR2 {
		if _, err := o.Store.Head(ctx, blobKey); err == nil {
			needR2 = false
		}
	}
	needHarbor := pushHarbor && !o.harborBlobExists(ctx, targetRepo, digest)
	if !needR2 && !needHarbor {
		return false, nil
	}

	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", srcHost, srcRepo, digest)
	if strings.HasPrefix(srcHost, "http://") {
		blobURL = fmt.Sprintf("%s/v2/%s/blobs/%s", srcHost, srcRepo, digest)
	}
	resp, err := o.doRegistryRequest(ctx, http.MethodGet, blobURL, "*/*", nil, "", "", "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("fetch OCI blob %s: HTTP %d", blobURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if needR2 {
		if err := o.Store.Put(ctx, blobKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
			ContentType:  "application/octet-stream",
			CacheControl: "public, max-age=31536000, immutable",
		}); err != nil {
			return false, err
		}
	}
	if needHarbor {
		if err := o.pushHarborBlob(ctx, targetRepo, digest, data); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (o *OCISyncer) harborAuth() (user, pass string) {
	if o.OCIConfig.HarborUserEnv != "" {
		user = os.Getenv(o.OCIConfig.HarborUserEnv)
	}
	if o.OCIConfig.HarborPasswordEnv != "" {
		pass = os.Getenv(o.OCIConfig.HarborPasswordEnv)
	}
	return user, pass
}

func (o *OCISyncer) harborBase() string {
	return strings.TrimRight(o.OCIConfig.HarborURL, "/")
}

func (o *OCISyncer) harborBlobExists(ctx context.Context, targetRepo, digest string) bool {
	u := fmt.Sprintf("%s/v2/%s/blobs/%s", o.harborBase(), targetRepo, digest)
	user, pass := o.harborAuth()
	resp, err := o.doRegistryRequest(ctx, http.MethodHead, u, "*/*", nil, "", user, pass)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (o *OCISyncer) pushHarborBlob(ctx context.Context, targetRepo, digest string, data []byte) error {
	user, pass := o.harborAuth()
	initURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", o.harborBase(), targetRepo)
	resp, err := o.doRegistryRequest(ctx, http.MethodPost, initURL, "", nil, "", user, pass)
	if err != nil {
		return err
	}
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || loc == "" {
		return fmt.Errorf("harbor initiate blob upload %s: HTTP %d", initURL, resp.StatusCode)
	}
	if strings.HasPrefix(loc, "/") {
		loc = o.harborBase() + loc
	}
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	putURL := loc + sep + "digest=" + url.QueryEscape(digest)
	resp2, err := o.doRegistryRequest(ctx, http.MethodPut, putURL, "", data, "application/octet-stream", user, pass)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 300 {
		return fmt.Errorf("harbor upload blob %s: HTTP %d", digest, resp2.StatusCode)
	}
	return nil
}

func (o *OCISyncer) pushHarborManifest(ctx context.Context, targetRepo, ref, mediaType string, data []byte) error {
	user, pass := o.harborAuth()
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", o.harborBase(), targetRepo, ref)
	resp, err := o.doRegistryRequest(ctx, http.MethodPut, u, "", data, mediaType, user, pass)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("harbor push manifest %s:%s: HTTP %d: %s", targetRepo, ref, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

var ociWWWAuthRE = regexp.MustCompile(`([a-zA-Z0-9_]+)="([^"]*)"`)

func (o *OCISyncer) doRegistryRequest(ctx context.Context, method, targetURL, accept string, body []byte, contentType, user, pass string) (*http.Response, error) {
	makeReq := func(authHeader string) (*http.Request, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, targetURL, rdr)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		} else if user != "" && pass != "" {
			req.SetBasicAuth(user, pass)
		}
		return req, nil
	}

	req, err := makeReq("")
	if err != nil {
		return nil, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()

	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return nil, fmt.Errorf("registry %s unauthorized (%s)", targetURL, challenge)
	}
	params := map[string]string{}
	for _, m := range ociWWWAuthRE.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	tokenURL, err := url.Parse(params["realm"])
	if err != nil {
		return nil, err
	}
	q := tokenURL.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if params["scope"] != "" {
		q.Set("scope", params["scope"])
	}
	tokenURL.RawQuery = q.Encode()

	tReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return nil, err
	}
	if user != "" && pass != "" {
		tReq.SetBasicAuth(user, pass)
	}
	tResp, err := o.Client.Do(tReq)
	if err != nil {
		return nil, err
	}
	defer tResp.Body.Close()
	if tResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oci token request %s: HTTP %d", tokenURL.String(), tResp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tResp.Body).Decode(&tok); err != nil {
		return nil, err
	}
	bearer := tok.Token
	if bearer == "" {
		bearer = tok.AccessToken
	}
	req2, err := makeReq("Bearer " + bearer)
	if err != nil {
		return nil, err
	}
	return o.Client.Do(req2)
}
