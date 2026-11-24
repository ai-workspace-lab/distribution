//go:build !js

package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// FileSyncer synchronizes files from HTTPS URLs, rsync upstreams, or local directories into any storage.Backend.
type FileSyncer struct {
	Store  storage.Backend
	Client *http.Client
}

func NewFileSyncer(store storage.Backend, client *http.Client) *FileSyncer {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &FileSyncer{Store: store, Client: client}
}

// SyncSpec executes either an HTTPS or rsync file mirror specification.
func (f *FileSyncer) SyncSpec(ctx context.Context, spec config.FileUpstreamSpec) (int, error) {
	switch strings.ToLower(strings.TrimSpace(spec.Type)) {
	case "", "https", "http":
		if err := f.SyncHTTPSFile(ctx, spec.SourceURL, spec.DestPath, false); err != nil {
			return 0, err
		}
		return 1, nil
	case "rsync":
		return f.SyncRsync(ctx, spec.SourceURL, spec.DestPath, spec.Include)
	default:
		return 0, fmt.Errorf("unsupported file upstream type: %s", spec.Type)
	}
}

// SyncHTTPSFile downloads sourceURL and writes it to destKey if not already present (unless force=true).
func (f *FileSyncer) SyncHTTPSFile(ctx context.Context, sourceURL, destKey string, force bool) error {
	destKey = storage.CleanKey(destKey)
	if !force {
		if _, err := f.Store.Head(ctx, destKey); err == nil {
			return nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "svc-plus-distribution/2.0")
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", sourceURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	cc := "public, max-age=31536000, immutable"
	if strings.HasSuffix(destKey, ".json") || strings.HasSuffix(destKey, ".yaml") || strings.HasSuffix(destKey, ".html") {
		cc = "public, max-age=60, stale-while-revalidate=300"
	}
	return f.Store.Put(ctx, destKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
		ContentType:  ct,
		CacheControl: cc,
	})
}

