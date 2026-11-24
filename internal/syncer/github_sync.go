//go:build !js

package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// GitHubSyncer mirrors GitHub repositories (static Git HTTP), GitHub Release assets, and GHCR images.
type GitHubSyncer struct {
	Store      storage.Backend
	Files      *FileSyncer
	OCI        *OCISyncer
	APIBaseURL string
	GitHubURL  string
	Token      string
	Client     *http.Client
}

func NewGitHubSyncer(store storage.Backend, files *FileSyncer, oci *OCISyncer, cfg config.GitHubUpstreamConfig, githubBase string) *GitHubSyncer {
	apiBase := cfg.APIBaseURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	if githubBase == "" {
		githubBase = "https://github.com"
	}
	tok := ""
	if cfg.TokenEnv != "" {
		tok = os.Getenv(cfg.TokenEnv)
	}
	return &GitHubSyncer{
		Store:      store,
		Files:      files,
		OCI:        oci,
		APIBaseURL: strings.TrimRight(apiBase, "/"),
		GitHubURL:  strings.TrimRight(githubBase, "/"),
		Token:      tok,
		Client:     &http.Client{Timeout: 2 * time.Minute},
	}
}

// SyncRepo executes Git mirror, Release asset mirror, and GHCR image mirror for a single GitHubRepoMirror spec.
func (g *GitHubSyncer) SyncRepo(ctx context.Context, spec config.GitHubRepoMirror) (int, error) {
	total := 0
	if spec.MirrorGit {
		n, err := g.MirrorBareGit(ctx, spec.Repo, "git/"+spec.Repo+".git")
		if err != nil {
			return total, err
		}
		total += n
	}
	if spec.MirrorRelease {
		n, err := g.MirrorLatestRelease(ctx, spec.Repo, spec.ProductID)
		if err != nil {
			return total, err
		}
		total += n
	}
	if g.OCI != nil {
		for _, img := range spec.GHCRImages {
			imgName, tag := splitImageTag(img)
			n, err := g.OCI.SyncImage(ctx, config.OCIImageMirror{
				Source:    imgName,
				Target:    strings.TrimPrefix(imgName, "ghcr.io/"),
				Tags:      []string{tag},
				StoreInR2: true,
			}, tag)
			if err != nil {
				return total, err
			}
			total += n
		}
	}
	return total, nil
}

// MirrorBareGit clones a bare Git mirror, runs `git update-server-info`, and uploads it for CDN static HTTP cloning.
func (g *GitHubSyncer) MirrorBareGit(ctx context.Context, repoSlug, destPrefix string) (int, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return 0, fmt.Errorf("git binary not found in PATH: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "distd-git-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := g.GitHubURL + "/" + strings.Trim(repoSlug, "/") + ".git"
	cmd := exec.CommandContext(ctx, gitBin, "clone", "--bare", "--quiet", cloneURL, tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("git clone --bare %s: %w (%s)", cloneURL, err, strings.TrimSpace(string(out)))
	}
	infoCmd := exec.CommandContext(ctx, gitBin, "--git-dir", tmpDir, "update-server-info")
	if out, err := infoCmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("git update-server-info %s: %w (%s)", repoSlug, err, strings.TrimSpace(string(out)))
	}
	return g.Files.SyncDirectory(ctx, tmpDir, destPrefix)
}

// MirrorLatestRelease downloads assets of the latest GitHub Release into storage.Backend.
func (g *GitHubSyncer) MirrorLatestRelease(ctx context.Context, repoSlug, productID string) (int, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/latest", g.APIBaseURL, strings.Trim(repoSlug, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "svc-plus-distribution/2.0")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github latest release %s: HTTP %d", repoSlug, resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return 0, err
	}

	count := 0
	for _, asset := range rel.Assets {
		ghKey := fmt.Sprintf("github/%s/releases/download/%s/%s", repoSlug, rel.TagName, asset.Name)
		if err := g.Files.SyncHTTPSFile(ctx, asset.BrowserDownloadURL, ghKey, false); err != nil {
			return count, err
		}
		if productID != "" {
			prodKey := fmt.Sprintf("releases/%s/%s/%s", productID, rel.TagName, asset.Name)
			if err := g.Files.SyncHTTPSFile(ctx, asset.BrowserDownloadURL, prodKey, false); err != nil {
				return count, err
			}
		}
		count++
	}
	return count, nil
}

func splitImageTag(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, "latest"
}
