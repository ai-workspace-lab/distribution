//go:build !js

package syncer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
	"gopkg.in/yaml.v3"
)

// HelmIndexFile represents a classic Helm repository index.yaml.
type HelmIndexFile struct {
	APIVersion string                        `yaml:"apiVersion"`
	Generated  string                        `yaml:"generated"`
	Entries    map[string][]HelmChartVersion `yaml:"entries"`
}

type HelmChartVersion struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	AppVersion  string   `yaml:"appVersion,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Digest      string   `yaml:"digest,omitempty"`
	Created     string   `yaml:"created,omitempty"`
	URLs        []string `yaml:"urls"`
}

// HelmSyncer aggregates classic HTTP Helm repos and OCI Helm charts into a unified dual-mode repo.
type HelmSyncer struct {
	Store         storage.Backend
	OCISyncer     *OCISyncer
	MirrorHelmURL string
	Client        *http.Client
}

func NewHelmSyncer(store storage.Backend, oci *OCISyncer, mirrorHelmURL string, client *http.Client) *HelmSyncer {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &HelmSyncer{
		Store:         store,
		OCISyncer:     oci,
		MirrorHelmURL: strings.TrimRight(mirrorHelmURL, "/"),
		Client:        client,
	}
}

// SyncAll synchronizes classic HTTP Helm repos (merging into helm/index.yaml) and OCI Helm charts.
func (h *HelmSyncer) SyncAll(ctx context.Context, cfg config.HelmUpstreamConfig) (int, error) {
	merged := &HelmIndexFile{
		APIVersion: "v1",
		Generated:  time.Now().UTC().Format(time.RFC3339),
		Entries:    map[string][]HelmChartVersion{},
	}
	synced := 0

	for _, repo := range cfg.Repos {
		n, err := h.syncClassicRepo(ctx, repo, merged)
		if err != nil {
			return synced, err
		}
		synced += n
	}

	if h.OCISyncer != nil {
		for _, chart := range cfg.OCICharts {
			tags := chart.Tags
			if len(tags) == 0 {
				tags = []string{"latest"}
			}
			for _, tag := range tags {
				n, err := h.OCISyncer.SyncImage(ctx, chart, tag)
				if err != nil {
					return synced, err
				}
				synced += n
			}
		}
	}

	// Sort versions deterministically per chart
	for name, vers := range merged.Entries {
		sort.Slice(vers, func(i, j int) bool { return vers[i].Version > vers[j].Version })
		merged.Entries[name] = vers
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return synced, err
	}
	if err := h.Store.Put(ctx, "helm/index.yaml", bytes.NewReader(out), int64(len(out)), storage.PutOptions{
		ContentType:  "application/yaml",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	}); err != nil {
		return synced, err
	}
	return synced, nil
}

func (h *HelmSyncer) syncClassicRepo(ctx context.Context, repo config.HelmRepoMirror, merged *HelmIndexFile) (int, error) {
	indexURL := strings.TrimRight(repo.URL, "/") + "/index.yaml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch helm index %s: HTTP %d", indexURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var idx HelmIndexFile
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return 0, fmt.Errorf("parse helm index %s: %w", indexURL, err)
	}

	allow := map[string]bool{}
	for _, c := range repo.Charts {
		allow[c] = true
	}

	synced := 0
	for chartName, versions := range idx.Entries {
		if len(allow) > 0 && !allow[chartName] {
			continue
		}
		for _, v := range versions {
			if len(v.URLs) == 0 {
				continue
			}
			srcChartURL := resolveRelativeURL(repo.URL, v.URLs[0])
			fileName := path.Base(srcChartURL)
			if !strings.HasSuffix(fileName, ".tgz") {
				fileName = fmt.Sprintf("%s-%s.tgz", chartName, v.Version)
			}
			storeKey := "helm/charts/" + storage.CleanKey(fileName)
			if _, err := h.Store.Head(ctx, storeKey); err != nil {
				cReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcChartURL, nil)
				if err != nil {
					return synced, err
				}
				cResp, err := h.Client.Do(cReq)
				if err != nil {
					return synced, err
				}
				if cResp.StatusCode != http.StatusOK {
					_ = cResp.Body.Close()
					return synced, fmt.Errorf("fetch chart %s: HTTP %d", srcChartURL, cResp.StatusCode)
				}
				cData, err := io.ReadAll(cResp.Body)
				_ = cResp.Body.Close()
				if err != nil {
					return synced, err
				}
				if v.Digest == "" {
					v.Digest = crypto.ComputeDigestsBytes(cData).SHA256
				}
				if err := h.Store.Put(ctx, storeKey, bytes.NewReader(cData), int64(len(cData)), storage.PutOptions{
					ContentType:  "application/gzip",
					CacheControl: "public, max-age=31536000, immutable",
				}); err != nil {
					return synced, err
				}
				synced++
			}
			v.URLs = []string{h.MirrorHelmURL + "/charts/" + fileName}
			merged.Entries[chartName] = append(merged.Entries[chartName], v)
		}
	}
	return synced, nil
}

func resolveRelativeURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	bu, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(ref, "/")
	}
	ru, err := url.Parse(ref)
	if err != nil {
		return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(ref, "/")
	}
	return bu.ResolveReference(ru).String()
}