// SyncRsync uses system rsync to pull an upstream rsync:// or SSH mirror into a staging directory
// and publishes all mirrored files into the configured storage.Backend.
func (f *FileSyncer) SyncRsync(ctx context.Context, sourceURL, destPrefix, includePattern string) (int, error) {
	rsyncBin, err := exec.LookPath("rsync")
	if err != nil {
		return 0, fmt.Errorf("rsync binary not found in PATH: %w", err)
	}
	stageDir, err := os.MkdirTemp("", "distd-rsync-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stageDir)

	args := []string{"-avz", "--no-perms", "--no-owner", "--no-group"}
	if includePattern != "" {
		args = append(args, "--include", includePattern)
	}
	args = append(args, sourceURL, stageDir+"/")
	cmd := exec.CommandContext(ctx, rsyncBin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("rsync %s failed: %w (%s)", sourceURL, err, strings.TrimSpace(string(out)))
	}
	return f.SyncDirectory(ctx, stageDir, destPrefix)
}

// SyncDirectory recursively uploads a local directory into storage.Backend under destPrefix.
func (f *FileSyncer) SyncDirectory(ctx context.Context, localDir, destPrefix string) (int, error) {
	count := 0
	destPrefix = storage.CleanKey(destPrefix)
	err := filepath.Walk(localDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if destPrefix != "" {
			key = destPrefix + "/" + key
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		cc := "public, max-age=31536000, immutable"
		if strings.HasSuffix(key, ".json") || strings.HasSuffix(key, ".yaml") || strings.HasSuffix(key, ".html") || strings.HasSuffix(key, "Release") || strings.HasSuffix(key, "InRelease") || strings.HasSuffix(key, "repomd.xml") {
			cc = "public, max-age=60, stale-while-revalidate=300"
		}
		if err := f.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
			CacheControl: cc,
		}); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// ReleaseMatrix mirrors the schema consumed by console.svc.plus/download and install.sh.
type ReleaseMatrix struct {
	SchemaVersion  int                           `json:"schemaVersion"`
	Product        string                        `json:"product"`
	DefaultChannel string                        `json:"defaultChannel"`
	Channels       map[string]ReleaseChannelItem `json:"channels"`
	History        []ReleaseChannelItem          `json:"history"`
}

type ReleaseChannelItem struct {
	Tag         string               `json:"tag"`
	Version     string               `json:"version"`
	Channel     string               `json:"channel"`
	PublishedAt string               `json:"publishedAt"`
	Platforms   []ReleasePlatformRow `json:"platforms"`
}

type ReleasePlatformRow struct {
	ID       string              `json:"id"`
	Packages []ReleasePackageRow `json:"packages"`
}

type ReleasePackageRow struct {
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Format string `json:"format"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
	URL    string `json:"url"`
	CNURL  string `json:"cnUrl"`
}

// BuildReleaseMatrixBytes builds release-matrix.json from local release artifacts.
func BuildReleaseMatrixBytes(product, tag, channel, dlBase, cnDLBase, distDir string) ([]byte, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return nil, err
	}
	dlBase = strings.TrimRight(dlBase, "/")
	cnDLBase = strings.TrimRight(cnDLBase, "/")
	byPlatform := map[string][]ReleasePackageRow{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, "SHA256SUMS") || name == "release-matrix.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(distDir, name))
		if err != nil {
			return nil, err
		}
		dig := crypto.ComputeDigestsBytes(raw)
		plat := detectPlatform(name)
		relPath := fmt.Sprintf("/releases/%s/%s/%s", product, tag, name)
		byPlatform[plat] = append(byPlatform[plat], ReleasePackageRow{
			Name:   name,
			Arch:   detectArch(name),
			Format: detectFormat(name),
			Size:   dig.Size,
			SHA256: dig.SHA256,
			Path:   relPath,
			URL:    dlBase + relPath,
			CNURL:  cnDLBase + relPath,
		})
	}
	var plats []ReleasePlatformRow
	for _, pid := range []string{"macos", "windows", "linux", "android", "ios"} {
		if pkgs, ok := byPlatform[pid]; ok && len(pkgs) > 0 {
			plats = append(plats, ReleasePlatformRow{ID: pid, Packages: pkgs})
		}
	}
	item := ReleaseChannelItem{
		Tag:         tag,
		Version:     strings.TrimPrefix(tag, "v"),
		Channel:     channel,
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Platforms:   plats,
	}
	matrix := ReleaseMatrix{
		SchemaVersion:  1,
		Product:        product,
		DefaultChannel: channel,
		Channels:       map[string]ReleaseChannelItem{channel: item},
		History:        []ReleaseChannelItem{item},
	}
	b, err := json.MarshalIndent(matrix, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func detectPlatform(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.Contains(low, "macos") || strings.Contains(low, "darwin") || strings.HasSuffix(low, ".dmg"):
		return "macos"
	case strings.Contains(low, "windows") || strings.HasSuffix(low, ".exe") || strings.HasSuffix(low, ".msi"):
		return "windows"
	case strings.Contains(low, "android") || strings.HasSuffix(low, ".apk") || strings.HasSuffix(low, ".aab"):
		return "android"
	case strings.Contains(low, "ios") || strings.HasSuffix(low, ".ipa"):
		return "ios"
	default:
		return "linux"
	}
}

func detectArch(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.Contains(low, "arm64") || strings.Contains(low, "aarch64"):
		return "arm64"
	case strings.Contains(low, "amd64") || strings.Contains(low, "x86_64") || strings.Contains(low, "x64"):
		return "amd64"
	default:
		return "universal"
	}
}

func detectFormat(name string) string {
	low := strings.ToLower(name)
	for _, ext := range []string{".tar.gz", ".dmg", ".deb", ".rpm", ".msi", ".exe", ".zip", ".apk", ".aab", ".ipa", ".appimage"} {
		if strings.HasSuffix(low, ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}
	return "binary"
}
