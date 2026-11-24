//go:build !js

package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/server"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// NPMSyncer mirrors npm package packuments and tarballs from an upstream registry into storage.Backend.
type NPMSyncer struct {
	Store            storage.Backend
	UpstreamRegistry string
	MirrorNPMBase    string
	Client           *http.Client
}

func NewNPMSyncer(store storage.Backend, upstreamRegistry, mirrorNPMBase string, client *http.Client) *NPMSyncer {
	if upstreamRegistry == "" {
		upstreamRegistry = "https://registry.npmjs.org"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &NPMSyncer{
		Store:            store,
		UpstreamRegistry: strings.TrimRight(upstreamRegistry, "/"),
		MirrorNPMBase:    strings.TrimRight(mirrorNPMBase, "/"),
		Client:           client,
	}
}

// SyncPackage mirrors the packument and tarballs (latest tag by default, or all versions if allVersions=true).
func (n *NPMSyncer) SyncPackage(ctx context.Context, pkgName string, allVersions bool) (int, error) {
	escaped := strings.ReplaceAll(url.PathEscape(pkgName), "%2F", "/")
	metaURL := n.UpstreamRegistry + "/" + escaped
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("npm fetch %s: HTTP %d", metaURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("decode npm packument %s: %w", pkgName, err)
	}

	targetVersions := map[string]bool{}
	if allVersions {
		for v := range doc.Versions {
			targetVersions[v] = true
		}
	} else {
		for _, v := range doc.DistTags {
			targetVersions[v] = true
		}
	}

	synced := 0
	for ver := range targetVersions {
		vinfo, ok := doc.Versions[ver]
		if !ok || vinfo.Dist.Tarball == "" {
			continue
		}
		tbURL, err := url.Parse(vinfo.Dist.Tarball)
		if err != nil {
			continue
		}
		storeKey := "npm/tarballs/" + storage.CleanKey(tbURL.Path)
		if _, err := n.Store.Head(ctx, storeKey); err == nil {
			continue
		}
		tbReq, err := http.NewRequestWithContext(ctx, http.MethodGet, vinfo.Dist.Tarball, nil)
		if err != nil {
			return synced, err
		}
		tbResp, err := n.Client.Do(tbReq)
		if err != nil {
			return synced, err
		}
		if tbResp.StatusCode != http.StatusOK {
			_ = tbResp.Body.Close()
			return synced, fmt.Errorf("npm tarball %s: HTTP %d", vinfo.Dist.Tarball, tbResp.StatusCode)
		}
		tbData, err := io.ReadAll(tbResp.Body)
		_ = tbResp.Body.Close()
		if err != nil {
			return synced, err
		}
		if err := n.Store.Put(ctx, storeKey, bytes.NewReader(tbData), int64(len(tbData)), storage.PutOptions{
			ContentType:  "application/octet-stream",
			CacheControl: "public, max-age=31536000, immutable",
		}); err != nil {
			return synced, err
		}
		synced++
	}

	rewritten := server.RewriteNPMPackumentTarballs(raw, n.MirrorNPMBase, n.UpstreamRegistry)
	metaKey := "npm/metadata/" + storage.CleanKey(pkgName) + "/index.json"
	if err := n.Store.Put(ctx, metaKey, bytes.NewReader(rewritten), int64(len(rewritten)), storage.PutOptions{
		ContentType:  "application/json",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	}); err != nil {
		return synced, err
	}
	return synced, nil
}
